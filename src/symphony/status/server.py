from __future__ import annotations

import asyncio
from typing import TYPE_CHECKING, Union

import uvicorn
from fastapi import FastAPI
from fastapi.responses import HTMLResponse, JSONResponse

if TYPE_CHECKING:
    from symphony.daemon.manager import DaemonManager
    from symphony.orchestrator import Orchestrator

# Either a single Orchestrator or a DaemonManager (multi-project).
StatusSource = Union["Orchestrator", "DaemonManager"]


def _is_manager(source: StatusSource) -> bool:
    from symphony.daemon.manager import DaemonManager  # noqa: PLC0415
    return isinstance(source, DaemonManager)


def create_status_app(source: StatusSource) -> FastAPI:
    """Create a FastAPI app wired to an Orchestrator or DaemonManager."""
    app = FastAPI(title="Symphony Status", version="0.2.0")

    # ------------------------------------------------------------------ #
    # Multi-project endpoints                                             #
    # ------------------------------------------------------------------ #

    @app.get("/api/v1/projects")
    async def api_projects() -> JSONResponse:
        if _is_manager(source):
            from symphony.daemon.manager import DaemonManager  # noqa: PLC0415
            assert isinstance(source, DaemonManager)
            states = source.get_all_states()
            return JSONResponse([
                {
                    "name": name,
                    "running": len(st.running),
                    "retrying": len(st.retry_queue),
                    "completed_count": st.completed_count,
                    "total_tokens": st.codex_totals.total_tokens,
                }
                for name, st in states.items()
            ])
        else:
            from symphony.orchestrator import Orchestrator  # noqa: PLC0415
            assert isinstance(source, Orchestrator)
            state = source.get_state()
            return JSONResponse([{
                "name": "default",
                "running": len(state.running),
                "retrying": len(state.retry_queue),
                "completed_count": state.completed_count,
                "total_tokens": state.codex_totals.total_tokens,
            }])

    @app.get("/api/v1/projects/{name}/state")
    async def api_project_state(name: str) -> JSONResponse:
        if _is_manager(source):
            from symphony.daemon.manager import DaemonManager  # noqa: PLC0415
            assert isinstance(source, DaemonManager)
            states = source.get_all_states()
            state = states.get(name)
            if state is None:
                return JSONResponse(
                    {"error": "not_found", "message": f"Project '{name}' not found"},
                    status_code=404,
                )
        else:
            from symphony.orchestrator import Orchestrator  # noqa: PLC0415
            assert isinstance(source, Orchestrator)
            if name != "default":
                return JSONResponse(
                    {"error": "not_found", "message": f"Project '{name}' not found"},
                    status_code=404,
                )
            state = source.get_state()

        return JSONResponse(_state_to_dict(state))

    # ------------------------------------------------------------------ #
    # Legacy single-orchestrator endpoints (backward compat)              #
    # ------------------------------------------------------------------ #

    @app.get("/api/v1/state")
    async def api_state() -> JSONResponse:
        if _is_manager(source):
            from symphony.daemon.manager import DaemonManager  # noqa: PLC0415
            assert isinstance(source, DaemonManager)
            states = list(source.get_all_states().values())
            combined_running: dict = {}
            combined_retrying: dict = {}
            combined_completed = 0
            combined_input = combined_output = combined_total = 0
            for st in states:
                for attempt in st.running.values():
                    combined_running[attempt.identifier] = _attempt_to_dict(attempt)
                for entry in st.retry_queue.values():
                    combined_retrying[entry.identifier] = {
                        "attempt": entry.attempt,
                        "error": entry.error,
                    }
                combined_completed += st.completed_count
                combined_input += st.codex_totals.input_tokens
                combined_output += st.codex_totals.output_tokens
                combined_total += st.codex_totals.total_tokens
            return JSONResponse({
                "running": combined_running,
                "retrying": combined_retrying,
                "completed_count": combined_completed,
                "codex_totals": {
                    "input_tokens": combined_input,
                    "output_tokens": combined_output,
                    "total_tokens": combined_total,
                },
                "rate_limits": [],
            })
        else:
            from symphony.orchestrator import Orchestrator  # noqa: PLC0415
            assert isinstance(source, Orchestrator)
            state = source.get_state()
            return JSONResponse({
                "running": {
                    attempt.identifier: _attempt_to_dict(attempt)
                    for attempt in state.running.values()
                },
                "retrying": {
                    entry.identifier: {"attempt": entry.attempt, "error": entry.error}
                    for entry in state.retry_queue.values()
                },
                "completed_count": state.completed_count,
                "codex_totals": {
                    "input_tokens": state.codex_totals.input_tokens,
                    "output_tokens": state.codex_totals.output_tokens,
                    "total_tokens": state.codex_totals.total_tokens,
                },
                "rate_limits": [
                    {
                        "model": rl.model,
                        "remaining_tokens": rl.remaining_tokens,
                        "remaining_requests": rl.remaining_requests,
                        "reset_at": rl.reset_at.isoformat() if rl.reset_at else None,
                    }
                    for rl in state.rate_limits
                ],
            })

    @app.get("/api/v1/{issue_identifier}")
    async def api_issue(issue_identifier: str) -> JSONResponse:
        if _is_manager(source):
            from symphony.daemon.manager import DaemonManager  # noqa: PLC0415
            assert isinstance(source, DaemonManager)
            for st in source.get_all_states().values():
                for attempt in st.running.values():
                    if attempt.identifier == issue_identifier:
                        return JSONResponse(_attempt_detail(attempt))
        else:
            from symphony.orchestrator import Orchestrator  # noqa: PLC0415
            assert isinstance(source, Orchestrator)
            state = source.get_state()
            for attempt in state.running.values():
                if attempt.identifier == issue_identifier:
                    return JSONResponse(_attempt_detail(attempt))

        return JSONResponse(
            {"error": "not_found", "message": f"Issue {issue_identifier} not running"},
            status_code=404,
        )

    @app.post("/api/v1/refresh", status_code=202)
    async def api_refresh() -> JSONResponse:
        if not _is_manager(source):
            from symphony.orchestrator import Orchestrator  # noqa: PLC0415
            assert isinstance(source, Orchestrator)
            asyncio.create_task(source.trigger_refresh())
        return JSONResponse({"status": "accepted"})

    # ------------------------------------------------------------------ #
    # Dashboard                                                           #
    # ------------------------------------------------------------------ #

    @app.get("/", response_class=HTMLResponse)
    async def dashboard() -> str:
        if _is_manager(source):
            from symphony.daemon.manager import DaemonManager  # noqa: PLC0415
            assert isinstance(source, DaemonManager)
            states = source.get_all_states()
        else:
            from symphony.orchestrator import Orchestrator  # noqa: PLC0415
            assert isinstance(source, Orchestrator)
            states = {"default": source.get_state()}

        rows = ""
        for proj_name, state in states.items():
            for attempt in state.running.values():
                rows += (
                    f"<tr><td>{proj_name}</td><td>{attempt.identifier}</td>"
                    f"<td>{attempt.status.value}</td>"
                    f"<td>{attempt.session.turn_count}</td>"
                    f"<td>{attempt.session.total_tokens}</td></tr>"
                )
        if not rows:
            rows = '<tr><td colspan="5">No running agents</td></tr>'

        total_running = sum(len(st.running) for st in states.values())
        total_retrying = sum(len(st.retry_queue) for st in states.values())
        total_completed = sum(st.completed_count for st in states.values())
        total_tokens = sum(st.codex_totals.total_tokens for st in states.values())

        return f"""<!DOCTYPE html>
<html><head><title>Symphony</title>
<meta http-equiv="refresh" content="10">
<style>
body {{ font-family: system-ui, sans-serif; margin: 2rem; background: #111; color: #ddd; }}
table {{ border-collapse: collapse; width: 100%; margin: 1rem 0; }}
th, td {{ border: 1px solid #333; padding: 0.5rem 1rem; text-align: left; }}
th {{ background: #222; }}
h1 {{ color: #7af; }}
h2 {{ color: #aaa; margin-top: 2rem; }}
.stat {{ display: inline-block; margin: 0 2rem 1rem 0; }}
.stat-val {{ font-size: 1.5rem; font-weight: bold; color: #7f7; }}
.stat-label {{ font-size: 0.8rem; color: #888; }}
</style></head><body>
<h1>Symphony Orchestrator</h1>
<div>
  <div class="stat"><div class="stat-val">{total_running}</div><div class="stat-label">Running</div></div>
  <div class="stat"><div class="stat-val">{total_retrying}</div><div class="stat-label">Retrying</div></div>
  <div class="stat"><div class="stat-val">{total_completed}</div><div class="stat-label">Completed</div></div>
  <div class="stat"><div class="stat-val">{total_tokens:,}</div><div class="stat-label">Total Tokens</div></div>
</div>
<h2>Running Sessions</h2>
<table><tr><th>Project</th><th>Issue</th><th>Status</th><th>Turns</th><th>Tokens</th></tr>
{rows}
</table>
</body></html>"""

    return app


