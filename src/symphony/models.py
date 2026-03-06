from __future__ import annotations

import enum
from dataclasses import dataclass, field
from datetime import datetime
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    import asyncio


class RunStatus(enum.Enum):
    PreparingWorkspace = "preparing_workspace"
    BuildingPrompt = "building_prompt"
    LaunchingAgentProcess = "launching_agent_process"
    InitializingSession = "initializing_session"
    StreamingTurn = "streaming_turn"
    Finishing = "finishing"
    Succeeded = "succeeded"
    Failed = "failed"
    TimedOut = "timed_out"
    Stalled = "stalled"
    CanceledByReconciliation = "canceled_by_reconciliation"


@dataclass
class BlockerRef:
    id: str
    identifier: str
    state: str


@dataclass
class Issue:
    id: str
    identifier: str
    title: str
    description: str | None
    priority: int | None
    state: str
    branch_name: str | None
    url: str | None
    labels: list[str] = field(default_factory=list)
    blocked_by: list[BlockerRef] = field(default_factory=list)
    created_at: datetime | None = None
    updated_at: datetime | None = None


@dataclass
class Workspace:
    path: str  # absolute path
    workspace_key: str  # sanitized identifier
    created_now: bool


@dataclass
class TokenUsage:
    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0


@dataclass
class LiveSession:
    session_id: str | None = None
    thread_id: str | None = None
    turn_id: str | None = None
    codex_app_server_pid: str | None = None
    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0
    turn_count: int = 0
    last_event_at: datetime | None = None


@dataclass
class RunAttempt:
    issue_id: str
    identifier: str
    attempt: int
    workspace_path: str | None = None
    started_at: datetime | None = None
    status: RunStatus = RunStatus.PreparingWorkspace
    error: str | None = None
    session: LiveSession = field(default_factory=LiveSession)
    issue_state: str | None = None  # last known tracker state (for per-state concurrency)


@dataclass
class RetryEntry:
    issue_id: str
    identifier: str
    attempt: int
    due_at_ms: float
    timer_handle: asyncio.TimerHandle | None = None
    error: str | None = None


@dataclass
class RateLimitInfo:
    model: str
    remaining_tokens: int | None = None
    remaining_requests: int | None = None
    reset_at: datetime | None = None


@dataclass
class OrchestratorState:
    poll_interval_ms: int = 10000
    poll_interval_idle_ms: int = 60000
    max_concurrent_agents: int = 1
    running: dict[str, RunAttempt] = field(default_factory=dict)  # issue_id -> RunAttempt
    claimed: set[str] = field(default_factory=set)  # issue_ids being dispatched
    retry_queue: dict[str, RetryEntry] = field(default_factory=dict)  # issue_id -> RetryEntry
    completed_count: int = 0
    codex_totals: TokenUsage = field(default_factory=TokenUsage)
    rate_limits: list[RateLimitInfo] = field(default_factory=list)
