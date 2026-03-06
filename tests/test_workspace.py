from __future__ import annotations

import asyncio

import pytest

from symphony.models import Workspace
from symphony.workspace import WorkspaceError, WorkspaceManager


class TestSanitizeIdentifier:
    def test_clean_identifier(self):
        assert WorkspaceManager.sanitize_identifier("ENG-123") == "ENG-123"

    def test_slashes_replaced(self):
        assert WorkspaceManager.sanitize_identifier("feat/ENG-123") == "feat_ENG-123"

    def test_spaces_replaced(self):
        assert WorkspaceManager.sanitize_identifier("ENG 123") == "ENG_123"

    def test_special_chars(self):
        assert WorkspaceManager.sanitize_identifier("ENG@123#test!") == "ENG_123_test_"

    def test_dots_and_underscores_preserved(self):
        assert WorkspaceManager.sanitize_identifier("ENG_123.fix") == "ENG_123.fix"

    def test_unicode_replaced(self):
        result = WorkspaceManager.sanitize_identifier("이슈-1")
        # Korean chars replaced with _, hyphen and digit preserved
        assert "-1" in result
        assert "이" not in result

    def test_empty_string(self):
        assert WorkspaceManager.sanitize_identifier("") == ""


class TestCreateOrReuse:
    def test_creates_new_workspace(self, tmp_path):
        mgr = WorkspaceManager(root=str(tmp_path))
        ws = mgr.create_or_reuse("ENG-1")
        assert ws.created_now is True
        assert ws.workspace_key == "ENG-1"
        assert (tmp_path / "ENG-1").is_dir()

    def test_reuses_existing_workspace(self, tmp_path):
        (tmp_path / "ENG-1").mkdir()
        mgr = WorkspaceManager(root=str(tmp_path))
        ws = mgr.create_or_reuse("ENG-1")
        assert ws.created_now is False

    def test_sanitizes_identifier(self, tmp_path):
        mgr = WorkspaceManager(root=str(tmp_path))
        ws = mgr.create_or_reuse("feat/ENG-1")
        assert ws.workspace_key == "feat_ENG-1"
        assert (tmp_path / "feat_ENG-1").is_dir()

    def test_path_containment_sanitizer_prevents_traversal(self, tmp_path):
        """Sanitizer replaces / with _, so traversal is prevented at sanitization level."""
        mgr = WorkspaceManager(root=str(tmp_path))
        ws = mgr.create_or_reuse("../../etc/passwd")
        # Slashes sanitized to _, dots preserved but path stays under root
        assert ws.workspace_key == ".._.._etc_passwd"
        assert str(tmp_path) in ws.path

    def test_path_containment_validation(self, tmp_path):
        """Direct call to _validate_path_containment catches escapes."""
        mgr = WorkspaceManager(root=str(tmp_path))
        import os
        escape_path = (tmp_path / ".." / "escape").resolve()
        with pytest.raises(WorkspaceError) as exc_info:
            mgr._validate_path_containment(escape_path)
        assert exc_info.value.code == "path_containment_violation"

    def test_workspace_path_is_absolute(self, tmp_path):
        mgr = WorkspaceManager(root=str(tmp_path))
        ws = mgr.create_or_reuse("ENG-1")
        assert ws.path.startswith("/")


