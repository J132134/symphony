from __future__ import annotations

from typing import Protocol, runtime_checkable

from symphony.models import Issue


@runtime_checkable
class TrackerClient(Protocol):
    async def fetch_candidate_issues(self) -> list[Issue]:
        """Fetch issues in active states for the configured project."""
        ...

    async def fetch_issues_by_states(self, state_names: list[str]) -> list[Issue]:
        """Fetch issues in specific states."""
        ...

    async def fetch_issue_states_by_ids(self, issue_ids: list[str]) -> list[Issue]:
        """Fetch current state for specific issue IDs (for reconciliation)."""
        ...

    async def close(self) -> None:
        """Close underlying connections."""
        ...
