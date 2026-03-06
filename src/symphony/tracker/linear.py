from __future__ import annotations

from datetime import datetime
from typing import Any

import httpx
import structlog

from symphony.models import BlockerRef, Issue

logger = structlog.get_logger()

_ISSUES_QUERY = """\
query($projectSlug: String!, $states: [String!], $after: String) {
  issues(
    filter: {
      project: { slugId: { eq: $projectSlug } }
      state: { name: { in: $states } }
    }
    first: 50
    after: $after
    orderBy: createdAt
  ) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id identifier title description priority
      state { name }
      branchName url
      labels { nodes { name } }
      relations {
        nodes { type relatedIssue { id identifier state { name } } }
      }
      createdAt updatedAt
    }
  }
}
"""

_ISSUES_BY_IDS_QUERY = """\
query($ids: [ID!]!) {
  issues(filter: { id: { in: $ids } }) {
    nodes { id identifier state { name } }
  }
}
"""


class LinearError(Exception):
    def __init__(self, code: str, message: str) -> None:
        self.code = code
        self.message = message
        super().__init__(f"[{code}] {message}")


class LinearClient:
    def __init__(
        self,
        api_key: str,
        endpoint: str,
        project_slug: str,
        active_states: list[str],
        terminal_states: list[str],
    ) -> None:
        if not api_key:
            raise LinearError("missing_tracker_api_key", "Linear API key is required")
        if not project_slug:
            raise LinearError(
                "missing_tracker_project_slug", "Linear project slug is required"
            )

        self._endpoint = endpoint
        self._project_slug = project_slug
        self._active_states = active_states
        self._terminal_states = terminal_states
        self._client = httpx.AsyncClient(
            headers={"Authorization": api_key, "Content-Type": "application/json"},
            timeout=30.0,
        )

    async def close(self) -> None:
        await self._client.aclose()

    # -- Protocol methods --

    async def fetch_candidate_issues(self) -> list[Issue]:
        return await self._fetch_issues_paginated(self._active_states)

    async def fetch_issues_by_states(self, state_names: list[str]) -> list[Issue]:
        if not state_names:
            return []
        return await self._fetch_issues_paginated(state_names)

    async def fetch_issue_states_by_ids(self, issue_ids: list[str]) -> list[Issue]:
        if not issue_ids:
            return []

        data = await self._execute(_ISSUES_BY_IDS_QUERY, {"ids": issue_ids})
        issues_data = data.get("issues")
        if not isinstance(issues_data, dict):
            raise LinearError(
                "linear_unknown_payload",
                "Unexpected response shape from Linear API",
            )

        nodes = issues_data.get("nodes", [])
        return [
            Issue(
                id=node["id"],
                identifier=node.get("identifier", ""),
                title="",
                description=None,
                priority=None,
                state=node.get("state", {}).get("name", ""),
                branch_name=None,
                url=None,
            )
            for node in nodes
        ]

    # -- Internal helpers --

    async def _fetch_issues_paginated(self, states: list[str]) -> list[Issue]:
        all_issues: list[Issue] = []
        cursor: str | None = None

        while True:
            variables: dict[str, Any] = {
                "projectSlug": self._project_slug,
                "states": states,
            }
            if cursor is not None:
                variables["after"] = cursor

            data = await self._execute(_ISSUES_QUERY, variables)
            issues_data = data.get("issues")
            if not isinstance(issues_data, dict):
                raise LinearError(
                    "linear_unknown_payload",
                    "Unexpected response shape from Linear API",
                )

            nodes = issues_data.get("nodes", [])
            for node in nodes:
                all_issues.append(self._normalize_issue(node))

            page_info = issues_data.get("pageInfo", {})
            if not page_info.get("hasNextPage", False):
                break

            cursor = page_info.get("endCursor")
            if cursor is None:
                raise LinearError(
                    "linear_missing_end_cursor",
                    "Pagination hasNextPage=true but endCursor is missing",
                )

        logger.debug(
            "linear.fetched_issues", count=len(all_issues), states=states
        )
        return all_issues

    async def _execute(
        self, query: str, variables: dict[str, Any]
    ) -> dict[str, Any]:
        try:
            resp = await self._client.post(
                self._endpoint,
                json={"query": query, "variables": variables},
            )
        except httpx.RequestError as exc:
            raise LinearError(
                "linear_api_request", f"Request failed: {exc}"
            ) from exc

        if resp.status_code != 200:
            raise LinearError(
                "linear_api_status",
                f"Linear API returned status {resp.status_code}: {resp.text[:500]}",
            )

        body = resp.json()

        if "errors" in body:
            errors = body["errors"]
            msg = "; ".join(e.get("message", str(e)) for e in errors)
            raise LinearError("linear_graphql_errors", msg)

        data = body.get("data")
        if not isinstance(data, dict):
            raise LinearError(
                "linear_unknown_payload",
                "Response missing 'data' field",
            )
        return data

    def _normalize_issue(self, node: dict[str, Any]) -> Issue:
        labels_data = node.get("labels", {})
        labels_nodes = labels_data.get("nodes", []) if isinstance(labels_data, dict) else []
        labels = [n.get("name", "").lower() for n in labels_nodes if n.get("name")]

        relations_data = node.get("relations", {})
        relations_nodes = relations_data.get("nodes", []) if isinstance(relations_data, dict) else []
        blocked_by: list[BlockerRef] = []
        for rel in relations_nodes:
            if rel.get("type") != "blocks":
                continue
            related = rel.get("relatedIssue", {})
            if related and related.get("id"):
                blocked_by.append(
                    BlockerRef(
                        id=related["id"],
                        identifier=related.get("identifier", ""),
                        state=related.get("state", {}).get("name", ""),
                    )
                )

        created_at = _parse_iso(node.get("createdAt"))
        updated_at = _parse_iso(node.get("updatedAt"))

        return Issue(
            id=node["id"],
            identifier=node.get("identifier", ""),
            title=node.get("title", ""),
            description=node.get("description"),
            priority=node.get("priority"),
            state=node.get("state", {}).get("name", ""),
            branch_name=node.get("branchName"),
            url=node.get("url"),
            labels=labels,
            blocked_by=blocked_by,
            created_at=created_at,
            updated_at=updated_at,
        )


def _parse_iso(value: str | None) -> datetime | None:
    if not value:
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except (ValueError, AttributeError):
        return None
