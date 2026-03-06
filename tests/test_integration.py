"""Integration test: mock Linear API + mock Codex process → full dispatch→run→complete cycle."""

from __future__ import annotations

import asyncio
import json
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from symphony.agent.base import AgentConfig, AgentEvent, TurnResult
from symphony.agent.protocol import (
    JsonRpcNotification,
    JsonRpcRequest,
    JsonRpcResponse,
    Methods,
    format_message,
)
from symphony.config import SymphonyConfig
from symphony.models import Issue, OrchestratorState, RunStatus, Workspace
from symphony.orchestrator import Orchestrator
from symphony.tracker.linear import LinearClient
from symphony.workspace import WorkspaceManager


# -- Fixtures ----------------------------------------------------------------


WORKFLOW_CONTENT = """\
---
tracker:
  kind: linear
  api_key: test_key
  project_slug: test-proj
  active_states:
    - In Progress
  terminal_states:
    - Done
polling:
  interval_ms: 100
agent:
  max_concurrent_agents: 1
  max_turns: 1
codex:
  command: echo noop
  read_timeout_ms: 5000
  turn_timeout_ms: 5000
  stall_timeout_ms: 60000
workspace:
  root: {workspace_root}
---
Fix: {{ issue.identifier }} - {{ issue.title }}
"""


def _make_issue(
    id: str = "issue-1",
    identifier: str = "TEST-1",
    state: str = "In Progress",
) -> Issue:
    return Issue(
        id=id,
        identifier=identifier,
        title="Test issue",
        description="Test description",
        priority=1,
        state=state,
        branch_name=None,
        url=None,
        created_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
    )


@pytest.fixture
def workflow_path(tmp_path):
    ws_root = tmp_path / "workspaces"
    ws_root.mkdir()
    p = tmp_path / "WORKFLOW.md"
    p.write_text(WORKFLOW_CONTENT.format(workspace_root=str(ws_root)))
    return p


# -- Mock Tracker Tests -------------------------------------------------------


