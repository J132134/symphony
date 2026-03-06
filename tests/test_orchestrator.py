from __future__ import annotations

from datetime import datetime, timezone

import pytest

import asyncio

from symphony.models import BlockerRef, Issue, OrchestratorState, RunAttempt, RunStatus
from symphony.orchestrator import Orchestrator
from tests.conftest import make_issue as _make_issue


_issue = _make_issue


class TestSortCandidates:
    def test_priority_asc(self):
        issues = [
            _issue(id="3", identifier="C", priority=3),
            _issue(id="1", identifier="A", priority=1),
            _issue(id="2", identifier="B", priority=2),
        ]
        result = Orchestrator._sort_candidates(issues)
        assert [i.identifier for i in result] == ["A", "B", "C"]

    def test_null_priority_last(self):
        issues = [
            _issue(id="2", identifier="B", priority=None),
            _issue(id="1", identifier="A", priority=1),
        ]
        result = Orchestrator._sort_candidates(issues)
        assert [i.identifier for i in result] == ["A", "B"]

    def test_same_priority_by_created_at(self):
        issues = [
            _issue(id="2", identifier="B", priority=1,
                   created_at=datetime(2025, 3, 1, tzinfo=timezone.utc)),
            _issue(id="1", identifier="A", priority=1,
                   created_at=datetime(2025, 1, 1, tzinfo=timezone.utc)),
        ]
        result = Orchestrator._sort_candidates(issues)
        assert [i.identifier for i in result] == ["A", "B"]

    def test_same_priority_and_date_by_identifier(self):
        dt = datetime(2025, 1, 1, tzinfo=timezone.utc)
        issues = [
            _issue(id="2", identifier="ENG-2", priority=1, created_at=dt),
            _issue(id="1", identifier="ENG-1", priority=1, created_at=dt),
        ]
        result = Orchestrator._sort_candidates(issues)
        assert [i.identifier for i in result] == ["ENG-1", "ENG-2"]

    def test_empty_list(self):
        assert Orchestrator._sort_candidates([]) == []


class TestRetryBackoff:
    def _make_orch(self, tmp_path):
        wf = tmp_path / "WORKFLOW.md"
        wf.write_text(
            "---\ntracker:\n  kind: linear\n  api_key: t\n  project_slug: p\n"
            "agent:\n  max_retry_backoff_ms: 320000\ncodex:\n  command: codex\n---\nX\n"
        )
        orch = Orchestrator(workflow_path=wf)
        orch._reload_workflow()
        return orch

    @pytest.mark.asyncio
    async def test_normal_retry_queues_entry(self, tmp_path):
        orch = self._make_orch(tmp_path)
        loop = asyncio.get_running_loop()
        before_ms = loop.time() * 1000
        orch._schedule_retry("id-1", abnormal=False, attempt=1, identifier="ENG-1")
        entry = orch._state.retry_queue["id-1"]
        assert entry.error is None
        assert abs(entry.due_at_ms - (before_ms + 1000)) < 200
        assert entry.timer_handle is not None
        entry.timer_handle.cancel()

    @pytest.mark.asyncio
    async def test_abnormal_retry_backoff(self, tmp_path):
        orch = self._make_orch(tmp_path)
        assert orch._config is not None
        max_b = orch._config.max_retry_backoff_ms
        loop = asyncio.get_running_loop()
        cases = [(1, 10000), (2, 20000), (3, 40000), (5, 160000), (6, 320000), (7, 320000)]
        for attempt, expected_delay in cases:
            before_ms = loop.time() * 1000
            orch._schedule_retry("id-1", error="e", abnormal=True, attempt=attempt, identifier="ENG-1")
            entry = orch._state.retry_queue.pop("id-1")
            orch._state.claimed.discard("id-1")
            actual_delay = min(10000 * (2 ** (attempt - 1)), max_b)
            assert actual_delay == expected_delay, f"attempt={attempt}"
            assert abs(entry.due_at_ms - (before_ms + actual_delay)) < 200
            assert entry.timer_handle is not None
            entry.timer_handle.cancel()


