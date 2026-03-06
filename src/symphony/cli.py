from __future__ import annotations

import asyncio
import sys
from pathlib import Path

import click
import structlog

from symphony.logging import configure_logging
from symphony.workflow import WorkflowError, load_workflow
from symphony.config import SymphonyConfig


@click.group()
def cli() -> None:
    """Symphony Orchestrator — poll tracker, dispatch coding agents."""


@cli.command()
@click.option(
    "--workflow",
    type=click.Path(exists=True),
    default="WORKFLOW.md",
    help="Path to WORKFLOW.md",
)
@click.option("--port", type=int, default=None, help="Status server port")
@click.option("--log-level", type=str, default="INFO", help="Log level")
def run(workflow: str, port: int | None, log_level: str) -> None:
    """Start the orchestrator daemon."""
    configure_logging(level=log_level)
    log = structlog.get_logger()

    workflow_path = Path(workflow).resolve()
    log.info("symphony.starting", workflow=str(workflow_path))

    from symphony.orchestrator import Orchestrator

    orchestrator = Orchestrator(workflow_path=workflow_path, port=port)

    async def _run() -> None:
        # Optionally start status server
        status_task = None
        if port is not None:
            from symphony.status.server import create_status_app, run_status_server
            app = create_status_app(orchestrator)
            status_task = asyncio.create_task(
                run_status_server(app, port=port)
            )

        await orchestrator.start()

        if status_task and not status_task.done():
            status_task.cancel()

    asyncio.run(_run())


@cli.command()
@click.option(
    "--workflow",
    type=click.Path(exists=True),
    default="WORKFLOW.md",
    help="Path to WORKFLOW.md",
)
def validate(workflow: str) -> None:
    """Validate WORKFLOW.md and its config."""
    configure_logging(level="WARNING")

    workflow_path = Path(workflow).resolve()

    try:
        wf = load_workflow(workflow_path)
    except WorkflowError as exc:
        click.echo(f"Error [{exc.code}]: {exc}", err=True)
        sys.exit(1)

    config = SymphonyConfig(wf.config)
    errors = config.validate()

    if errors:
        click.echo("Config validation errors:", err=True)
        for err in errors:
            click.echo(f"  - {err}", err=True)
        sys.exit(1)

    click.echo(f"Workflow valid: {workflow_path}")
    click.echo(f"  Tracker: {config.tracker_kind}")
    click.echo(f"  Project: {config.tracker_project_slug}")
    click.echo(f"  Active states: {config.active_states}")
    click.echo(f"  Terminal states: {config.terminal_states}")
    click.echo(f"  Max concurrent: {config.max_concurrent_agents}")
    click.echo(f"  Agent command: {config.codex_command}")


@cli.command()
@click.option(
    "--config",
    "config_path",
    type=click.Path(),
    default=None,
    help="Path to config.yaml (default: ~/.config/symphony/config.yaml)",
)
@click.option("--log-level", type=str, default="INFO", help="Log level")
def daemon(config_path: str | None, log_level: str) -> None:
    """Start multi-project daemon (reads config.yaml)."""
    configure_logging(level=log_level)
    log = structlog.get_logger()

    from symphony.daemon.config import DaemonConfig
    from symphony.daemon.manager import DaemonManager

    try:
        cfg = DaemonConfig.load(config_path)
    except FileNotFoundError as exc:
        click.echo(f"Config not found: {exc}", err=True)
        sys.exit(1)

    errors = cfg.validate()
    if errors:
        click.echo("Config validation errors:", err=True)
        for err in errors:
            click.echo(f"  - {err}", err=True)
        sys.exit(1)

    log.info("symphony.daemon.starting", projects=[p.name for p in cfg.projects])

    manager = DaemonManager(cfg)

    async def _run() -> None:
        status_task = None
        if cfg.status_server.enabled:
            from symphony.status.server import create_status_app, run_status_server
            app = create_status_app(manager)
            status_task = asyncio.create_task(
                run_status_server(app, port=cfg.status_server.port)
            )

        # Start auto-update background task if enabled
        update_task = None
        if cfg.auto_update.enabled:
            import threading
            from symphony.daemon.updater import check_for_updates
            interval_s = cfg.auto_update.interval_minutes * 60

            async def _update_loop() -> None:
                while True:
                    try:
                        await asyncio.wait_for(
                            manager._shutdown_event.wait(),
                            timeout=interval_s,
                        )
                        break
                    except asyncio.TimeoutError:
                        loop = asyncio.get_running_loop()
                        await loop.run_in_executor(
                            None, check_for_updates, manager, None
                        )

            update_task = asyncio.create_task(_update_loop())

        await manager.run()

        for task in (status_task, update_task):
            if task and not task.done():
                task.cancel()

    asyncio.run(_run())
