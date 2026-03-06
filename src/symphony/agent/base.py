from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from typing import Any, Callable, Coroutine, Protocol, runtime_checkable

from symphony.models import Issue, TokenUsage


@dataclass
class AgentConfig:
    """Config passed to agent runner for a session."""

    command: str  # e.g. "codex" or "claude"
    approval_policy: str  # e.g. "auto-edit"
    max_turns: int
    turn_timeout_ms: int
    read_timeout_ms: int
    stall_timeout_ms: int
    turn_sandbox_policy: str | dict[str, Any] | None = None
    thread_sandbox: str | None = None


@dataclass
class AgentEvent:
    """Event emitted by agent runner to orchestrator."""

    event: str  # session_started, turn_started, turn_completed, turn_failed, token_usage, rate_limit, etc.
    timestamp: datetime
    session_id: str | None = None
    thread_id: str | None = None
    turn_id: str | None = None
    usage: TokenUsage | None = None
    message: str | None = None
    pid: str | None = None
    data: dict[str, Any] | None = None


@dataclass
class TurnResult:
    success: bool
    error: str | None = None
    completed_naturally: bool = True  # false if cancelled/timed out


EventCallback = Callable[[AgentEvent], Coroutine[Any, Any, None] | None]


@runtime_checkable
class AgentRunner(Protocol):
    async def start_session(
        self, workspace_path: Path, config: AgentConfig
    ) -> str:
        """Launch agent process, perform handshake. Returns thread_id."""
        ...

    async def run_turn(
        self,
        thread_id: str,
        prompt: str,
        issue: Issue,
        on_event: EventCallback | None = None,
    ) -> TurnResult:
        """Run one turn, streaming events to callback. Returns result."""
        ...

    async def stop_session(self) -> None:
        """Graceful shutdown: SIGTERM -> wait 5s -> SIGKILL."""
        ...

    @property
    def pid(self) -> str | None:
        """PID of the agent subprocess, if running."""
        ...

    @property
    def session_id(self) -> str | None:
        """Current session ID, if active."""
        ...
