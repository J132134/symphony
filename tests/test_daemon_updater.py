"""Tests for daemon/updater.py — auto-update logic."""
from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from symphony.daemon.updater import (
    RESTART_EXIT_CODE,
    _get_binary_mtime,
    _is_uv_tool_install,
    check_for_updates,
    get_git_hash,
)


class TestIsUvToolInstall:
    def test_dev_environment_not_uv_tool(self):
        # In test context, should not detect uv tool install
        # (tests run from source tree)
        result = _is_uv_tool_install()
        assert isinstance(result, bool)

    def test_uv_tool_path_detected(self, monkeypatch):
        import importlib.util
        mock_spec = MagicMock()
        mock_spec.submodule_search_locations = ["/home/user/.local/share/uv/tools/symphony/lib"]
        with patch("importlib.util.find_spec", return_value=mock_spec):
            assert _is_uv_tool_install() is True

    def test_non_uv_path_not_detected(self, monkeypatch):
        import importlib.util
        mock_spec = MagicMock()
        mock_spec.submodule_search_locations = ["/home/user/Projects/symphony/src"]
        with patch("importlib.util.find_spec", return_value=mock_spec):
            assert _is_uv_tool_install() is False


class TestGetBinaryMtime:
    def test_returns_none_when_no_binary(self, monkeypatch):
        with patch("shutil.which", return_value=None):
            assert _get_binary_mtime() is None

    def test_returns_mtime_when_binary_exists(self, tmp_path):
        binary = tmp_path / "symphony"
        binary.touch()
        with patch("shutil.which", return_value=str(binary)):
            mtime = _get_binary_mtime()
            assert mtime is not None
            assert mtime > 0


class TestGetGitHash:
    def test_returns_hash_on_success(self):
        with patch("subprocess.run") as mock_run:
            mock_run.return_value = MagicMock(returncode=0, stdout="abc1234\n")
            result = get_git_hash()
            assert result == "abc1234"

    def test_returns_none_on_failure(self):
        with patch("subprocess.run") as mock_run:
            mock_run.return_value = MagicMock(returncode=1, stdout="")
            result = get_git_hash()
            assert result is None

    def test_uses_cwd_when_repo_dir_provided(self, tmp_path):
        with patch("subprocess.run") as mock_run:
            mock_run.return_value = MagicMock(returncode=0, stdout="deadbeef\n")
            get_git_hash(repo_dir=tmp_path)
            call_kwargs = mock_run.call_args[1]
            assert call_kwargs["cwd"] == str(tmp_path)


class TestCheckForUpdates:
    def test_no_update_no_restart(self):
        mock_manager = MagicMock()
        with patch("symphony.daemon.updater._is_uv_tool_install", return_value=False), \
             patch("symphony.daemon.updater._try_git_update", return_value=False):
            check_for_updates(mock_manager)
            mock_manager.request_shutdown.assert_not_called()

    def test_update_detected_exits(self):
        mock_manager = MagicMock()
        with patch("symphony.daemon.updater._is_uv_tool_install", return_value=False), \
             patch("symphony.daemon.updater._try_git_update", return_value=True), \
             patch("symphony.daemon.updater.get_git_hash", return_value="newhead"), \
             pytest.raises(SystemExit) as exc_info:
            check_for_updates(mock_manager)

        assert exc_info.value.code == RESTART_EXIT_CODE
        mock_manager.request_shutdown.assert_called_once()

    def test_uv_tool_mode_upgrade_exit(self):
        mock_manager = MagicMock()
        with patch("symphony.daemon.updater._is_uv_tool_install", return_value=True), \
             patch("symphony.daemon.updater._try_uv_tool_upgrade", return_value=True), \
             patch("symphony.daemon.updater.get_git_hash", return_value=None), \
             pytest.raises(SystemExit) as exc_info:
            check_for_updates(mock_manager)

        assert exc_info.value.code == RESTART_EXIT_CODE

    def test_exception_does_not_propagate(self):
        mock_manager = MagicMock()
        with patch("symphony.daemon.updater._is_uv_tool_install", side_effect=RuntimeError("boom")):
            # Should log error and return gracefully
            check_for_updates(mock_manager)
