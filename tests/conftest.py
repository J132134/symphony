from __future__ import annotations

from datetime import datetime, timezone

import pytest

from symphony.models import BlockerRef, Issue


def make_issue(
    id: str = "id-1",
    identifier: str = "ENG-1",
    title: str = "Fix bug",
    description: str | None = None,
    priority: int | None = 1,
    state: str = "In Progress",
    branch_name: str | None = None,
    url: str | None = None,
    created_at: datetime | None = None,
    blocked_by: list[BlockerRef] | None = None,
) -> Issue:
    return Issue(
        id=id,
        identifier=identifier,
        title=title,
        description=description,
        priority=priority,
        state=state,
        branch_name=branch_name,
        url=url,
        created_at=created_at or datetime(2025, 1, 1, tzinfo=timezone.utc),
        blocked_by=blocked_by or [],
    )


@pytest.fixture(name="make_issue")
def make_issue_fixture():
    """Pytest fixture returning the make_issue factory."""
    return make_issue
