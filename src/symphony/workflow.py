from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

import jinja2
import yaml

from .models import Issue


class WorkflowError(Exception):
    """Raised when a workflow file cannot be loaded or rendered."""

    def __init__(self, code: str, message: str) -> None:
        self.code = code
        super().__init__(message)


@dataclass
class WorkflowDefinition:
    config: dict[str, Any]  # raw YAML front matter
    prompt_template: jinja2.Template
    raw_prompt: str
    file_path: str


_JINJA_ENV = jinja2.Environment(undefined=jinja2.StrictUndefined)


def load_workflow(path: str | Path) -> WorkflowDefinition:
    """Parse ``WORKFLOW.md``: split YAML front matter from prompt body.

    The file must start with ``---`` on the first line. A second ``---``
    delimiter closes the front matter block; everything after it is the
    Jinja2 prompt template.

    Raises :class:`WorkflowError` with codes:
    - ``missing_workflow_file``
    - ``workflow_parse_error``
    - ``workflow_front_matter_not_a_map``
    - ``template_parse_error``
    """
    filepath = Path(path)

    if not filepath.exists():
        raise WorkflowError(
            "missing_workflow_file",
            f"Workflow file not found: {filepath}",
        )

    try:
        text = filepath.read_text(encoding="utf-8")
    except OSError as exc:
        raise WorkflowError(
            "workflow_parse_error",
            f"Cannot read workflow file: {exc}",
        ) from exc

    lines = text.split("\n")

    # Spec §5.2: if file starts with '---', parse front matter; otherwise treat whole file as prompt
    if lines and lines[0].strip() == "---":
        # Find closing delimiter
        closing_idx: int | None = None
        for idx in range(1, len(lines)):
            if lines[idx].strip() == "---":
                closing_idx = idx
                break

        if closing_idx is None:
            raise WorkflowError(
                "workflow_parse_error",
                "No closing '---' found for YAML front matter",
            )

        yaml_block = "\n".join(lines[1:closing_idx])
        raw_prompt = "\n".join(lines[closing_idx + 1 :])
    else:
        # No front matter — empty config, entire file is prompt body
        yaml_block = ""
        raw_prompt = text

    # Parse YAML
    try:
        config = yaml.safe_load(yaml_block)
    except yaml.YAMLError as exc:
        raise WorkflowError(
            "workflow_parse_error",
            f"Invalid YAML in front matter: {exc}",
        ) from exc

    if config is None:
        config = {}

    if not isinstance(config, dict):
        raise WorkflowError(
            "workflow_front_matter_not_a_map",
            f"Front matter must be a YAML mapping, got {type(config).__name__}",
        )

    # Compile Jinja2 template
    try:
        template = _JINJA_ENV.from_string(raw_prompt)
    except jinja2.TemplateSyntaxError as exc:
        raise WorkflowError(
            "template_parse_error",
            f"Jinja2 template syntax error: {exc}",
        ) from exc

    return WorkflowDefinition(
        config=config,
        prompt_template=template,
        raw_prompt=raw_prompt,
        file_path=str(filepath),
    )


def render_prompt(
    workflow: WorkflowDefinition,
    issue: Issue,
    attempt: int = 1,
) -> str:
    """Render the Jinja2 prompt template with issue fields and attempt number.

    Raises :class:`WorkflowError` with code ``template_render_error``
    on any rendering failure.
    """
    try:
        return workflow.prompt_template.render(issue=issue, attempt=attempt)
    except jinja2.UndefinedError as exc:
        raise WorkflowError(
            "template_render_error",
            f"Template render error (undefined variable): {exc}",
        ) from exc
    except jinja2.TemplateError as exc:
        raise WorkflowError(
            "template_render_error",
            f"Template render error: {exc}",
        ) from exc
