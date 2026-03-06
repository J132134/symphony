from __future__ import annotations

import asyncio
import os
import re
import shutil
from pathlib import Path

import structlog

from symphony.models import Workspace

log = structlog.get_logger()


class WorkspaceError(Exception):
    def __init__(self, code: str, message: str) -> None:
        self.code = code
        super().__init__(message)


class WorkspaceManager:
    def __init__(
        self,
        root: str,
        hooks: dict[str, str] | None = None,
        hooks_timeout_ms: int = 30000,
    ) -> None:
        self._root = Path(root).resolve()
        self._hooks = hooks or {}
        self._hooks_timeout_ms = hooks_timeout_ms

    async def ensure_root(self) -> None:
        """Create workspace root directory if it doesn't exist."""
        self._root.mkdir(parents=True, exist_ok=True)

    def create_or_reuse(self, identifier: str) -> Workspace:
        """Create or reuse workspace for issue identifier."""
        key = self.sanitize_identifier(identifier)
        ws_path = (self._root / key).resolve()
        self._validate_path_containment(ws_path)

        created_now = not ws_path.exists()
        ws_path.mkdir(parents=True, exist_ok=True)

        return Workspace(
            path=str(ws_path),
            workspace_key=key,
            created_now=created_now,
        )

    async def setup_workspace(self, identifier: str) -> Workspace:
        """Create/reuse workspace and run after_create hook if newly created."""
        await self.ensure_root()
        ws = self.create_or_reuse(identifier)

        if ws.created_now and "after_create" in self._hooks:
            log.info("workspace.hook.after_create", workspace=ws.path)
            try:
                await self.run_hook("after_create", ws.path)
            except WorkspaceError:
                # Fatal — clean up the newly created directory
                shutil.rmtree(ws.path, ignore_errors=True)
                raise

        return ws

    async def prepare_for_run(self, workspace: Workspace) -> None:
        """Run before_run hook if configured. Failure is fatal."""
        if "before_run" in self._hooks:
            log.info("workspace.hook.before_run", workspace=workspace.path)
            await self.run_hook("before_run", workspace.path)

    async def finish_run(self, workspace: Workspace) -> None:
        """Run after_run hook if configured. Failure is logged but not fatal."""
        if "after_run" in self._hooks:
            log.info("workspace.hook.after_run", workspace=workspace.path)
            try:
                await self.run_hook("after_run", workspace.path)
            except WorkspaceError as exc:
                log.warning(
                    "workspace.hook.after_run.failed",
                    workspace=workspace.path,
                    error=str(exc),
                )

    async def cleanup(self, workspace: Workspace) -> None:
        """Remove workspace directory."""
        if "before_remove" in self._hooks:
            log.info("workspace.hook.before_remove", workspace=workspace.path)
            try:
                await self.run_hook("before_remove", workspace.path)
            except WorkspaceError as exc:
                log.warning(
                    "workspace.hook.before_remove.failed",
                    workspace=workspace.path,
                    error=str(exc),
                )

        loop = asyncio.get_running_loop()
        await loop.run_in_executor(None, shutil.rmtree, workspace.path)
        log.info("workspace.cleaned_up", workspace=workspace.path)

    async def run_hook(self, name: str, workspace_path: str) -> None:
        """Run a hook script via bash."""
        script = self._hooks[name]
        env = {**os.environ, "SYMPHONY_WORKSPACE": workspace_path}
        timeout_s = self._hooks_timeout_ms / 1000

        try:
            proc = await asyncio.create_subprocess_exec(
                "bash",
                "-lc",
                script,
                cwd=workspace_path,
                env=env,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
            )
            _, stderr = await asyncio.wait_for(
                proc.communicate(), timeout=timeout_s
            )
        except asyncio.TimeoutError:
            proc.kill()  # type: ignore[union-attr]
            raise WorkspaceError(
                "hook_timeout",
                f"Hook '{name}' timed out after {self._hooks_timeout_ms}ms",
            )

        if proc.returncode != 0:
            raise WorkspaceError(
                "hook_failed",
                f"Hook '{name}' exited with code {proc.returncode}: "
                f"{stderr.decode().strip()}",
            )

    @staticmethod
    def sanitize_identifier(identifier: str) -> str:
        """Replace characters not in [A-Za-z0-9._-] with underscore."""
        return re.sub(r"[^A-Za-z0-9._\-]", "_", identifier)

    def _validate_path_containment(self, workspace_path: Path) -> None:
        """Ensure workspace_path is under workspace root."""
        resolved = workspace_path.resolve()
        root = self._root.resolve()
        if not (resolved == root or str(resolved).startswith(str(root) + os.sep)):
            raise WorkspaceError(
                "path_containment_violation",
                f"Path {resolved} is not under workspace root {root}",
            )
