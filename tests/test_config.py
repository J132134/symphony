from __future__ import annotations

import os

import pytest

from symphony.config import SymphonyConfig


def _base_raw(**overrides):
    raw = {
        "tracker": {
            "kind": "linear",
            "api_key": "lin_test_key",
            "project_slug": "test-proj",
            "active_states": ["In Progress", "Todo"],
            "terminal_states": ["Done", "Cancelled"],
        },
        "codex": {"command": "codex"},
    }
    for k, v in overrides.items():
        parts = k.split(".")
        node = raw
        for p in parts[:-1]:
            node = node.setdefault(p, {})
        node[parts[-1]] = v
    return raw


class TestDefaults:
    def test_poll_interval_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.poll_interval_ms == 10000

    def test_workspace_root_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.workspace_root.endswith("/.symphony/workspaces")

    def test_hooks_timeout_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.hooks_timeout_ms == 60000

    def test_codex_command_default(self):
        cfg = SymphonyConfig({"tracker": {"kind": "linear", "api_key": "k", "project_slug": "p"}})
        assert cfg.codex_command == "codex app-server"

    def test_approval_policy_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.approval_policy == "auto-edit"

    def test_turn_timeout_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.turn_timeout_ms == 3600000

    def test_read_timeout_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.read_timeout_ms == 5000

    def test_stall_timeout_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.stall_timeout_ms == 300000

    def test_max_concurrent_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.max_concurrent_agents == 10

    def test_max_turns_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.max_turns == 3

    def test_max_retry_backoff_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.max_retry_backoff_ms == 300000

    def test_server_port_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.server_port == 7777

    def test_tracker_endpoint_default(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.tracker_endpoint == "https://api.linear.app/graphql"


class TestEnvIndirection:
    def test_resolve_env_var(self, monkeypatch):
        monkeypatch.setenv("MY_LINEAR_KEY", "resolved_key")
        cfg = SymphonyConfig(_base_raw(**{"tracker.api_key": "$MY_LINEAR_KEY"}))
        assert cfg.tracker_api_key == "resolved_key"

    def test_resolve_missing_env_var(self, monkeypatch):
        monkeypatch.delenv("NONEXISTENT_VAR", raising=False)
        cfg = SymphonyConfig(_base_raw(**{"tracker.api_key": "$NONEXISTENT_VAR"}))
        assert cfg.tracker_api_key == ""

    def test_literal_value_not_resolved(self):
        cfg = SymphonyConfig(_base_raw(**{"tracker.api_key": "literal_key"}))
        assert cfg.tracker_api_key == "literal_key"


class TestPathExpansion:
    def test_tilde_expansion(self):
        cfg = SymphonyConfig(_base_raw(**{"workspace.root": "~/my_workspaces"}))
        assert not cfg.workspace_root.startswith("~")
        assert cfg.workspace_root.endswith("/my_workspaces")

    def test_env_var_in_path(self, monkeypatch):
        monkeypatch.setenv("WORK_DIR", "/tmp/symphony")
        cfg = SymphonyConfig(_base_raw(**{"workspace.root": "$WORK_DIR"}))
        assert cfg.workspace_root == "/tmp/symphony"


class TestNestedAccess:
    def test_dotted_key(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg._get("tracker.kind") == "linear"

    def test_missing_nested_key(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg._get("tracker.nonexistent", "fallback") == "fallback"

    def test_missing_top_level(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg._get("nonexistent.key", 42) == 42


class TestValidation:
    def test_valid_config(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.validate() == []

    def test_wrong_tracker_kind(self):
        cfg = SymphonyConfig(_base_raw(**{"tracker.kind": "jira"}))
        errors = cfg.validate()
        assert any("tracker.kind" in e for e in errors)

    def test_missing_api_key(self):
        cfg = SymphonyConfig(_base_raw(**{"tracker.api_key": ""}))
        errors = cfg.validate()
        assert any("api_key" in e for e in errors)

    def test_missing_project_slug(self):
        cfg = SymphonyConfig(_base_raw(**{"tracker.project_slug": ""}))
        errors = cfg.validate()
        assert any("project_slug" in e for e in errors)

    def test_empty_codex_command(self):
        cfg = SymphonyConfig(_base_raw(**{"codex.command": ""}))
        errors = cfg.validate()
        assert any("codex.command" in e for e in errors)

    def test_multiple_errors(self):
        raw = {"tracker": {"kind": "jira"}}
        cfg = SymphonyConfig(raw)
        errors = cfg.validate()
        assert len(errors) >= 3


class TestCollections:
    def test_active_states(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.active_states == ["In Progress", "Todo"]

    def test_terminal_states(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.terminal_states == ["Done", "Cancelled"]

    def test_empty_states(self):
        cfg = SymphonyConfig({"tracker": {"kind": "linear", "api_key": "k", "project_slug": "p"}})
        assert cfg.active_states == ["Todo", "In Progress"]
        assert cfg.terminal_states == ["Closed", "Cancelled", "Canceled", "Duplicate", "Done"]

    def test_hooks(self):
        cfg = SymphonyConfig(_base_raw(**{
            "hooks.after_create": "git init",
            "hooks.before_run": "npm install",
        }))
        hooks = cfg.hooks
        assert hooks["after_create"] == "git init"
        assert hooks["before_run"] == "npm install"

    def test_hooks_excludes_timeout_ms(self):
        cfg = SymphonyConfig(_base_raw(**{
            "hooks.timeout_ms": 5000,
            "hooks.after_create": "echo hi",
        }))
        assert "timeout_ms" not in cfg.hooks

    def test_max_concurrent_by_state(self):
        cfg = SymphonyConfig(_base_raw(**{
            "agent.max_concurrent_agents_by_state": {"Todo": 1, "In Progress": 2},
        }))
        by_state = cfg.max_concurrent_agents_by_state
        assert by_state == {"todo": 1, "in progress": 2}

    def test_optional_codex_fields(self):
        cfg = SymphonyConfig(_base_raw())
        assert cfg.turn_sandbox_policy is None
        assert cfg.thread_sandbox is None

    def test_codex_optional_set(self):
        cfg = SymphonyConfig(_base_raw(**{
            "codex.turn_sandbox_policy": "read-only",
            "codex.thread_sandbox": "workspace-write",
        }))
        assert cfg.turn_sandbox_policy == "read-only"
        assert cfg.thread_sandbox == "workspace-write"

    def test_turn_sandbox_policy_object(self):
        cfg = SymphonyConfig(_base_raw(**{
            "codex.turn_sandbox_policy": {
                "type": "workspace-write",
                "network_access": True,
                "writable_roots": ["/tmp/shared-git-dir"],
            },
        }))
        assert cfg.turn_sandbox_policy == {
            "type": "workspace-write",
            "network_access": True,
            "writable_roots": ["/tmp/shared-git-dir"],
        }
        assert cfg.thread_sandbox is None

    def test_thread_sandbox_derived_from_object_type(self):
        cfg = SymphonyConfig(_base_raw(**{
            "codex.thread_sandbox": {"type": "workspace-write"},
        }))
        assert cfg.thread_sandbox == "workspace-write"
