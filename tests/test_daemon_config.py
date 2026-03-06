"""Tests for daemon/config.py — DaemonConfig."""
from __future__ import annotations

import os
import textwrap
from pathlib import Path

import pytest
import yaml

from symphony.daemon.config import DaemonConfig, ProjectConfig


def _write_config(tmp_path: Path, content: str) -> Path:
    p = tmp_path / "config.yaml"
    p.write_text(textwrap.dedent(content))
    return p


class TestDaemonConfigLoad:
    def test_load_minimal(self, tmp_path):
        wf = tmp_path / "WORKFLOW.md"
        wf.touch()
        cfg_path = _write_config(tmp_path, f"""
            projects:
              - name: proj1
                workflow: {wf}
        """)
        cfg = DaemonConfig.load(cfg_path)
        assert len(cfg.projects) == 1
        assert cfg.projects[0].name == "proj1"
        assert cfg.projects[0].workflow == str(wf)

    def test_defaults(self, tmp_path):
        wf = tmp_path / "WORKFLOW.md"
        wf.touch()
        cfg_path = _write_config(tmp_path, f"""
            projects:
              - name: p
                workflow: {wf}
        """)
        cfg = DaemonConfig.load(cfg_path)
        assert cfg.auto_update.enabled is True
        assert cfg.auto_update.interval_minutes == 30
        assert cfg.status_server.enabled is True
        assert cfg.status_server.port == 7777

    def test_full_config(self, tmp_path):
        wf1 = tmp_path / "WF1.md"
        wf2 = tmp_path / "WF2.md"
        wf1.touch()
        wf2.touch()
        cfg_path = _write_config(tmp_path, f"""
            projects:
              - name: alpha
                workflow: {wf1}
              - name: beta
                workflow: {wf2}
            auto_update:
              enabled: false
              interval_minutes: 60
            status_server:
              enabled: true
              port: 8888
        """)
        cfg = DaemonConfig.load(cfg_path)
        assert len(cfg.projects) == 2
        assert cfg.projects[1].name == "beta"
        assert cfg.auto_update.enabled is False
        assert cfg.auto_update.interval_minutes == 60
        assert cfg.status_server.port == 8888

    def test_tilde_expansion(self, tmp_path, monkeypatch):
        home = tmp_path / "home"
        home.mkdir()
        monkeypatch.setenv("HOME", str(home))
        wf = home / "WORKFLOW.md"
        wf.touch()
        cfg_path = _write_config(tmp_path, """
            projects:
              - name: p
                workflow: ~/WORKFLOW.md
        """)
        cfg = DaemonConfig.load(cfg_path)
        assert not cfg.projects[0].workflow.startswith("~")

    def test_env_var_in_workflow(self, tmp_path, monkeypatch):
        wf = tmp_path / "WORKFLOW.md"
        wf.touch()
        monkeypatch.setenv("MY_WF", str(wf))
        cfg_path = _write_config(tmp_path, """
            projects:
              - name: p
                workflow: $MY_WF
        """)
        cfg = DaemonConfig.load(cfg_path)
        assert cfg.projects[0].workflow == str(wf)

    def test_file_not_found(self):
        with pytest.raises(FileNotFoundError):
            DaemonConfig.load("/nonexistent/path/config.yaml")


class TestDaemonConfigValidate:
    def test_valid(self, tmp_path):
        wf = tmp_path / "WORKFLOW.md"
        wf.touch()
        cfg = DaemonConfig._from_raw({
            "projects": [{"name": "p", "workflow": str(wf)}]
        })
        assert cfg.validate() == []

    def test_no_projects(self, tmp_path):
        cfg = DaemonConfig._from_raw({"projects": []})
        errors = cfg.validate()
        assert any("No projects" in e for e in errors)

    def test_duplicate_names(self, tmp_path):
        wf = tmp_path / "WORKFLOW.md"
        wf.touch()
        cfg = DaemonConfig._from_raw({
            "projects": [
                {"name": "dup", "workflow": str(wf)},
                {"name": "dup", "workflow": str(wf)},
            ]
        })
        errors = cfg.validate()
        assert any("unique" in e for e in errors)

    def test_missing_workflow_file(self, tmp_path):
        cfg = DaemonConfig._from_raw({
            "projects": [{"name": "p", "workflow": "/nonexistent/WORKFLOW.md"}]
        })
        errors = cfg.validate()
        assert any("not found" in e for e in errors)
