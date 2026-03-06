"""DaemonManager — coordinates multiple Orchestrator instances."""
from __future__ import annotations

import asyncio
import signal
from typing import Any

import structlog

from symphony.daemon.config import DaemonConfig, ProjectConfig
from symphony.models import OrchestratorState

log = structlog.get_logger()

_RESTART_DELAY_S = 5.0  # seconds before restarting a crashed project


class _ProjectRunner:
    """Manages the lifecycle of one Orchestrator within the daemon."""

    def __init__(
        self,
        project: ProjectConfig,
        shutdown_event: asyncio.Event,
    ) -> None:
        self.project = project
        self.shutdown_event = shutdown_event
        self._orchestrator: Any = None  # Orchestrator (imported lazily)
        self._task: asyncio.Task[None] | None = None

    def start(self) -> None:
        self._task = asyncio.create_task(
            self._run_forever(),
            name=f"project-{self.project.name}",
        )

    async def stop(self) -> None:
        if self._orchestrator is not None:
            try:
                await self._orchestrator.stop()
            except Exception:
                log.exception("manager.orchestrator_stop_error", project=self.project.name)
        if self._task and not self._task.done():
            self._task.cancel()
            try:
                await self._task
            except (asyncio.CancelledError, Exception):
                pass

    def get_state(self) -> OrchestratorState | None:
        if self._orchestrator is None:
            return None
        return self._orchestrator.get_state()

    async def _run_forever(self) -> None:
        """Run orchestrator, restart on crash until shutdown_event is set."""
        while not self.shutdown_event.is_set():
            try:
                from symphony.orchestrator import Orchestrator  # noqa: PLC0415

                self._orchestrator = Orchestrator(
                    workflow_path=self.project.workflow,
                    shutdown_event=self.shutdown_event,
                    name=self.project.name,
                )
                log.info("manager.project_starting", project=self.project.name)
                await self._orchestrator.start()
                log.info("manager.project_stopped", project=self.project.name)
            except asyncio.CancelledError:
                break
            except Exception:
                log.exception("manager.project_crashed", project=self.project.name)

            if self.shutdown_event.is_set():
                break

            # Wait before restarting — but respect shutdown
            try:
                await asyncio.wait_for(
                    self.shutdown_event.wait(),
                    timeout=_RESTART_DELAY_S,
                )
                break  # shutdown during wait
            except asyncio.TimeoutError:
                log.info("manager.project_restarting", project=self.project.name)


class DaemonManager:
    """Coordinates multiple Orchestrators; manages signal handling and shutdown."""

    def __init__(self, config: DaemonConfig) -> None:
        self._config = config
        self._shutdown_event = asyncio.Event()
        self._runners: list[_ProjectRunner] = []

    async def run(self) -> None:
        """Start all projects and block until shutdown."""
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(sig, self._request_shutdown)

        self._runners = [
            _ProjectRunner(p, self._shutdown_event)
            for p in self._config.projects
        ]

        for runner in self._runners:
            runner.start()

        log.info("manager.started", projects=[p.name for p in self._config.projects])

        # Block until shutdown
        await self._shutdown_event.wait()

        log.info("manager.shutting_down")
        await asyncio.gather(
            *(r.stop() for r in self._runners),
            return_exceptions=True,
        )
        log.info("manager.stopped")

    def _request_shutdown(self) -> None:
        log.info("manager.shutdown_requested")
        self._shutdown_event.set()

    def request_shutdown(self) -> None:
        """Public API for updater to trigger graceful shutdown."""
        self._shutdown_event.set()

    def get_all_states(self) -> dict[str, OrchestratorState]:
        """Return current state for all projects (keyed by project name)."""
        result: dict[str, OrchestratorState] = {}
        for runner in self._runners:
            state = runner.get_state()
            if state is not None:
                result[runner.project.name] = state
        return result
