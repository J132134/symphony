"""DaemonConfig — parses ~/.config/symphony/config.yaml for multi-project daemon."""
from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml


@dataclass
class ProjectConfig:
    name: str
    workflow: str  # resolved absolute path


@dataclass
class AutoUpdateConfig:
    enabled: bool = True
    interval_minutes: int = 30


@dataclass
class StatusServerConfig:
    enabled: bool = True
    port: int = 7777


@dataclass
class DaemonConfig:
    projects: list[ProjectConfig] = field(default_factory=list)
    auto_update: AutoUpdateConfig = field(default_factory=AutoUpdateConfig)
    status_server: StatusServerConfig = field(default_factory=StatusServerConfig)

    # Path of the config file (for reporting)
    config_path: str = ""

    @classmethod
    def load(cls, path: str | Path | None = None) -> "DaemonConfig":
        """Load config from path, falling back to ~/.config/symphony/config.yaml."""
        if path is None:
            path = Path.home() / ".config" / "symphony" / "config.yaml"
        else:
            path = Path(path).expanduser().resolve()

        with open(path) as f:
            raw: dict[str, Any] = yaml.safe_load(f) or {}

        return cls._from_raw(raw, config_path=str(path))

    @classmethod
    def _from_raw(cls, raw: dict[str, Any], config_path: str = "") -> "DaemonConfig":
        projects = []
        for p in raw.get("projects", []):
            name = str(p.get("name", ""))
            workflow_raw = str(p.get("workflow", ""))
            workflow = cls._resolve_path(workflow_raw)
            projects.append(ProjectConfig(name=name, workflow=workflow))

        au_raw = raw.get("auto_update", {})
        auto_update = AutoUpdateConfig(
            enabled=bool(au_raw.get("enabled", True)),
            interval_minutes=int(au_raw.get("interval_minutes", 30)),
        )

        ss_raw = raw.get("status_server", {})
        status_server = StatusServerConfig(
            enabled=bool(ss_raw.get("enabled", True)),
            port=int(ss_raw.get("port", 7777)),
        )

        return cls(
            projects=projects,
            auto_update=auto_update,
            status_server=status_server,
            config_path=config_path,
        )

    @staticmethod
    def _resolve_path(value: str) -> str:
        """Resolve $VAR env substitution and ~ expansion."""
        if value.startswith("$"):
            env_name = value[1:]
            value = os.environ.get(env_name, value)
        return str(Path(value).expanduser().resolve())

    def validate(self) -> list[str]:
        """Return list of validation errors, empty if valid."""
        errors: list[str] = []

        if not self.projects:
            errors.append("No projects configured")
            return errors

        names = [p.name for p in self.projects]
        if len(names) != len(set(names)):
            errors.append("Project names must be unique")

        workspace_roots: dict[str, str] = {}
        for p in self.projects:
            if not p.name:
                errors.append("Each project must have a name")
            if not p.workflow:
                errors.append(f"Project '{p.name}': workflow path is required")
            elif not Path(p.workflow).exists():
                errors.append(f"Project '{p.name}': workflow not found: {p.workflow}")
            # Warn about workspace root collisions (checked externally via DaemonManager)

        return errors