def _attempt_to_dict(attempt) -> dict:
    return {
        "issue_id": attempt.issue_id,
        "status": attempt.status.value,
        "attempt": attempt.attempt,
        "turn_count": attempt.session.turn_count,
        "tokens": {
            "input": attempt.session.input_tokens,
            "output": attempt.session.output_tokens,
            "total": attempt.session.total_tokens,
        },
        "pid": attempt.session.codex_app_server_pid,
        "started_at": attempt.started_at.isoformat() if attempt.started_at else None,
    }


def _attempt_detail(attempt) -> dict:
    return {
        "identifier": attempt.identifier,
        "issue_id": attempt.issue_id,
        "status": attempt.status.value,
        "attempt": attempt.attempt,
        "workspace_path": attempt.workspace_path,
        "session": {
            "session_id": attempt.session.session_id,
            "thread_id": attempt.session.thread_id,
            "turn_count": attempt.session.turn_count,
            "tokens": {
                "input": attempt.session.input_tokens,
                "output": attempt.session.output_tokens,
                "total": attempt.session.total_tokens,
            },
            "pid": attempt.session.codex_app_server_pid,
            "last_event_at": (
                attempt.session.last_event_at.isoformat()
                if attempt.session.last_event_at else None
            ),
        },
        "started_at": attempt.started_at.isoformat() if attempt.started_at else None,
        "error": attempt.error,
    }


def _state_to_dict(state) -> dict:
    return {
        "running": {
            attempt.identifier: _attempt_to_dict(attempt)
            for attempt in state.running.values()
        },
        "retrying": {
            entry.identifier: {"attempt": entry.attempt, "error": entry.error}
            for entry in state.retry_queue.values()
        },
        "completed_count": state.completed_count,
        "codex_totals": {
            "input_tokens": state.codex_totals.input_tokens,
            "output_tokens": state.codex_totals.output_tokens,
            "total_tokens": state.codex_totals.total_tokens,
        },
    }


async def run_status_server(app: FastAPI, port: int = 7777) -> None:
    """Run the status server as an async task."""
    config = uvicorn.Config(
        app,
        host="127.0.0.1",
        port=port,
        log_level="warning",
    )
    server = uvicorn.Server(config)
    await server.serve()