class TestMockTrackerIntegration:
    """Test orchestrator with mocked tracker and agent runner."""

    @pytest.mark.asyncio
    async def test_full_dispatch_run_complete_cycle(self, workflow_path, tmp_path):
        """Verify: fetch issue → create workspace → run agent → mark complete."""
        issue = _make_issue()

        orch = Orchestrator(workflow_path=workflow_path)
        orch._reload_workflow()

        # Mock tracker
        mock_tracker = AsyncMock()
        mock_tracker.fetch_candidate_issues = AsyncMock(return_value=[issue])
        mock_tracker.fetch_issue_states_by_ids = AsyncMock(return_value=[issue])
        mock_tracker.fetch_issues_by_states = AsyncMock(return_value=[])
        mock_tracker.close = AsyncMock()
        orch._tracker = mock_tracker

        # Setup workspace manager
        ws_root = tmp_path / "workspaces"
        ws_root.mkdir(exist_ok=True)
        orch._workspace_mgr = WorkspaceManager(root=str(ws_root))
        await orch._workspace_mgr.ensure_root()

        # Mock _create_runner to return a mock agent
        mock_runner = AsyncMock()
        mock_runner.start_session = AsyncMock(return_value="thread-1")
        mock_runner.run_turn = AsyncMock(return_value=TurnResult(success=True))
        mock_runner.stop_session = AsyncMock()
        mock_runner.pid = "1234"
        mock_runner.session_id = "session-1"

        with patch.object(orch, "_create_runner", return_value=mock_runner):
            # Run a single tick
            await orch._tick()

            # Wait for worker to finish
            if orch._worker_tasks:
                await asyncio.gather(*orch._worker_tasks.values(), return_exceptions=True)

        # Verify agent was called
        mock_runner.start_session.assert_called_once()
        mock_runner.run_turn.assert_called_once()
        mock_runner.stop_session.assert_called_once()

        # Verify issue completed (no double-append regression)
        assert orch._state.completed_count == 1

    @pytest.mark.asyncio
    async def test_respects_concurrency_limit(self, workflow_path, tmp_path):
        """With max_concurrent_agents=1, only one issue should be dispatched."""
        issues = [_make_issue(id=f"issue-{i}", identifier=f"TEST-{i}") for i in range(3)]

        orch = Orchestrator(workflow_path=workflow_path)
        orch._reload_workflow()
        orch._state.max_concurrent_agents = 1

        mock_tracker = AsyncMock()
        mock_tracker.fetch_candidate_issues = AsyncMock(return_value=issues)
        mock_tracker.fetch_issue_states_by_ids = AsyncMock(return_value=issues)
        mock_tracker.fetch_issues_by_states = AsyncMock(return_value=[])
        orch._tracker = mock_tracker

        ws_root = tmp_path / "workspaces"
        ws_root.mkdir(exist_ok=True)
        orch._workspace_mgr = WorkspaceManager(root=str(ws_root))

        # Use a slow runner so we can check concurrency
        started = asyncio.Event()

        async def slow_start(*args, **kwargs):
            started.set()
            await asyncio.sleep(10)
            return "thread-1"

        mock_runner = AsyncMock()
        mock_runner.start_session = slow_start
        mock_runner.pid = "1234"
        mock_runner.session_id = "session-1"

        with patch.object(orch, "_create_runner", return_value=mock_runner):
            await orch._tick()

        # Should only have dispatched 1
        assert len(orch._state.running) == 1

        # Cancel running tasks
        for task in orch._worker_tasks.values():
            task.cancel()
        await asyncio.gather(*orch._worker_tasks.values(), return_exceptions=True)

    @pytest.mark.asyncio
    async def test_skips_blocked_todo_issues(self, workflow_path, tmp_path):
        """Todo issues blocked by non-terminal issues should be skipped."""
        from symphony.models import BlockerRef

        blocker = BlockerRef(id="blocker-1", identifier="TEST-0", state="In Progress")
        blocked_issue = Issue(
            id="issue-1", identifier="TEST-1", title="Test issue",
            description=None, priority=1, state="Todo",
            branch_name=None, url=None,
            created_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
            blocked_by=[blocker],
        )

        orch = Orchestrator(workflow_path=workflow_path)
        orch._reload_workflow()

        mock_tracker = AsyncMock()
        mock_tracker.fetch_candidate_issues = AsyncMock(return_value=[blocked_issue])
        mock_tracker.fetch_issue_states_by_ids = AsyncMock(return_value=[])
        mock_tracker.fetch_issues_by_states = AsyncMock(return_value=[])
        orch._tracker = mock_tracker

        ws_root = tmp_path / "workspaces"
        ws_root.mkdir(exist_ok=True)
        orch._workspace_mgr = WorkspaceManager(root=str(ws_root))

        await orch._tick()

        assert len(orch._state.running) == 0

    @pytest.mark.asyncio
    async def test_reconcile_terminal_cancels_agent(self, workflow_path, tmp_path):
        """If an issue goes terminal during execution, reconcile should cancel it."""
        from symphony.models import RunAttempt

        orch = Orchestrator(workflow_path=workflow_path)
        orch._reload_workflow()

        # Simulate a running issue
        attempt = RunAttempt(
            issue_id="issue-1",
            identifier="TEST-1",
            attempt=1,
            started_at=datetime.now(timezone.utc),
            workspace_path=str(tmp_path / "workspaces" / "TEST-1"),
        )
        orch._state.running["issue-1"] = attempt

        # Mock a task
        async def noop():
            await asyncio.sleep(100)

        task = asyncio.create_task(noop())
        orch._worker_tasks["issue-1"] = task

        # Tracker returns the issue as Done (terminal)
        done_issue = _make_issue(state="Done")
        mock_tracker = AsyncMock()
        mock_tracker.fetch_issue_states_by_ids = AsyncMock(return_value=[done_issue])
        orch._tracker = mock_tracker

        ws_root = tmp_path / "workspaces"
        ws_root.mkdir(exist_ok=True)
        (ws_root / "TEST-1").mkdir(exist_ok=True)
        orch._workspace_mgr = WorkspaceManager(root=str(ws_root))

        await orch._reconcile()

        # Task should be cancelled
        assert task.cancelled()
        await asyncio.gather(task, return_exceptions=True)

    @pytest.mark.asyncio
    async def test_agent_failure_does_not_crash_orchestrator(self, workflow_path, tmp_path):
        """If agent raises, orchestrator should handle it gracefully."""
        issue = _make_issue()

        orch = Orchestrator(workflow_path=workflow_path)
        orch._reload_workflow()

        mock_tracker = AsyncMock()
        mock_tracker.fetch_candidate_issues = AsyncMock(return_value=[issue])
        mock_tracker.fetch_issue_states_by_ids = AsyncMock(return_value=[issue])
        mock_tracker.fetch_issues_by_states = AsyncMock(return_value=[])
        orch._tracker = mock_tracker

        ws_root = tmp_path / "workspaces"
        ws_root.mkdir(exist_ok=True)
        orch._workspace_mgr = WorkspaceManager(root=str(ws_root))

        mock_runner = AsyncMock()
        mock_runner.start_session = AsyncMock(side_effect=RuntimeError("Agent crashed"))
        mock_runner.stop_session = AsyncMock()
        mock_runner.pid = None
        mock_runner.session_id = None

        with patch.object(orch, "_create_runner", return_value=mock_runner):
            await orch._tick()
            # Wait for worker
            if orch._worker_tasks:
                await asyncio.gather(*orch._worker_tasks.values(), return_exceptions=True)

        # Issue should NOT be counted as completed (it failed)
        assert orch._state.completed_count == 0