class TestSetupWorkspace:
    @pytest.mark.asyncio
    async def test_setup_creates_root(self, tmp_path):
        root = tmp_path / "workspaces"
        mgr = WorkspaceManager(root=str(root))
        ws = await mgr.setup_workspace("ENG-1")
        assert root.is_dir()
        assert (root / "ENG-1").is_dir()

    @pytest.mark.asyncio
    async def test_after_create_hook_runs(self, tmp_path):
        marker = tmp_path / "hook_ran"
        mgr = WorkspaceManager(
            root=str(tmp_path / "ws"),
            hooks={"after_create": f"touch {marker}"},
        )
        await mgr.setup_workspace("ENG-1")
        assert marker.exists()

    @pytest.mark.asyncio
    async def test_after_create_hook_not_on_reuse(self, tmp_path):
        root = tmp_path / "ws"
        root.mkdir()
        (root / "ENG-1").mkdir()
        marker = tmp_path / "hook_ran"
        mgr = WorkspaceManager(
            root=str(root),
            hooks={"after_create": f"touch {marker}"},
        )
        await mgr.setup_workspace("ENG-1")
        assert not marker.exists()

    @pytest.mark.asyncio
    async def test_after_create_hook_failure_cleans_up(self, tmp_path):
        mgr = WorkspaceManager(
            root=str(tmp_path / "ws"),
            hooks={"after_create": "exit 1"},
        )
        with pytest.raises(WorkspaceError):
            await mgr.setup_workspace("ENG-1")
        assert not (tmp_path / "ws" / "ENG-1").exists()


class TestHooks:
    @pytest.mark.asyncio
    async def test_before_run_fatal(self, tmp_path):
        (tmp_path / "ENG-1").mkdir(parents=True)
        mgr = WorkspaceManager(
            root=str(tmp_path),
            hooks={"before_run": "exit 1"},
        )
        ws = Workspace(path=str(tmp_path / "ENG-1"), workspace_key="ENG-1", created_now=False)
        with pytest.raises(WorkspaceError) as exc_info:
            await mgr.prepare_for_run(ws)
        assert exc_info.value.code == "hook_failed"

    @pytest.mark.asyncio
    async def test_after_run_non_fatal(self, tmp_path):
        (tmp_path / "ENG-1").mkdir(parents=True)
        mgr = WorkspaceManager(
            root=str(tmp_path),
            hooks={"after_run": "exit 1"},
        )
        ws = Workspace(path=str(tmp_path / "ENG-1"), workspace_key="ENG-1", created_now=False)
        # Should not raise
        await mgr.finish_run(ws)

    @pytest.mark.asyncio
    async def test_hook_timeout(self, tmp_path):
        (tmp_path / "ENG-1").mkdir(parents=True)
        mgr = WorkspaceManager(
            root=str(tmp_path),
            hooks={"before_run": "sleep 10"},
            hooks_timeout_ms=500,
        )
        ws = Workspace(path=str(tmp_path / "ENG-1"), workspace_key="ENG-1", created_now=False)
        with pytest.raises(WorkspaceError) as exc_info:
            await mgr.prepare_for_run(ws)
        assert exc_info.value.code == "hook_timeout"

    @pytest.mark.asyncio
    async def test_hook_receives_env_var(self, tmp_path):
        (tmp_path / "ENG-1").mkdir(parents=True)
        marker = tmp_path / "env_check"
        mgr = WorkspaceManager(
            root=str(tmp_path),
            hooks={"before_run": f'echo "$SYMPHONY_WORKSPACE" > {marker}'},
        )
        ws = Workspace(path=str(tmp_path / "ENG-1"), workspace_key="ENG-1", created_now=False)
        await mgr.prepare_for_run(ws)
        content = marker.read_text().strip()
        assert content == str(tmp_path / "ENG-1")


class TestCleanup:
    @pytest.mark.asyncio
    async def test_removes_directory(self, tmp_path):
        ws_dir = tmp_path / "ENG-1"
        ws_dir.mkdir()
        (ws_dir / "file.txt").write_text("data")
        mgr = WorkspaceManager(root=str(tmp_path))
        ws = Workspace(path=str(ws_dir), workspace_key="ENG-1", created_now=False)
        await mgr.cleanup(ws)
        assert not ws_dir.exists()

    @pytest.mark.asyncio
    async def test_before_remove_hook_non_fatal(self, tmp_path):
        ws_dir = tmp_path / "ENG-1"
        ws_dir.mkdir()
        mgr = WorkspaceManager(
            root=str(tmp_path),
            hooks={"before_remove": "exit 1"},
        )
        ws = Workspace(path=str(ws_dir), workspace_key="ENG-1", created_now=False)
        await mgr.cleanup(ws)
        assert not ws_dir.exists()
