from __future__ import annotations

import asyncio
import signal
from datetime import datetime, timezone
from pathlib import Path

import structlog

from symphony.agent.base import AgentConfig, AgentEvent, AgentRunner
from symphony.agent.codex import CodexRunner
from symphony.config import SymphonyConfig, normalize_state
from symphony.models import (
    Issue,
    OrchestratorState,
    RetryEntry,
    RunAttempt,
    RunStatus,
    Workspace,
)
from symphony.tracker.base import TrackerClient
from symphony.tracker.linear import LinearClient
from symphony.workflow import WorkflowDefinition, load_workflow, render_prompt
from symphony.workspace import WorkspaceManager

log = structlog.get_logger()


class Orchestrator:
    """Main orchestration loop: poll tracker, dispatch agents, reconcile."""

    def __init__(
        self,
        workflow_path: str | Path,
        port: int | None = None,
        shutdown_event: asyncio.Event | None = None,
        name: str | None = None,
    ) -> None:
        self._workflow_path = Path(workflow_path).resolve()
        self._port_override = port
        self._name = name
        self._workflow: WorkflowDefinition | None = None
        self._config: SymphonyConfig | None = None
        self._state = OrchestratorState()
        self._tracker: TrackerClient | None = None
        self._workspace_mgr: WorkspaceManager | None = None
        self._poll_task: asyncio.Task[None] | None = None
        self._watch_task: asyncio.Task[None] | None = None
        self._worker_tasks: dict[str, asyncio.Task[None]] = {}
        # Use provided shared event (daemon mode) or own one (standalone mode).
        self._shutdown_event = shutdown_event if shutdown_event is not None else asyncio.Event()
        self._owns_shutdown_event = shutdown_event is None
        self._loop: asyncio.AbstractEventLoop | None = None

    # -- public API -----------------------------------------------------------

    async def start(self) -> None:
        """Start the orchestrator: load workflow, validate, begin polling."""
        self._loop = asyncio.get_running_loop()

        # Only install signal handlers when running standalone (not under DaemonManager).
        if self._owns_shutdown_event:
            for sig in (signal.SIGINT, signal.SIGTERM):
                self._loop.add_signal_handler(sig, self._request_shutdown)

        # Bind project name to all log messages for this orchestrator.
        if self._name:
            import structlog
            structlog.contextvars.bind_contextvars(project=self._name)

        # Load and validate workflow
        self._reload_workflow()
        assert self._config is not None

        errors = self._config.validate()
        if errors:
            raise RuntimeError(f"Config validation failed: {'; '.join(errors)}")

        # Initialize tracker
        self._tracker = LinearClient(
            api_key=self._config.tracker_api_key,
            endpoint=self._config.tracker_endpoint,
            project_slug=self._config.tracker_project_slug,
            active_states=self._config.active_states,
            terminal_states=self._config.terminal_states,
        )

        # Initialize workspace manager
        self._workspace_mgr = WorkspaceManager(
            root=self._config.workspace_root,
            hooks=self._config.hooks,
            hooks_timeout_ms=self._config.hooks_timeout_ms,
        )
        await self._workspace_mgr.ensure_root()

        # Apply state from config
        self._state.poll_interval_ms = self._config.poll_interval_ms
        self._state.poll_interval_idle_ms = self._config.poll_interval_idle_ms
        self._state.max_concurrent_agents = self._config.max_concurrent_agents

        # Terminal workspace cleanup
        await self._startup_cleanup()

        # Start file watcher for WORKFLOW.md hot-reload
        self._watch_task = asyncio.create_task(self._watch_workflow())

        log.info(
            "orchestrator.started",
            workflow=str(self._workflow_path),
            poll_interval_ms=self._state.poll_interval_ms,
            max_concurrent=self._state.max_concurrent_agents,
        )

        # Run poll loop
        await self._poll_loop()

    async def stop(self) -> None:
        """Graceful shutdown: cancel poll, terminate agents, cleanup."""
        log.info("orchestrator.stopping")

        if self._poll_task and not self._poll_task.done():
            self._poll_task.cancel()
        if self._watch_task and not self._watch_task.done():
            self._watch_task.cancel()

        # Terminate all running agents
        tasks = []
        for issue_id in list(self._state.running):
            log.info("orchestrator.stopping_agent", issue_id=issue_id)
            task = self._worker_tasks.get(issue_id)
            if task and not task.done():
                task.cancel()
                tasks.append(task)

        if tasks:
            await asyncio.gather(*tasks, return_exceptions=True)

        # Cancel retry timers
        for entry in self._state.retry_queue.values():
            if entry.timer_handle:
                cancel = getattr(entry.timer_handle, "cancel", None)
                if cancel:
                    cancel()
        self._state.retry_queue.clear()

        # Close tracker client
        if self._tracker:
            await self._tracker.close()

        log.info("orchestrator.stopped")

    def get_state(self) -> OrchestratorState:
        """Return current orchestrator state (for status API)."""
        return self._state

    async def trigger_refresh(self) -> None:
        """Trigger an immediate poll+reconciliation cycle."""
        log.info("orchestrator.manual_refresh")
        await self._tick()

    # -- signal handling ------------------------------------------------------

    def _request_shutdown(self) -> None:
        """Signal handler: set shutdown event."""
        log.info("orchestrator.shutdown_requested")
        self._shutdown_event.set()

    # -- poll loop ------------------------------------------------------------

    async def _poll_loop(self) -> None:
        """Main loop: tick at adaptive interval, stop on shutdown event.

        Uses poll_interval_ms when agents are active, poll_interval_idle_ms otherwise.
        """
        # Immediate first tick
        await self._tick()

        while not self._shutdown_event.is_set():
            active = bool(self._state.running or self._state.retry_queue)
            interval_ms = (
                self._state.poll_interval_ms
                if active
                else self._state.poll_interval_idle_ms
            )
            try:
                await asyncio.wait_for(
                    self._shutdown_event.wait(),
                    timeout=interval_ms / 1000.0,
                )
                break  # shutdown requested
            except asyncio.TimeoutError:
                pass  # normal: timeout means it's time to tick

            await self._tick()

        await self.stop()

    async def _tick(self) -> None:
        """One poll cycle: reconcile, fetch candidates, dispatch."""
        log.debug("orchestrator.tick")

        # Step 1: Reconcile running issues
        await self._reconcile()

        # Step 2: Validate dispatch config
        if not self._config or self._config.validate():
            log.warning("orchestrator.dispatch_skipped", reason="config_invalid")
            return

        assert self._tracker is not None

        # Step 3: Fetch candidate issues
        try:
            candidates = await self._tracker.fetch_candidate_issues()
        except Exception:
            log.exception("orchestrator.fetch_failed")
            return

        # Step 4: Sort candidates
        candidates = self._sort_candidates(candidates)

        # Step 5: Select and dispatch
        for issue in candidates:
            if not self._can_dispatch(issue):
                continue

            available_slots = self._state.max_concurrent_agents - len(self._state.running)
            if available_slots <= 0:
                break

            await self._dispatch(issue)

    # -- candidate selection (Section 8.2) ------------------------------------

    def _can_dispatch(self, issue: Issue) -> bool:
        """Check if an issue is eligible for dispatch (spec §8.2)."""
        assert self._config is not None

        # Already running, claimed, or scheduled for retry (spec §7.1)
        if (
            issue.id in self._state.running
            or issue.id in self._state.claimed
            or issue.id in self._state.retry_queue
        ):
            return False

        # State must be in active_states (case-insensitive, spec §4.2)
        norm_state = normalize_state(issue.state)
        active_norm = {normalize_state(s) for s in self._config.active_states}
        terminal_norm = {normalize_state(s) for s in self._config.terminal_states}

        if norm_state not in active_norm:
            return False

        if norm_state in terminal_norm:
            return False

        # Global concurrency
        if len(self._state.running) >= self._state.max_concurrent_agents:
            return False

        # Per-state concurrency (state keys normalized)
        by_state = self._config.max_concurrent_agents_by_state  # keys already normalized in config
        if by_state and norm_state in by_state:
            state_count = sum(
                1 for r in self._state.running.values()
                if normalize_state(r.issue_state or "") == norm_state
            )
            if state_count >= by_state[norm_state]:
                return False

        # Todo blocker gate (spec §8.2): only for the "todo" state
        if norm_state == "todo" and issue.blocked_by:
            for blocker in issue.blocked_by:
                if normalize_state(blocker.state) not in terminal_norm:
                    log.debug(
                        "orchestrator.blocked",
                        issue=issue.identifier,
                        blocker=blocker.identifier,
                    )
                    return False

        return True

    @staticmethod
    def _sort_candidates(candidates: list[Issue]) -> list[Issue]:
        """Sort: priority asc (null last), created_at oldest, identifier lexicographic."""

        def sort_key(issue: Issue) -> tuple[int, datetime, str]:
            # Priority: lower is higher priority, None goes last
            pri = issue.priority if issue.priority is not None else 999
            # Created: oldest first
            created = issue.created_at or datetime.min.replace(tzinfo=timezone.utc)
            return (pri, created, issue.identifier)

        return sorted(candidates, key=sort_key)

    # -- dispatch (Section 16.4) ----------------------------------------------

    async def _dispatch(self, issue: Issue, attempt_num: int = 1) -> None:
        """Spawn an async task for run_agent_attempt."""
        log.info("orchestrator.dispatching", issue=issue.identifier, attempt=attempt_num)

        self._state.claimed.add(issue.id)

        attempt = RunAttempt(
            issue_id=issue.id,
            identifier=issue.identifier,
            attempt=attempt_num,
            started_at=datetime.now(timezone.utc),
            issue_state=issue.state,
        )
        self._state.running[issue.id] = attempt

        task = asyncio.create_task(
            self._run_agent_attempt(issue, attempt),
            name=f"worker-{issue.identifier}",
        )
        self._worker_tasks[issue.id] = task
        task.add_done_callback(lambda t: self._on_worker_done(issue.id, t))

    def _on_worker_done(self, issue_id: str, task: asyncio.Task[None]) -> None:
        """Callback when a worker task finishes (spec §7.3, §16.6)."""
        attempt = self._state.running.pop(issue_id, None)
        self._worker_tasks.pop(issue_id, None)
        attempt_num = attempt.attempt if attempt else 1
        identifier = attempt.identifier if attempt else issue_id

        if task.cancelled():
            log.info("orchestrator.worker_cancelled", issue_id=issue_id)
            self._state.claimed.discard(issue_id)
        elif task.exception():
            exc = task.exception()
            log.error(
                "orchestrator.worker_failed",
                issue_id=issue_id,
                error=str(exc),
            )
            # Abnormal exit: exponential-backoff retry (stay claimed)
            self._schedule_retry(
                issue_id,
                error=str(exc),
                abnormal=True,
                attempt=attempt_num,
                identifier=identifier,
            )
        else:
            # Normal exit: schedule short continuation retry (1s) so orchestrator
            # can re-check if issue still needs work (spec §7.3 §16.6)
            log.info("orchestrator.worker_completed", issue_id=issue_id)
            self._state.completed_count += 1
            self._schedule_retry(
                issue_id,
                error=None,
                abnormal=False,
                attempt=1,
                identifier=identifier,
            )

    # -- worker attempt (Section 16.5) ----------------------------------------

    async def _run_agent_attempt(self, issue: Issue, attempt: RunAttempt) -> None:
        """Full lifecycle for one agent attempt on an issue."""
        assert self._config is not None
        assert self._workspace_mgr is not None
        assert self._workflow is not None

        runner: AgentRunner | None = None

        try:
            # 1. Create/reuse workspace
            attempt.status = RunStatus.PreparingWorkspace
            workspace = await self._workspace_mgr.setup_workspace(issue.identifier)
            attempt.workspace_path = workspace.path

            # 2. Run before_run hook
            await self._workspace_mgr.prepare_for_run(workspace)

            # 3. Build prompt
            attempt.status = RunStatus.BuildingPrompt
            prompt = render_prompt(self._workflow, issue, attempt=attempt.attempt)

            # 4. Create agent runner
            attempt.status = RunStatus.LaunchingAgentProcess
            runner = self._create_runner()

            agent_config = AgentConfig(
                command=self._config.codex_command,
                approval_policy=self._config.approval_policy,
                max_turns=self._config.max_turns,
                turn_timeout_ms=self._config.turn_timeout_ms,
                read_timeout_ms=self._config.read_timeout_ms,
                stall_timeout_ms=self._config.stall_timeout_ms,
                turn_sandbox_policy=self._config.turn_sandbox_policy,
                thread_sandbox=self._config.thread_sandbox,
            )

            # 5. Start session (handshake)
            attempt.status = RunStatus.InitializingSession
            thread_id = await runner.start_session(
                Path(workspace.path), agent_config
            )
            attempt.session.thread_id = thread_id
            attempt.session.session_id = runner.session_id
            attempt.session.codex_app_server_pid = runner.pid

            # 6. Turn loop
            for turn_num in range(1, self._config.max_turns + 1):
                attempt.status = RunStatus.StreamingTurn
                attempt.session.turn_count = turn_num

                # Use full prompt on first turn, continuation on subsequent
                turn_prompt = prompt if turn_num == 1 else (
                    f"Continue working on {issue.identifier}: {issue.title}. "
                    f"This is turn {turn_num} of {self._config.max_turns}."
                )

                result = await runner.run_turn(
                    thread_id,
                    turn_prompt,
                    issue,
                    on_event=lambda e: self._handle_agent_event(issue.id, e),
                )

                if not result.success:
                    if not result.completed_naturally:
                        log.warning(
                            "orchestrator.turn_abnormal",
                            issue=issue.identifier,
                            turn=turn_num,
                            error=result.error,
                        )
                        attempt.status = RunStatus.Failed
                        attempt.error = result.error
                        raise RuntimeError(f"Turn failed: {result.error}")

                    log.info(
                        "orchestrator.turn_failed",
                        issue=issue.identifier,
                        turn=turn_num,
                        error=result.error,
                    )
                    break

                # Check if issue is still active before continuing
                if turn_num < self._config.max_turns:
                    still_active = await self._is_issue_still_active(issue.id)
                    if not still_active:
                        log.info(
                            "orchestrator.issue_no_longer_active",
                            issue=issue.identifier,
                        )
                        break

            # 7. Finish
            attempt.status = RunStatus.Finishing
            await runner.stop_session()
            runner = None

            await self._workspace_mgr.finish_run(workspace)
            attempt.status = RunStatus.Succeeded

        except asyncio.CancelledError:
            attempt.status = RunStatus.CanceledByReconciliation
            raise

        except Exception as exc:
            attempt.status = RunStatus.Failed
            attempt.error = str(exc)
            log.exception(
                "orchestrator.attempt_failed",
                issue=issue.identifier,
                attempt=attempt.attempt,
            )
            raise

        finally:
            if runner is not None:
                try:
                    await runner.stop_session()
                except Exception:
                    log.warning("orchestrator.runner_cleanup_error", issue=issue.identifier)

    def _create_runner(self) -> AgentRunner:
        """Create the agent runner."""
        return CodexRunner()  # type: ignore[return-value]

    def _handle_agent_event(self, issue_id: str, event: AgentEvent) -> None:
        """Process agent events: update state, track tokens."""
        attempt = self._state.running.get(issue_id)
        if not attempt:
            return

        attempt.session.last_event_at = event.timestamp

        if event.session_id:
            attempt.session.session_id = event.session_id
        if event.pid:
            attempt.session.codex_app_server_pid = event.pid

        # Token accounting
        if event.usage:
            self._state.codex_totals.input_tokens += event.usage.input_tokens
            self._state.codex_totals.output_tokens += event.usage.output_tokens
            self._state.codex_totals.total_tokens += event.usage.total_tokens
            attempt.session.input_tokens += event.usage.input_tokens
            attempt.session.output_tokens += event.usage.output_tokens
            attempt.session.total_tokens += event.usage.total_tokens

        log.debug(
            "orchestrator.agent_event",
            issue_id=issue_id,
            agent_event=event.event,
        )

    # -- reconciliation (Section 8.5) -----------------------------------------

    async def _reconcile(self) -> None:
        """Reconcile running issues against tracker state."""
        if not self._state.running:
            return

        assert self._config is not None
        assert self._tracker is not None

        # Part A: Stall detection
        now = datetime.now(timezone.utc)
        stall_ms = self._config.stall_timeout_ms

        for issue_id, attempt in list(self._state.running.items()):
            last_ts = attempt.session.last_event_at or attempt.started_at
            if last_ts is None:
                continue

            elapsed_ms = (now - last_ts).total_seconds() * 1000
            if elapsed_ms > stall_ms:
                log.warning(
                    "orchestrator.stall_detected",
                    issue=attempt.identifier,
                    elapsed_ms=elapsed_ms,
                )
                attempt.status = RunStatus.Stalled
                task = self._worker_tasks.get(issue_id)
                if task and not task.done():
                    task.cancel()

        # Part B: Tracker state refresh
        running_ids = list(self._state.running.keys())
        if not running_ids:
            return

        try:
            current_issues = await self._tracker.fetch_issue_states_by_ids(running_ids)
        except Exception:
            log.exception("orchestrator.reconcile_fetch_failed")
            return

        issue_map = {i.id: i for i in current_issues}
        terminal_states = {normalize_state(s) for s in self._config.terminal_states}
        for issue_id in running_ids:
            if issue_id not in self._state.running:
                continue  # already removed by stall detection

            current = issue_map.get(issue_id)
            if current is None:
                # Issue not found — kill without cleanup
                log.warning("orchestrator.reconcile_issue_missing", issue_id=issue_id)
                task = self._worker_tasks.get(issue_id)
                if task and not task.done():
                    task.cancel()
            elif normalize_state(current.state) in terminal_states:
                # Terminal — kill + cleanup workspace
                log.info(
                    "orchestrator.reconcile_terminal",
                    issue=current.identifier,
                    state=current.state,
                )
                attempt = self._state.running.get(issue_id)
                task = self._worker_tasks.get(issue_id)
                if task and not task.done():
                    task.cancel()
                if attempt and attempt.workspace_path and self._workspace_mgr:
                    try:
                        ws = Workspace(
                            path=attempt.workspace_path,
                            workspace_key=attempt.identifier,
                            created_now=False,
                        )
                        await self._workspace_mgr.cleanup(ws)
                    except Exception:
                        log.warning(
                            "orchestrator.reconcile_cleanup_failed",
                            issue_id=issue_id,
                        )

    # -- retry (Section 8.4) --------------------------------------------------

    def _schedule_retry(
        self,
        issue_id: str,
        error: str | None = None,
        abnormal: bool = False,
        attempt: int = 1,
        identifier: str = "",
    ) -> None:
        """Schedule a retry; issue stays in `claimed` while queued (spec §7.1, §8.4)."""
        assert self._config is not None

        # Cancel any existing retry timer for same issue
        existing = self._state.retry_queue.get(issue_id)
        if existing and existing.timer_handle:
            cancel = getattr(existing.timer_handle, "cancel", None)
            if cancel:
                cancel()

        if abnormal:
            # Spec §8.4: delay = min(10000 * 2^(attempt-1), max_retry_backoff_ms)
            delay_ms = min(10000 * (2 ** (attempt - 1)), self._config.max_retry_backoff_ms)
        else:
            delay_ms = 1000  # normal continuation retry

        due_at_ms = asyncio.get_running_loop().time() * 1000 + delay_ms

        entry = RetryEntry(
            issue_id=issue_id,
            identifier=identifier,
            attempt=attempt,
            due_at_ms=due_at_ms,
            error=error,
        )

        loop = asyncio.get_running_loop()
        handle = loop.call_later(
            delay_ms / 1000.0,
            lambda: asyncio.ensure_future(self._on_retry_timer(issue_id)),
        )
        entry.timer_handle = handle
        self._state.retry_queue[issue_id] = entry
        self._state.claimed.add(issue_id)  # stay claimed while queued

        log.info(
            "orchestrator.retry_scheduled",
            issue_id=issue_id,
            attempt=entry.attempt,
            delay_ms=delay_ms,
        )

    async def _on_retry_timer(self, issue_id: str) -> None:
        """Fired when a retry timer expires (spec §8.4, §16.6)."""
        entry = self._state.retry_queue.pop(issue_id, None)
        if entry is None:
            return

        assert self._tracker is not None
        assert self._config is not None

        # Re-fetch active candidates to check if this issue is still eligible
        try:
            candidates = await self._tracker.fetch_candidate_issues()
        except Exception:
            log.exception("orchestrator.retry_fetch_failed", issue_id=issue_id)
            # Re-queue with incremented attempt on fetch failure
            self._schedule_retry(
                issue_id, error="retry poll failed", abnormal=True,
                attempt=entry.attempt + 1, identifier=entry.identifier,
            )
            return

        issue = next((c for c in candidates if c.id == issue_id), None)

        if issue is None:
            # Not found among active candidates — release claim
            log.info("orchestrator.retry_issue_gone", issue_id=issue_id)
            self._state.claimed.discard(issue_id)
            return

        # Check per-spec eligibility (excluding claimed check since we hold the claim)
        norm_state = normalize_state(issue.state)
        active_norm = {normalize_state(s) for s in self._config.active_states}
        if norm_state not in active_norm:
            # No longer active — release claim
            log.info("orchestrator.retry_issue_inactive", issue_id=issue_id, state=issue.state)
            self._state.claimed.discard(issue_id)
            return

        available = self._state.max_concurrent_agents - len(self._state.running)
        if available <= 0:
            log.debug("orchestrator.retry_no_slots", issue_id=issue_id)
            self._schedule_retry(
                issue_id,
                error="no available orchestrator slots",
                abnormal=True,
                attempt=entry.attempt + 1,
                identifier=entry.identifier,
            )
            return

        # Temporarily remove from claimed so _dispatch can re-add
        self._state.claimed.discard(issue_id)
        await self._dispatch(issue, attempt_num=entry.attempt)

    # -- issue state check ----------------------------------------------------

    async def _is_issue_still_active(self, issue_id: str) -> bool:
        """Check if an issue is still in an active state."""
        assert self._tracker is not None
        assert self._config is not None

        try:
            issues = await self._tracker.fetch_issue_states_by_ids([issue_id])
        except Exception:
            log.warning("orchestrator.state_check_failed", issue_id=issue_id)
            return True  # assume active on error

        if not issues:
            return False

        active_norm = {normalize_state(s) for s in self._config.active_states}
        return normalize_state(issues[0].state) in active_norm

    # -- startup cleanup ------------------------------------------------------

    async def _startup_cleanup(self) -> None:
        """Clean up workspaces for issues that are in terminal states."""
        assert self._tracker is not None
        assert self._config is not None
        assert self._workspace_mgr is not None

        terminal_states = self._config.terminal_states
        if not terminal_states:
            return

        try:
            terminal_issues = await self._tracker.fetch_issues_by_states(terminal_states)
        except Exception:
            log.warning("orchestrator.startup_cleanup_fetch_failed")
            return

        root = Path(self._config.workspace_root)
        for issue in terminal_issues:
            key = WorkspaceManager.sanitize_identifier(issue.identifier)
            ws_path = root / key
            if ws_path.exists():
                log.info(
                    "orchestrator.startup_cleanup",
                    issue=issue.identifier,
                    path=str(ws_path),
                )
                try:
                    ws = Workspace(path=str(ws_path), workspace_key=key, created_now=False)
                    await self._workspace_mgr.cleanup(ws)
                except Exception:
                    log.warning(
                        "orchestrator.startup_cleanup_failed",
                        issue=issue.identifier,
                    )

    # -- workflow hot-reload (Section 6.2) ------------------------------------

    async def _watch_workflow(self) -> None:
        """Watch WORKFLOW.md for changes and hot-reload config."""
        try:
            import watchfiles
        except ImportError:
            log.warning("orchestrator.watchfiles_not_available")
            return

        try:
            async for changes in watchfiles.awatch(self._workflow_path):
                log.info("orchestrator.workflow_changed", changes=str(changes))
                try:
                    self._reload_workflow()
                    log.info("orchestrator.workflow_reloaded")
                except Exception:
                    log.exception("orchestrator.workflow_reload_failed")
                    # Keep last-known-good config
        except asyncio.CancelledError:
            pass
        except Exception:
            log.exception("orchestrator.watcher_error")

    def _reload_workflow(self) -> None:
        """Reload WORKFLOW.md and update config."""
        self._workflow = load_workflow(self._workflow_path)
        self._config = SymphonyConfig(self._workflow.config)

        # Apply dynamic config changes
        self._state.poll_interval_ms = self._config.poll_interval_ms
        self._state.poll_interval_idle_ms = self._config.poll_interval_idle_ms
        self._state.max_concurrent_agents = self._config.max_concurrent_agents