# -- Mock Linear API Tests ---------------------------------------------------


class TestLinearClientMocked:
    """Test LinearClient with mocked httpx responses."""

    @pytest.mark.asyncio
    async def test_fetch_candidate_issues(self):
        import httpx

        response_data = {
            "data": {
                "issues": {
                    "pageInfo": {"hasNextPage": False, "endCursor": None},
                    "nodes": [
                        {
                            "id": "id-1",
                            "identifier": "ENG-1",
                            "title": "Fix bug",
                            "description": "A bug",
                            "priority": 2,
                            "state": {"name": "In Progress"},
                            "branchName": "eng-1",
                            "url": "https://linear.app/ENG-1",
                            "labels": {"nodes": [{"name": "Bug"}, {"name": "P1"}]},
                            "relations": {"nodes": []},
                            "createdAt": "2025-01-01T00:00:00Z",
                            "updatedAt": "2025-01-02T00:00:00Z",
                        }
                    ],
                }
            }
        }

        mock_response = httpx.Response(200, json=response_data)

        client = LinearClient(
            api_key="test-key",
            endpoint="https://api.linear.app/graphql",
            project_slug="test",
            active_states=["In Progress"],
            terminal_states=["Done"],
        )

        with patch.object(client._client, "post", return_value=mock_response):
            issues = await client.fetch_candidate_issues()

        assert len(issues) == 1
        issue = issues[0]
        assert issue.id == "id-1"
        assert issue.identifier == "ENG-1"
        assert issue.title == "Fix bug"
        assert issue.state == "In Progress"
        assert issue.labels == ["bug", "p1"]  # lowercase
        assert issue.branch_name == "eng-1"
        assert issue.created_at is not None

        await client.close()

    @pytest.mark.asyncio
    async def test_fetch_with_pagination(self):
        import httpx

        page1 = {
            "data": {
                "issues": {
                    "pageInfo": {"hasNextPage": True, "endCursor": "cursor-1"},
                    "nodes": [
                        {
                            "id": "id-1", "identifier": "ENG-1", "title": "A",
                            "description": None, "priority": 1,
                            "state": {"name": "Todo"}, "branchName": None,
                            "url": None, "labels": {"nodes": []},
                            "relations": {"nodes": []},
                            "createdAt": "2025-01-01T00:00:00Z", "updatedAt": None,
                        }
                    ],
                }
            }
        }
        page2 = {
            "data": {
                "issues": {
                    "pageInfo": {"hasNextPage": False, "endCursor": None},
                    "nodes": [
                        {
                            "id": "id-2", "identifier": "ENG-2", "title": "B",
                            "description": None, "priority": 2,
                            "state": {"name": "Todo"}, "branchName": None,
                            "url": None, "labels": {"nodes": []},
                            "relations": {"nodes": []},
                            "createdAt": "2025-01-02T00:00:00Z", "updatedAt": None,
                        }
                    ],
                }
            }
        }

        responses = [httpx.Response(200, json=page1), httpx.Response(200, json=page2)]

        client = LinearClient(
            api_key="key", endpoint="https://api.linear.app/graphql",
            project_slug="proj", active_states=["Todo"], terminal_states=["Done"],
        )

        with patch.object(client._client, "post", side_effect=responses):
            issues = await client.fetch_candidate_issues()

        assert len(issues) == 2
        assert issues[0].identifier == "ENG-1"
        assert issues[1].identifier == "ENG-2"

        await client.close()

    @pytest.mark.asyncio
    async def test_fetch_issue_states_by_ids(self):
        import httpx

        response_data = {
            "data": {
                "issues": {
                    "nodes": [
                        {"id": "id-1", "identifier": "ENG-1", "state": {"name": "Done"}},
                    ]
                }
            }
        }

        client = LinearClient(
            api_key="key", endpoint="https://api.linear.app/graphql",
            project_slug="proj", active_states=["Todo"], terminal_states=["Done"],
        )

        with patch.object(
            client._client, "post",
            return_value=httpx.Response(200, json=response_data),
        ):
            issues = await client.fetch_issue_states_by_ids(["id-1"])

        assert len(issues) == 1
        assert issues[0].state == "Done"

        await client.close()

    @pytest.mark.asyncio
    async def test_graphql_error(self):
        import httpx
        from symphony.tracker.linear import LinearError

        response_data = {"errors": [{"message": "Invalid query"}]}

        client = LinearClient(
            api_key="key", endpoint="https://api.linear.app/graphql",
            project_slug="proj", active_states=[], terminal_states=[],
        )

        with patch.object(
            client._client, "post",
            return_value=httpx.Response(200, json=response_data),
        ):
            with pytest.raises(LinearError) as exc_info:
                await client.fetch_candidate_issues()
            assert exc_info.value.code == "linear_graphql_errors"

        await client.close()

    @pytest.mark.asyncio
    async def test_missing_api_key(self):
        from symphony.tracker.linear import LinearError

        with pytest.raises(LinearError) as exc_info:
            LinearClient(
                api_key="", endpoint="https://api.linear.app/graphql",
                project_slug="proj", active_states=[], terminal_states=[],
            )
        assert exc_info.value.code == "missing_tracker_api_key"

    @pytest.mark.asyncio
    async def test_blocked_by_normalization(self):
        import httpx

        response_data = {
            "data": {
                "issues": {
                    "pageInfo": {"hasNextPage": False, "endCursor": None},
                    "nodes": [
                        {
                            "id": "id-1", "identifier": "ENG-1", "title": "A",
                            "description": None, "priority": 1,
                            "state": {"name": "Todo"}, "branchName": None,
                            "url": None, "labels": {"nodes": []},
                            "relations": {
                                "nodes": [
                                    {
                                        "type": "blocks",
                                        "relatedIssue": {
                                            "id": "blocker-1",
                                            "identifier": "ENG-0",
                                            "state": {"name": "In Progress"},
                                        }
                                    }
                                ]
                            },
                            "createdAt": "2025-01-01T00:00:00Z",
                            "updatedAt": None,
                        }
                    ],
                }
            }
        }

        client = LinearClient(
            api_key="key", endpoint="https://api.linear.app/graphql",
            project_slug="proj", active_states=["Todo"], terminal_states=["Done"],
        )

        with patch.object(
            client._client, "post",
            return_value=httpx.Response(200, json=response_data),
        ):
            issues = await client.fetch_candidate_issues()

        assert len(issues[0].blocked_by) == 1
        assert issues[0].blocked_by[0].identifier == "ENG-0"
        assert issues[0].blocked_by[0].state == "In Progress"

        await client.close()
