from __future__ import annotations

import asyncio
import inspect
import os
import shlex
import signal
import uuid
from collections.abc import AsyncIterator
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import structlog

from symphony.agent.base import (
    AgentConfig,
    AgentEvent,
    EventCallback,
    TurnResult,
)
from symphony.agent.protocol import (
    JsonRpcError,
    JsonRpcNotification,
    JsonRpcRequest,
    JsonRpcResponse,
    Methods,
    format_message,
    parse_message,
)
from symphony.models import Issue, TokenUsage

log = structlog.get_logger()

_THREAD_SANDBOX_MODES = {"read-only", "workspace-write"}
_TURN_SANDBOX_POLICY_TYPES = {
    "read-only",
    "workspace-write",
    "external-sandbox",
}
_STREAM_READ_CHUNK_SIZE = 64 * 1024


class CodexError(Exception):
    def __init__(self, code: str, message: str) -> None:
        self.code = code
        super().__init__(message)


class CodexRunner:
    """Codex app-server subprocess runner.

    Lifecycle:
    1. start_session: launch process, initialize handshake, create thread
    2. run_turn (repeated): send turn/start, stream events, handle approvals
    3. stop_session: graceful SIGTERM -> wait 5s -> SIGKILL
    """

    def __init__(self) -> None:
        self._process: asyncio.subprocess.Process | None = None
        self._session_id: str | None = None
        self._thread_id: str | None = None
        self._request_id_counter: int = 0
        self._pending_requests: dict[int, asyncio.Future[dict[str, Any]]] = {}
        self._reader_task: asyncio.Task[None] | None = None
        self._stderr_task: asyncio.Task[None] | None = None
        self._notification_queue: asyncio.Queue[
            JsonRpcNotification | JsonRpcRequest
        ] = asyncio.Queue()
        self._workspace_path: Path | None = None
        self._config: AgentConfig | None = None
        self._last_token_usage: TokenUsage = TokenUsage()

    @staticmethod
    def _build_launch_command(command: str) -> str:
        """Build launch command with Symphony-specific Codex overrides."""
        cmd = command.strip()
        if not cmd:
            return command

        try:
            parts = shlex.split(cmd)
        except ValueError:
            parts = cmd.split()

        if not parts:
            return cmd
        if Path(parts[0]).name != "codex":
            return cmd

        # Force-disable global Codex notify hooks for unattended Symphony runs.
        return f"{cmd} -c 'notify=[]'"

    def _next_request_id(self) -> int:
        self._request_id_counter += 1
        return self._request_id_counter

    @staticmethod
    def _normalize_turn_sandbox_policy(
        policy: str | dict[str, Any] | None,
    ) -> dict[str, Any] | None:
        if isinstance(policy, str):
            mode = policy.strip()
            if mode in _TURN_SANDBOX_POLICY_TYPES:
                return {"type": mode}
            return None

        if isinstance(policy, dict):
            sandbox_type = policy.get("type")
            if isinstance(sandbox_type, str) and sandbox_type in _TURN_SANDBOX_POLICY_TYPES:
                return policy

        return None

    @classmethod
    def _resolve_thread_sandbox_mode(cls, config: AgentConfig) -> str | None:
        if config.thread_sandbox in _THREAD_SANDBOX_MODES:
            return config.thread_sandbox

        policy = cls._normalize_turn_sandbox_policy(config.turn_sandbox_policy)
        if policy is None:
            return None

        sandbox_type = policy.get("type")
        if isinstance(sandbox_type, str) and sandbox_type in _THREAD_SANDBOX_MODES:
            return sandbox_type

        return None

    @property
    def pid(self) -> str | None:
        if self._process and self._process.pid:
            return str(self._process.pid)
        return None

    @property
    def session_id(self) -> str | None:
        return self._session_id

    async def start_session(
        self, workspace_path: Path, config: AgentConfig
    ) -> str:
        """Launch codex app-server and perform handshake.

        1. Launch process with workspace as cwd
        2. Start reader tasks for stdout/stderr
        3. Send initialize request, wait for response
        4. Send initialized notification
        5. Send thread/start request, get thread_id
        """
        self._workspace_path = workspace_path
        self._config = config
        self._session_id = uuid.uuid4().hex
        self._last_token_usage = TokenUsage()
        launch_command = self._build_launch_command(config.command)

        env = {**os.environ, "CODEX_APPROVAL_POLICY": config.approval_policy}

        self._process = await asyncio.create_subprocess_exec(
            "bash",
            "-lc",
            launch_command,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            cwd=str(workspace_path),
            env=env,
        )

        log.info(
            "codex.process_started",
            pid=self._process.pid,
            session_id=self._session_id,
            command=launch_command,
        )

        # Start background readers
        self._reader_task = asyncio.create_task(self._read_stdout())
        self._stderr_task = asyncio.create_task(self._read_stderr())

        # Initialize handshake
        init_result = await self._send_request(
            Methods.INITIALIZE,
            {
                "protocolVersion": "2025-01-01",
                "capabilities": {},
                "clientInfo": {
                    "name": "symphony",
                    "version": "0.1.0",
                },
            },
            timeout_ms=config.read_timeout_ms,
        )
        log.info("codex.initialized", server_info=init_result.get("serverInfo"))

        # Send initialized notification
        await self._send_notification(Methods.INITIALIZED)

        # Create thread
        thread_params: dict[str, Any] = {
            "approvalPolicy": config.approval_policy,
            "cwd": str(workspace_path),
        }
        thread_sandbox = self._resolve_thread_sandbox_mode(config)
        if thread_sandbox:
            thread_params["sandbox"] = thread_sandbox

        thread_result = await self._send_request(
            Methods.THREAD_START,
            thread_params,
            timeout_ms=config.read_timeout_ms,
        )
        thread_obj = (thread_result or {}).get("thread") or {}
        self._thread_id = str(thread_obj.get("id") or "")
        if not self._thread_id:
            raise CodexError("response_error", "thread/start response missing thread.id")

        log.info("codex.thread_created", thread_id=self._thread_id)
        return self._thread_id

    async def run_turn(
        self,
        thread_id: str,
        prompt: str,
        issue: Issue,
        on_event: EventCallback | None = None,
    ) -> TurnResult:
        """Run one turn on the thread.

        Sends turn/start, then streams events until turn completes/fails/cancels.
        Auto-approves command execution and file change requests.
        """
        if not self._process or self._process.returncode is not None:
            return TurnResult(
                success=False,
                error="process_not_running",
                completed_naturally=False,
            )

        turn_id = uuid.uuid4().hex

        self._emit_event(
            on_event,
            AgentEvent(
                event="turn_started",
                timestamp=datetime.now(timezone.utc),
                session_id=self._session_id,
                thread_id=thread_id,
                turn_id=turn_id,
                pid=self.pid,
            ),
        )

        # Send turn/start as a request per spec §16.3
        cwd = str(self._workspace_path) if self._workspace_path else "."
        title = f"{issue.identifier}: {issue.title}" if issue.title else issue.identifier
        await self._send_request(
            Methods.TURN_START,
            {
                "threadId": thread_id,
                "input": [{"type": "text", "text": prompt}],
                "cwd": cwd,
                "title": title,
                "approvalPolicy": self._config.approval_policy if self._config else "auto-edit",
                **(
                    {"sandboxPolicy": sandbox_policy}
                    if (sandbox_policy := self._normalize_turn_sandbox_policy(
                        self._config.turn_sandbox_policy if self._config else None
                    ))
                    else {}
                ),
            },
            timeout_ms=self._config.turn_timeout_ms if self._config else 3600000,
        )

        # Drain any stale notifications before this turn
        while not self._notification_queue.empty():
            try:
                self._notification_queue.get_nowait()
            except asyncio.QueueEmpty:
                break

        # Consume events until turn completes
        try:
            return await asyncio.wait_for(
                self._consume_turn_events(thread_id, turn_id, on_event),
                timeout=30 * 60,  # 30 min max safety net
            )
        except asyncio.TimeoutError:
            log.warning("codex.turn_timeout", turn_id=turn_id)
            self._emit_event(
                on_event,
                AgentEvent(
                    event="turn_failed",
                    timestamp=datetime.now(timezone.utc),
                    session_id=self._session_id,
                    thread_id=thread_id,
                    turn_id=turn_id,
                    message="turn_timeout",
                ),
            )
            return TurnResult(
                success=False,
                error="turn_timeout",
                completed_naturally=False,
            )

    async def _consume_turn_events(
        self,
        thread_id: str,
        turn_id: str,
        on_event: EventCallback | None,
    ) -> TurnResult:
        """Consume notification queue until turn ends."""
        while True:
            msg = await self._notification_queue.get()

            # Handle server requests (approvals) inline
            if isinstance(msg, JsonRpcRequest):
                await self._handle_server_request(msg, on_event)
                continue

            method = msg.method
            params = msg.params

            if method == Methods.TURN_COMPLETED:
                self._emit_event(
                    on_event,
                    AgentEvent(
                        event="turn_completed",
                        timestamp=datetime.now(timezone.utc),
                        session_id=self._session_id,
                        thread_id=thread_id,
                        turn_id=turn_id,
                    ),
                )
                return TurnResult(success=True)

            elif method == Methods.TURN_FAILED:
                error_msg = params.get("error", "unknown_error")
                self._emit_event(
                    on_event,
                    AgentEvent(
                        event="turn_failed",
                        timestamp=datetime.now(timezone.utc),
                        session_id=self._session_id,
                        thread_id=thread_id,
                        turn_id=turn_id,
                        message=error_msg,
                    ),
                )
                return TurnResult(success=False, error=error_msg)

            elif method == Methods.TURN_CANCELLED:
                self._emit_event(
                    on_event,
                    AgentEvent(
                        event="turn_cancelled",
                        timestamp=datetime.now(timezone.utc),
                        session_id=self._session_id,
                        thread_id=thread_id,
                        turn_id=turn_id,
                    ),
                )
                return TurnResult(
                    success=False,
                    error="cancelled",
                    completed_naturally=False,
                )

            elif method == Methods.TOKEN_USAGE_UPDATED:
                # Protocol sends absolute totals; compute deltas
                new_input = params.get("inputTokens", 0)
                new_output = params.get("outputTokens", 0)
                new_total = params.get("totalTokens", new_input + new_output)

                delta = TokenUsage(
                    input_tokens=new_input - self._last_token_usage.input_tokens,
                    output_tokens=new_output - self._last_token_usage.output_tokens,
                    total_tokens=new_total - self._last_token_usage.total_tokens,
                )
                self._last_token_usage = TokenUsage(
                    input_tokens=new_input,
                    output_tokens=new_output,
                    total_tokens=new_total,
                )

                self._emit_event(
                    on_event,
                    AgentEvent(
                        event="token_usage",
                        timestamp=datetime.now(timezone.utc),
                        session_id=self._session_id,
                        thread_id=thread_id,
                        turn_id=turn_id,
                        usage=delta,
                        data={"cumulative": {
                            "input_tokens": new_input,
                            "output_tokens": new_output,
                            "total_tokens": new_total,
                        }},
                    ),
                )

            elif method == Methods.RATE_LIMITS_UPDATED:
                self._emit_event(
                    on_event,
                    AgentEvent(
                        event="rate_limit",
                        timestamp=datetime.now(timezone.utc),
                        session_id=self._session_id,
                        thread_id=thread_id,
                        turn_id=turn_id,
                        data=params,
                    ),
                )

            else:
                log.debug("codex.unhandled_notification", method=method)

    async def stop_session(self) -> None:
        """Graceful shutdown: SIGTERM -> wait 5s -> SIGKILL."""
        if self._reader_task and not self._reader_task.done():
            self._reader_task.cancel()
        if self._stderr_task and not self._stderr_task.done():
            self._stderr_task.cancel()

        if self._process and self._process.returncode is None:
            log.info("codex.stopping", pid=self._process.pid)
            try:
                self._process.send_signal(signal.SIGTERM)
                await asyncio.wait_for(self._process.wait(), timeout=5.0)
            except asyncio.TimeoutError:
                log.warning("codex.force_killing", pid=self._process.pid)
                self._process.kill()
                await self._process.wait()

        # Resolve any pending requests with errors
        for future in self._pending_requests.values():
            if not future.done():
                future.set_exception(
                    CodexError("session_closed", "Session stopped")
                )
        self._pending_requests.clear()

        self._process = None
        self._session_id = None
        self._thread_id = None
        self._request_id_counter = 0

        log.info("codex.stopped")

    # -- stdio reader tasks ------------------------------------------------

    async def _iter_stream_lines(
        self,
        stream: asyncio.StreamReader,
    ) -> AsyncIterator[bytes]:
        """Yield newline-delimited records without StreamReader readline limits."""
        buffer = bytearray()

        while True:
            chunk = await stream.read(_STREAM_READ_CHUNK_SIZE)
            if not chunk:
                break

            buffer.extend(chunk)

            while True:
                newline_index = buffer.find(b"\n")
                if newline_index < 0:
                    break

                yield bytes(buffer[:newline_index])
                del buffer[: newline_index + 1]

        if buffer:
            yield bytes(buffer)

    async def _read_stdout(self) -> None:
        """Background task: read lines from process stdout, dispatch."""
        assert self._process and self._process.stdout
        try:
            async for line_bytes in self._iter_stream_lines(self._process.stdout):
                line = line_bytes.decode("utf-8", errors="replace").strip()
                if not line:
                    continue

                try:
                    msg = parse_message(line)
                except Exception:
                    log.debug("codex.unparseable_stdout", line=line[:200])
                    continue

                if isinstance(msg, (JsonRpcResponse, JsonRpcError)):
                    # Resolve pending request future
                    msg_id = msg.id
                    # Normalize id: codex may return string or int
                    if isinstance(msg_id, str) and msg_id.isdigit():
                        msg_id = int(msg_id)
                    if isinstance(msg_id, int) and msg_id in self._pending_requests:
                        future = self._pending_requests.pop(msg_id)
                        if not future.done():
                            if isinstance(msg, JsonRpcError):
                                future.set_exception(
                                    CodexError(str(msg.code), msg.message)
                                )
                            else:
                                future.set_result(msg.result)
                    else:
                        log.debug(
                            "codex.unexpected_response",
                            id=msg_id,
                        )
                elif isinstance(msg, JsonRpcRequest):
                    # Server request (approval, user input) — put on queue
                    await self._notification_queue.put(msg)
                elif isinstance(msg, JsonRpcNotification):
                    await self._notification_queue.put(msg)

        except asyncio.CancelledError:
            pass
        except Exception:
            log.exception("codex.reader_error")

    async def _read_stderr(self) -> None:
        """Background task: read stderr and log lines."""
        assert self._process and self._process.stderr
        try:
            async for line_bytes in self._iter_stream_lines(self._process.stderr):
                line = line_bytes.decode("utf-8", errors="replace").rstrip()
                if line:
                    log.debug("codex.stderr", line=line[:500])
        except asyncio.CancelledError:
            pass
        except Exception:
            log.exception("codex.stderr_reader_error")

    # -- JSON-RPC communication --------------------------------------------

    async def _send_request(
        self,
        method: str,
        params: dict[str, Any] | None = None,
        timeout_ms: int | None = None,
    ) -> dict[str, Any]:
        """Send a JSON-RPC request and wait for response."""
        assert self._process and self._process.stdin

        req_id = self._next_request_id()
        request = JsonRpcRequest(
            method=method,
            id=req_id,
            params=params or {},
        )

        loop = asyncio.get_running_loop()
        future: asyncio.Future[dict[str, Any]] = loop.create_future()
        self._pending_requests[req_id] = future

        data = format_message(request).encode("utf-8")
        self._process.stdin.write(data)
        await self._process.stdin.drain()

        log.debug("codex.request_sent", method=method, id=req_id)

        timeout_s = (timeout_ms / 1000.0) if timeout_ms else 30.0
        try:
            result = await asyncio.wait_for(future, timeout=timeout_s)
            return result
        except asyncio.TimeoutError:
            self._pending_requests.pop(req_id, None)
            raise CodexError(
                "request_timeout",
                f"Timeout waiting for response to {method} (id={req_id})",
            )

    async def _send_notification(
        self, method: str, params: dict[str, Any] | None = None
    ) -> None:
        """Send a JSON-RPC notification (no response expected)."""
        assert self._process and self._process.stdin

        notification = JsonRpcNotification(
            method=method,
            params=params or {},
        )

        data = format_message(notification).encode("utf-8")
        self._process.stdin.write(data)
        await self._process.stdin.drain()

        log.debug("codex.notification_sent", method=method)

    async def _send_response(
        self, request_id: str | int, result: dict[str, Any]
    ) -> None:
        """Send a JSON-RPC response to a server request."""
        assert self._process and self._process.stdin

        response = JsonRpcResponse(id=request_id, result=result)
        data = format_message(response).encode("utf-8")
        self._process.stdin.write(data)
        await self._process.stdin.drain()

        log.debug("codex.response_sent", id=request_id)

    async def _handle_server_request(
        self,
        msg: JsonRpcRequest,
        on_event: EventCallback | None = None,
    ) -> None:
        """Handle incoming server requests (approvals, user input).

        - COMMAND_APPROVAL, FILE_CHANGE_APPROVAL -> auto-approve
        - USER_INPUT_REQUEST -> return error (unsupported)
        - Unknown -> return unsupported error
        """
        method = msg.method

        if method in (Methods.COMMAND_APPROVAL, Methods.FILE_CHANGE_APPROVAL):
            log.debug("codex.auto_approve", method=method, id=msg.id)
            await self._send_response(msg.id, {"approved": True})
            self._emit_event(
                on_event,
                AgentEvent(
                    event="approval_granted",
                    timestamp=datetime.now(timezone.utc),
                    session_id=self._session_id,
                    message=method,
                    data=msg.params,
                ),
            )

        elif method == Methods.USER_INPUT_REQUEST:
            log.warning("codex.user_input_unsupported", id=msg.id)
            err_response = JsonRpcError(
                id=msg.id,
                code=-32601,
                message="User input not supported in autonomous mode",
            )
            assert self._process and self._process.stdin
            data = format_message(err_response).encode("utf-8")
            self._process.stdin.write(data)
            await self._process.stdin.drain()

        else:
            log.warning("codex.unknown_server_request", method=method, id=msg.id)
            err_response = JsonRpcError(
                id=msg.id,
                code=-32601,
                message=f"Unsupported method: {method}",
            )
            assert self._process and self._process.stdin
            data = format_message(err_response).encode("utf-8")
            self._process.stdin.write(data)
            await self._process.stdin.drain()

    # -- event emission ----------------------------------------------------

    def _emit_event(
        self, on_event: EventCallback | None, event: AgentEvent
    ) -> None:
        """Emit event to callback, handling both sync and async callbacks."""
        if on_event is None:
            return
        result = on_event(event)
        if inspect.isawaitable(result):
            task = asyncio.ensure_future(result)
            task.add_done_callback(
                lambda t: log.warning("codex.event_callback_error", error=str(t.exception()))
                if not t.cancelled() and t.exception() else None
            )