class TestCandidateSelection:
    """Test _can_dispatch logic via orchestrator state."""

    def _make_orchestrator_with_config(self, tmp_path):
        workflow = tmp_path / "WORKFLOW.md"
        workflow.write_text(
            "---\n"
            "tracker:\n"
            "  kind: linear\n"
            "  api_key: test\n"
            "  project_slug: proj\n"
            "  active_states: [In Progress, Todo]\n"
            "  terminal_states: [Done]\n"
            "codex:\n"
            "  command: codex\n"
            "---\nPrompt\n"
        )
        orch = Orchestrator(workflow_path=workflow)
        orch._reload_workflow()
        orch._state.max_concurrent_agents = 2
        return orch

    def test_eligible_issue(self, tmp_path):
        orch = self._make_orchestrator_with_config(tmp_path)
        issue = _issue(state="In Progress")
        assert orch._can_dispatch(issue) is True

    def test_already_running(self, tmp_path):
        orch = self._make_orchestrator_with_config(tmp_path)
        orch._state.running["id-1"] = RunAttempt(
            issue_id="id-1", identifier="ENG-1", attempt=1
        )
        issue = _issue(state="In Progress")
        assert orch._can_dispatch(issue) is False

    def test_already_claimed(self, tmp_path):
        orch = self._make_orchestrator_with_config(tmp_path)
        orch._state.claimed.add("id-1")
        issue = _issue(state="In Progress")
        assert orch._can_dispatch(issue) is False

    def test_terminal_state_rejected(self, tmp_path):
        orch = self._make_orchestrator_with_config(tmp_path)
        issue = _issue(state="Done")
        assert orch._can_dispatch(issue) is False

    def test_inactive_state_rejected(self, tmp_path):
        orch = self._make_orchestrator_with_config(tmp_path)
        issue = _issue(state="Backlog")
        assert orch._can_dispatch(issue) is False

    def test_global_concurrency_limit(self, tmp_path):
        orch = self._make_orchestrator_with_config(tmp_path)
        orch._state.max_concurrent_agents = 1
        orch._state.running["other-id"] = RunAttempt(
            issue_id="other-id", identifier="ENG-0", attempt=1
        )
        issue = _issue(state="In Progress")
        assert orch._can_dispatch(issue) is False

    def test_blocked_issue_rejected(self, tmp_path):
        orch = self._make_orchestrator_with_config(tmp_path)
        blockers = [BlockerRef(id="b1", identifier="ENG-0", state="In Progress")]
        issue = _issue(state="Todo", blocked_by=blockers)
        assert orch._can_dispatch(issue) is False

    def test_blocked_by_terminal_allowed(self, tmp_path):
        orch = self._make_orchestrator_with_config(tmp_path)
        blockers = [BlockerRef(id="b1", identifier="ENG-0", state="Done")]
        issue = _issue(state="Todo", blocked_by=blockers)
        assert orch._can_dispatch(issue) is True

    def test_in_retry_queue_rejected(self, tmp_path):
        from symphony.models import RetryEntry
        orch = self._make_orchestrator_with_config(tmp_path)
        orch._state.retry_queue["id-1"] = RetryEntry(
            issue_id="id-1", identifier="ENG-1", attempt=2, due_at_ms=0
        )
        issue = _issue(state="In Progress")
        assert orch._can_dispatch(issue) is False


class TestStateNormalization:
    """Regression: state comparisons must be case-insensitive (spec §4.2)."""

    def _make_orch(self, tmp_path):
        wf = tmp_path / "WORKFLOW.md"
        wf.write_text(
            "---\ntracker:\n  kind: linear\n  api_key: t\n  project_slug: p\n"
            "  active_states: [In Progress, Todo]\n"
            "  terminal_states: [Done]\n"
            "codex:\n  command: codex\n---\nX\n"
        )
        orch = Orchestrator(workflow_path=wf)
        orch._reload_workflow()
        orch._state.max_concurrent_agents = 2
        return orch

    def test_can_dispatch_lowercase_state(self, tmp_path):
        """'in progress' matches 'In Progress' in active_states."""
        orch = self._make_orch(tmp_path)
        assert orch._can_dispatch(_issue(state="in progress")) is True

    def test_can_dispatch_uppercase_state(self, tmp_path):
        """'IN PROGRESS' matches 'In Progress' in active_states."""
        orch = self._make_orch(tmp_path)
        assert orch._can_dispatch(_issue(state="IN PROGRESS")) is True

    def test_terminal_state_case_insensitive(self, tmp_path):
        """'done' (lowercase) matches 'Done' in terminal_states."""
        orch = self._make_orch(tmp_path)
        assert orch._can_dispatch(_issue(state="done")) is False

    @pytest.mark.asyncio
    async def test_reconcile_terminal_case_insensitive(self, tmp_path):
        """Reconcile cancels task when tracker returns lowercase terminal state."""
        from datetime import datetime, timezone
        from unittest.mock import AsyncMock

        orch = self._make_orch(tmp_path)
        attempt = RunAttempt(
            issue_id="issue-1",
            identifier="ENG-1",
            attempt=1,
            started_at=datetime.now(timezone.utc),
        )
        orch._state.running["issue-1"] = attempt

        async def noop():
            await asyncio.sleep(100)

        task = asyncio.create_task(noop())
        orch._worker_tasks["issue-1"] = task

        # Tracker returns "done" (lowercase) — should still be recognized as terminal
        done_issue = _issue(id="issue-1", state="done")
        mock_tracker = AsyncMock()
        mock_tracker.fetch_issue_states_by_ids = AsyncMock(return_value=[done_issue])
        orch._tracker = mock_tracker

        await orch._reconcile()

        # Give the event loop a chance to propagate the cancellation
        await asyncio.gather(task, return_exceptions=True)
        assert task.cancelled()

    @pytest.mark.asyncio
    async def test_is_issue_still_active_case_insensitive(self, tmp_path):
        """_is_issue_still_active matches 'in progress' against 'In Progress'."""
        from unittest.mock import AsyncMock

        orch = self._make_orch(tmp_path)
        active_issue = _issue(id="issue-1", state="in progress")
        mock_tracker = AsyncMock()
        mock_tracker.fetch_issue_states_by_ids = AsyncMock(return_value=[active_issue])
        orch._tracker = mock_tracker

        result = await orch._is_issue_still_active("issue-1")
        assert result is True
