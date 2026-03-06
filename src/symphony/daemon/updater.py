"""Auto-update support for symphony daemon.

Supports two modes:
- uv tool install: runs `uv tool upgrade symphony`, detects via binary mtime change
- git dev mode: git fetch + pull --ff-only + uv sync, detects via HEAD hash change

On update: gracefully stop all orchestrators → sys.exit(42) for launchd restart.
"""
from __future__ import annotations

import shutil
import subprocess
import sys
from pathlib import Path
from typing import TYPE_CHECKING

import structlog

if TYPE_CHECKING:
    from symphony.daemon.manager import DaemonManager

logger = structlog.get_logger(__name__)

RESTART_EXIT_CODE = 42


def _is_uv_tool_install() -> bool:
    """Check if running from uv tool install (not dev/git environment)."""
    try:
        import importlib.util
        spec = importlib.util.find_spec("symphony")
        if spec is None:
            return False
        src_path = str(
            spec.submodule_search_locations[0]
            if spec.submodule_search_locations
            else ""
        )
        uv_tools_markers = [".local/share/uv/tools", ".uv/tools", "uv/tools"]
        return any(marker in src_path for marker in uv_tools_markers)
    except Exception:
        return False


def _get_binary_mtime() -> float | None:
    binary = shutil.which("symphony")
    if not binary:
        return None
    try:
        return Path(binary).stat().st_mtime
    except Exception:
        return None


def _try_uv_tool_upgrade() -> bool:
    """Run uv tool upgrade symphony; return True if binary was updated."""
    try:
        before = _get_binary_mtime()
        result = subprocess.run(
            ["uv", "tool", "upgrade", "symphony"],
            capture_output=True,
            text=True,
            timeout=120,
        )
        logger.info(
            "uv_tool_upgrade",
            returncode=result.returncode,
            output=(result.stdout + result.stderr)[:200],
        )
        if result.returncode != 0:
            return False
        after = _get_binary_mtime()
        return after is not None and after != before
    except Exception as e:
        logger.warning("uv_tool_upgrade_failed", error=str(e))
        return False


def get_git_hash(repo_dir: str | Path | None = None) -> str | None:
    """Return current git HEAD hash, or None on failure."""
    cwd = str(repo_dir) if repo_dir else None
    try:
        result = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            capture_output=True,
            text=True,
            timeout=10,
            cwd=cwd,
        )
        return result.stdout.strip() if result.returncode == 0 else None
    except Exception:
        return None


def _try_git_update(repo_dir: str | Path | None = None) -> bool:
    """Run git pull --ff-only + uv sync; return True if updated."""
    cwd = str(repo_dir) if repo_dir else None
    try:
        before = get_git_hash(repo_dir)

        fetch = subprocess.run(
            ["git", "fetch", "--quiet"],
            capture_output=True,
            text=True,
            timeout=30,
            cwd=cwd,
        )
        if fetch.returncode != 0:
            logger.warning("git_fetch_failed", stderr=fetch.stderr)
            return False

        pull = subprocess.run(
            ["git", "pull", "--ff-only"],
            capture_output=True,
            text=True,
            timeout=30,
            cwd=cwd,
        )
        if pull.returncode != 0:
            logger.warning("git_pull_failed", stderr=pull.stderr)
            return False

        after = get_git_hash(repo_dir)
        if before == after:
            return False

        subprocess.run(
            ["uv", "sync"],
            capture_output=True,
            text=True,
            timeout=120,
            cwd=cwd,
        )
        logger.info("git_updated", before=before, after=after)
        return True
    except Exception as e:
        logger.warning("git_update_failed", error=str(e))
        return False


def check_for_updates(manager: "DaemonManager", repo_dir: str | Path | None = None) -> None:
    """Check for updates and restart daemon if updated. Called by scheduler/timer."""
    try:
        if _is_uv_tool_install():
            updated = _try_uv_tool_upgrade()
        else:
            updated = _try_git_update(repo_dir)

        if not updated:
            return

        version_after = get_git_hash(repo_dir)
        logger.info("daemon_restarting_for_update", version=version_after, exit_code=RESTART_EXIT_CODE)
        manager.request_shutdown()
        sys.exit(RESTART_EXIT_CODE)
    except SystemExit:
        raise
    except Exception as e:
        logger.error("auto_update_error", error=str(e))
