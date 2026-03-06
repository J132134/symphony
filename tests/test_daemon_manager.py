"""Tests for daemon/manager.py — DaemonManager."""
from __future__ import annotations

import asyncio
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from symphony.daemon.config import DaemonConfig, ProjectConfig, AutoUpdateConfig, StatusServerConfig
from symphony.daemon.manager import DaemonManager, _ProjectRunner


def _make_config(names: list[str], tmp_path: Path) -> DaemonConfig:
    projects = []
    for name in names:
        wf = tmp_path / f"{name}.md"
        wf.touch()
        projects.append(ProjectConfig(name=name, workflow=str(wf)))
    return DaemonConfig(
        projects=projects,
        auto_update=AutoUpdateConfig(enabled=False),
        status_server=StatusServerConfig(enabled=False),
    )


class TestDaemonManager:
    async def test_get_all_states_empty(self, tmp_path):
        cfg = _make_config(["p1"], tmp_path)
        manager = DaemonManager(cfg)
        # No runners started yet
        assert manager.get_all_states() == {}

    async def test_request_shutdown_sets_event(self, tmp_path):
        cfg = _make_config(["p1"], tmp_path)
        manager = DaemonManager(cfg)
        assert not manager._shutdown_event.is_set()
        manager.request_shutdown()
        assert manager._shutdown_event.is_set()

    async def test_run_starts_and_stops(self, tmp_path):
        cfg = _make_config(["p1", "p2"], tmp_path)
        manager = DaemonManager(cfg)

        mock_orch = AsyncMock()
        mock_orch.get_state.return_value = MagicMock(
            running={}, retry_queue={}, completed_count=0,
            codex_totals=MagicMock(total_tokens=0),
        )

        with patch("symphony.daemon.manager._ProjectRunner._run_forever", new_callable=AsyncMock) as mock_run:
            async def stop_after_start():
                manager.request_shutdown()

            # Patch _run_forever to immediately signal shutdown
            async def fake_run(self_runner):
                manager.request_shutdown()

            with patch.object(_ProjectRunner, "_run_forever", fake_run):
                await asyncio.wait_for(manager.run(), timeout=2.0)

        # Shutdown event should be set
        assert manager._shutdown_event.is_set()

    async def test_get_all_states_with_runners(self, tmp_path):
        cfg = _make_config(["alpha"], tmp_path)
        manager = DaemonManager(cfg)

        # Manually create a runner with a mock orchestrator
        from symphony.daemon.manager import _ProjectRunner
        runner = _ProjectRunner(cfg.projects[0], manager._shutdown_event)
        mock_state = MagicMock()
        runner._orchestrator = MagicMock()
        runner._orchestrator.get_state.return_value = mock_state
        manager._runners = [runner]

        states = manager.get_all_states()
        assert "alpha" in states
        assert states["alpha"] is mock_state
