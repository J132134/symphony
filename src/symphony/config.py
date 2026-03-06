from __future__ import annotations

import os
from pathlib import Path
from typing import Any


def _parse_state_list(val: Any) -> list[str]:
    """Parse active/terminal states — accepts list or comma-separated string."""
    if isinstance(val, list):
        return [str(s).strip() for s in val if str(s).strip()]
    if isinstance(val, str):
        return [s.strip() for s in val.split(",") if s.strip()]
    return []


def normalize_state(state: str) -> str:
    """Normalize a state name: trim + lowercase (spec §4.2)."""
    return state.strip().lower()


class SymphonyConfig:
    """Typed config from WORKFLOW.md YAML front matter.

    Supports ``$VAR`` env-var indirection and ``~`` home expansion for paths.
    Nested access via dot-notation: ``config._get("tracker.api_key")``.
    """

    def __init__(self, raw: dict[str, Any]) -> None:
        self._raw = raw

    # -- internal helpers ------------------------------------------------

    def _resolve_env(self, value: str) -> str:
        """If *value* starts with ``$``, resolve from ``os.environ``."""
        if isinstance(value, str) and value.startswith("$"):
            env_name = value[1:]
            return os.environ.get(env_name, "")
        return value

    def _expand_path(self, value: str) -> str:
        """Expand ``~`` and resolve env vars in paths."""
        resolved = self._resolve_env(value)
        return str(Path(resolved).expanduser())

    def _get(self, dotted_key: str, default: Any = None) -> Any:
        """Nested dict access: ``'tracker.api_key'`` -> ``raw['tracker']['api_key']``."""
        keys = dotted_key.split(".")
        node: Any = self._raw
        for key in keys:
            if not isinstance(node, dict):
                return default
            node = node.get(key)
            if node is None:
                return default
        if isinstance(node, str):
            return self._resolve_env(node)
        return node

    # -- tracker ---------------------------------------------------------

    @property
    def tracker_kind(self) -> str:
        return str(self._get("tracker.kind", "linear"))

    @property
    def tracker_api_key(self) -> str:
        return str(self._get("tracker.api_key", ""))

    @property
    def tracker_project_slug(self) -> str:
        return str(self._get("tracker.project_slug", ""))

    @property
    def tracker_endpoint(self) -> str:
        return str(self._get("tracker.endpoint", "https://api.linear.app/graphql"))

    @property
    def active_states(self) -> list[str]:
        val = self._get("tracker.active_states", "Todo, In Progress")
        return _parse_state_list(val)

    @property
    def terminal_states(self) -> list[str]:
        val = self._get("tracker.terminal_states", "Closed, Cancelled, Canceled, Duplicate, Done")
        return _parse_state_list(val)

    # -- polling ---------------------------------------------------------

    @property
    def poll_interval_ms(self) -> int:
        return int(self._get("polling.interval_ms", 10000))

    @property
    def poll_interval_idle_ms(self) -> int:
        return int(self._get("polling.idle_interval_ms", 60000))

    # -- workspace -------------------------------------------------------

    @property
    def workspace_root(self) -> str:
        return self._expand_path(
            str(self._get("workspace.root", "~/.symphony/workspaces"))
        )

    # -- hooks -----------------------------------------------------------

    @property
    def hooks(self) -> dict[str, str]:
        val = self._get("hooks")
        if isinstance(val, dict):
            # Filter to known hook names, resolve env in values
            result: dict[str, str] = {}
            for k, v in val.items():
                if k == "timeout_ms":
                    continue
                result[str(k)] = self._resolve_env(str(v))
            return result
        return {}

    @property
    def hooks_timeout_ms(self) -> int:
        return int(self._get("hooks.timeout_ms", 60000))

    # -- agent -----------------------------------------------------------

    @property
    def max_concurrent_agents(self) -> int:
        return int(self._get("agent.max_concurrent_agents", 10))

    @property
    def max_turns(self) -> int:
        return int(self._get("agent.max_turns", 3))

    @property
    def max_retry_backoff_ms(self) -> int:
        return int(self._get("agent.max_retry_backoff_ms", 300000))

    @property
    def max_concurrent_agents_by_state(self) -> dict[str, int]:
        val = self._get("agent.max_concurrent_agents_by_state")
        if isinstance(val, dict):
            result: dict[str, int] = {}
            for k, v in val.items():
                try:
                    iv = int(v)
                    if iv > 0:  # spec: ignore non-positive or non-numeric entries
                        result[str(k).strip().lower()] = iv  # normalize key
                except (ValueError, TypeError):
                    pass
            return result
        return {}

    # -- codex -----------------------------------------------------------

    @property
    def codex_command(self) -> str:
        return str(self._get("codex.command", "codex app-server"))

    @property
    def approval_policy(self) -> str:
        return str(self._get("codex.approval_policy", "auto-edit"))

    @property
    def turn_timeout_ms(self) -> int:
        return int(self._get("codex.turn_timeout_ms", 3600000))

    @property
    def read_timeout_ms(self) -> int:
        return int(self._get("codex.read_timeout_ms", 5000))

    @property
    def stall_timeout_ms(self) -> int:
        return int(self._get("codex.stall_timeout_ms", 300000))

    @property
    def turn_sandbox_policy(self) -> str | dict[str, Any] | None:
        val = self._get("codex.turn_sandbox_policy")
        if isinstance(val, (str, dict)):
            return val
        return None

    @property
    def thread_sandbox(self) -> str | None:
        val = self._get("codex.thread_sandbox")
        if isinstance(val, str):
            return val
        if isinstance(val, dict):
            sandbox_type = val.get("type")
            if sandbox_type is not None:
                return str(sandbox_type)
        return None

    # -- server ----------------------------------------------------------

    @property
    def server_port(self) -> int:
        return int(self._get("server.port", 7777))

    # -- validation ------------------------------------------------------

    def validate(self) -> list[str]:
        """Return list of validation errors, empty if valid."""
        errors: list[str] = []

        if self.tracker_kind != "linear":
            errors.append(f"tracker.kind must be 'linear', got '{self.tracker_kind}'")

        if not self.tracker_api_key:
            errors.append("tracker.api_key is required")

        if not self.tracker_project_slug:
            errors.append("tracker.project_slug is required")

        if not self.codex_command:
            errors.append("codex.command must be non-empty")

        return errors
