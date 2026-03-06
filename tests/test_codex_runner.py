from __future__ import annotations

import asyncio
from pathlib import Path

import pytest

from symphony.agent.base import AgentConfig, TurnResult
from symphony.agent.codex import CodexRunner
from symphony.agent.protocol import JsonRpcResponse, format_message
from symphony.models import Issue


class TestCodexRunnerCommand:
    def test_codex_command_disables_notify(self):
        command = CodexRunner._build_launch_command("codex app-server")
        assert command == "codex app-server -c 'notify=[]'"

    def test_absolute_codex_path_disables_notify(self):
        command = CodexRunner._build_launch_command("/usr/local/bin/codex app-server")
        assert command == "/usr/local/bin/codex app-server -c 'notify=[]'"

    def test_non_codex_command_unchanged(self):
        command = CodexRunner._build_launch_command("python fake_server.py")
        assert command == "python fake_server.py"


class _FakeStreamWriter:
    def write(self, data: bytes) -> None:
        self.last_write = data

    async def drain(self) -> None:
        return None


class _FakeProcess:
    def __init__(self, *, stdout_limit: int = 2**16, stderr_limit: int = 2**16) -> None:
        self.pid = 1234
        self.returncode = None
        self.stdin = _FakeStreamWriter()
        self.stdout = asyncio.StreamReader(limit=stdout_limit)
        self.stderr = asyncio.StreamReader(limit=stderr_limit)


class TestCodexRunnerSandbox:
    @pytest.mark.asyncio
    async def test_start_session_derives_thread_sandbox_from_turn_policy(
        self,
        monkeypatch,
        tmp_path,
    ):
        runner = CodexRunner()
        sent_requests: list[tuple[str, dict]] = []

        async def fake_create_subprocess_exec(*args, **kwargs):
            return _FakeProcess()

        async def fake_send_request(method, params=None, timeout_ms=None):
            sent_requests.append((method, params or {}))
            if method == "initialize":
                return {"serverInfo": {"name": "codex"}}
            if method == "thread/start":
                return {"thread": {"id": "thread-1"}}
            raise AssertionError(f"unexpected method: {method}")

        async def fake_send_notification(method, params=None):
            return None

        async def fake_reader():
            return None

        monkeypatch.setattr(asyncio, "create_subprocess_exec", fake_create_subprocess_exec)
        monkeypatch.setattr(runner, "_send_request", fake_send_request)
        monkeypatch.setattr(runner, "_send_notification", fake_send_notification)
        monkeypatch.setattr(runner, "_read_stdout", fake_reader)
        monkeypatch.setattr(runner, "_read_stderr", fake_reader)

        config = AgentConfig(
            command="codex app-server",
            approval_policy="never",
            max_turns=1,
            turn_timeout_ms=1000,
            read_timeout_ms=1000,
            stall_timeout_ms=1000,
            turn_sandbox_policy={"type": "workspace-write"},
            thread_sandbox=None,
        )

        await runner.start_session(tmp_path, config)

        thread_start = next(params for method, params in sent_requests if method == "thread/start")
        assert thread_start["sandbox"] == "workspace-write"

    @pytest.mark.asyncio
    async def test_run_turn_sends_structured_sandbox_policy(self):
        runner = CodexRunner()
        runner._process = _FakeProcess()
        runner._workspace_path = Path("/tmp/workspace")
        runner._config = AgentConfig(
            command="codex app-server",
            approval_policy="never",
            max_turns=1,
            turn_timeout_ms=1000,
            read_timeout_ms=1000,
            stall_timeout_ms=1000,
            turn_sandbox_policy={
                "type": "workspace-write",
                "network_access": True,
                "writable_roots": ["/tmp/shared-git-dir"],
            },
            thread_sandbox="workspace-write",
        )
        sent_requests: list[tuple[str, dict]] = []

        async def fake_send_request(method, params=None, timeout_ms=None):
            sent_requests.append((method, params or {}))
            return {}

        async def fake_consume_turn_events(thread_id, turn_id, on_event):
            return TurnResult(success=True)

        runner._send_request = fake_send_request  # type: ignore[method-assign]
        runner._consume_turn_events = fake_consume_turn_events  # type: ignore[method-assign]

        issue = Issue(
            id="issue-1",
            identifier="TEST-1",
            title="Test issue",
            description=None,
            priority=None,
            state="In Progress",
            branch_name=None,
            url=None,
            created_at=None,  # type: ignore[arg-type]
        )

        await runner.run_turn("thread-1", "do work", issue)

        turn_start = next(params for method, params in sent_requests if method == "turn/start")
        assert turn_start["sandboxPolicy"] == {
            "type": "workspace-write",
            "network_access": True,
            "writable_roots": ["/tmp/shared-git-dir"],
        }


class TestCodexRunnerStreamHandling:
    @pytest.mark.asyncio
    async def test_read_stdout_handles_json_line_larger_than_stream_limit(self):
        runner = CodexRunner()
        runner._process = _FakeProcess(stdout_limit=64)

        future = asyncio.get_running_loop().create_future()
        runner._pending_requests[1] = future

        runner._process.stdout.feed_data(
            format_message(
                JsonRpcResponse(
                    id=1,
                    result={"payload": "x" * 512},
                )
            ).encode("utf-8")
        )
        runner._process.stdout.feed_eof()

        await runner._read_stdout()

        assert future.done()
        assert future.result() == {"payload": "x" * 512}
