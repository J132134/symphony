from __future__ import annotations

import pytest

from symphony.models import Issue, BlockerRef
from symphony.workflow import WorkflowDefinition, WorkflowError, load_workflow, render_prompt


VALID_WORKFLOW = """\
---
tracker:
  kind: linear
  api_key: test
  project_slug: proj
---
# {{ issue.identifier }}: {{ issue.title }}

{{ issue.description or "No description." }}

Attempt {{ attempt }}.
"""


def _write_workflow(tmp_path, content=VALID_WORKFLOW):
    p = tmp_path / "WORKFLOW.md"
    p.write_text(content)
    return p


def _make_issue(**kwargs):
    defaults = dict(
        id="id-1",
        identifier="ENG-1",
        title="Fix bug",
        description="Some bug to fix",
        priority=1,
        state="In Progress",
        branch_name="eng-1-fix-bug",
        url="https://linear.app/issue/ENG-1",
    )
    defaults.update(kwargs)
    return Issue(**defaults)


class TestLoadWorkflow:
    def test_valid_workflow(self, tmp_path):
        p = _write_workflow(tmp_path)
        wf = load_workflow(p)
        assert isinstance(wf, WorkflowDefinition)
        assert wf.config["tracker"]["kind"] == "linear"
        assert "{{ issue.identifier }}" in wf.raw_prompt

    def test_missing_file(self, tmp_path):
        with pytest.raises(WorkflowError) as exc_info:
            load_workflow(tmp_path / "nonexistent.md")
        assert exc_info.value.code == "missing_workflow_file"

    def test_no_front_matter_delimiter(self, tmp_path):
        p = tmp_path / "WORKFLOW.md"
        p.write_text("just plain text")
        wf = load_workflow(p)
        assert wf.config == {}
        assert wf.raw_prompt == "just plain text"

    def test_no_closing_delimiter(self, tmp_path):
        p = tmp_path / "WORKFLOW.md"
        p.write_text("---\nkey: value\nno closing")
        with pytest.raises(WorkflowError) as exc_info:
            load_workflow(p)
        assert exc_info.value.code == "workflow_parse_error"

    def test_front_matter_not_a_map(self, tmp_path):
        p = tmp_path / "WORKFLOW.md"
        p.write_text("---\n- item1\n- item2\n---\nPrompt here")
        with pytest.raises(WorkflowError) as exc_info:
            load_workflow(p)
        assert exc_info.value.code == "workflow_front_matter_not_a_map"

    def test_invalid_yaml(self, tmp_path):
        p = tmp_path / "WORKFLOW.md"
        p.write_text("---\n: invalid: yaml: {{{\n---\nPrompt")
        with pytest.raises(WorkflowError) as exc_info:
            load_workflow(p)
        assert exc_info.value.code == "workflow_parse_error"

    def test_invalid_jinja2_syntax(self, tmp_path):
        p = tmp_path / "WORKFLOW.md"
        p.write_text("---\nkey: val\n---\n{% if unclosed %}")
        with pytest.raises(WorkflowError) as exc_info:
            load_workflow(p)
        assert exc_info.value.code == "template_parse_error"

    def test_empty_front_matter(self, tmp_path):
        p = tmp_path / "WORKFLOW.md"
        p.write_text("---\n---\nJust a prompt")
        wf = load_workflow(p)
        assert wf.config == {}

    def test_file_path_stored(self, tmp_path):
        p = _write_workflow(tmp_path)
        wf = load_workflow(p)
        assert wf.file_path == str(p)


class TestRenderPrompt:
    def test_basic_render(self, tmp_path):
        p = _write_workflow(tmp_path)
        wf = load_workflow(p)
        issue = _make_issue()
        result = render_prompt(wf, issue, attempt=1)
        assert "ENG-1" in result
        assert "Fix bug" in result
        assert "Some bug to fix" in result
        assert "Attempt 1." in result

    def test_attempt_number(self, tmp_path):
        p = _write_workflow(tmp_path)
        wf = load_workflow(p)
        result = render_prompt(wf, _make_issue(), attempt=3)
        assert "Attempt 3." in result

    def test_none_description_fallback(self, tmp_path):
        p = _write_workflow(tmp_path)
        wf = load_workflow(p)
        result = render_prompt(wf, _make_issue(description=None))
        assert "No description." in result

    def test_description_present(self, tmp_path):
        p = _write_workflow(tmp_path)
        wf = load_workflow(p)
        result = render_prompt(wf, _make_issue(description="Actual desc"))
        assert "Actual desc" in result
        assert "No description." not in result

    def test_undefined_variable_error(self, tmp_path):
        p = tmp_path / "WORKFLOW.md"
        p.write_text("---\nk: v\n---\n{{ nonexistent_var }}")
        wf = load_workflow(p)
        with pytest.raises(WorkflowError) as exc_info:
            render_prompt(wf, _make_issue())
        assert exc_info.value.code == "template_render_error"

    def test_issue_fields_available(self, tmp_path):
        p = tmp_path / "WORKFLOW.md"
        p.write_text("---\nk: v\n---\n{{ issue.id }} {{ issue.state }} {{ issue.priority }}")
        wf = load_workflow(p)
        result = render_prompt(wf, _make_issue())
        assert "id-1" in result
        assert "In Progress" in result
        assert "1" in result

    def test_labels_accessible(self, tmp_path):
        p = tmp_path / "WORKFLOW.md"
        p.write_text("---\nk: v\n---\n{% for l in issue.labels %}{{ l }} {% endfor %}")
        wf = load_workflow(p)
        result = render_prompt(wf, _make_issue(labels=["bug", "urgent"]))
        assert "bug" in result
        assert "urgent" in result

    def test_blocked_by_accessible(self, tmp_path):
        p = tmp_path / "WORKFLOW.md"
        p.write_text("---\nk: v\n---\n{% for b in issue.blocked_by %}{{ b.identifier }} {% endfor %}")
        wf = load_workflow(p)
        blockers = [BlockerRef(id="b1", identifier="ENG-0", state="Todo")]
        result = render_prompt(wf, _make_issue(blocked_by=blockers))
        assert "ENG-0" in result
