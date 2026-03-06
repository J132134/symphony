---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: 057e772534dc
  active_states:
    - Todo
    - In Progress
    - Human Review
    - Merging
    - Rework
  terminal_states:
    - Done
    - Cancelled
    - Canceled
    - Duplicate

polling:
  interval_ms: 10000
  idle_interval_ms: 60000

workspace:
  root: ~/.symphony/workspaces

hooks:
  after_create: |
    git clone --depth 1 https://github.com/J132134/symphony.git .
    go mod download
  before_run: |
    if [ ! -d .git ]; then
      git clone --depth 1 https://github.com/J132134/symphony.git .
      go mod download
    else
      git fetch origin
      git stash --include-untracked 2>/dev/null || true
      git rebase origin/main || git rebase --abort
      git stash pop 2>/dev/null || true
    fi
  after_run: |
    git push origin HEAD 2>/dev/null || true
  timeout_ms: 120000

agent:
  max_concurrent_agents: 2
  max_turns: 20
  max_retry_backoff_ms: 300000

codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_timeout_ms: 3600000
  stall_timeout_ms: 600000
---
You are working on Linear ticket `{{ issue.identifier }}`.

{% if attempt and attempt > 1 %}
Continuation context:

- This is retry attempt #{{ attempt }} — the ticket is still in an active state.
- Resume from the current workspace state instead of restarting from scratch.
- Do not repeat already-completed steps unless necessary.
- Do not end the turn while the issue remains active unless blocked by missing permissions/secrets.
{% endif %}

Issue context:
- Identifier: {{ issue.identifier }}
- Title: {{ issue.title }}
- Status: {{ issue.state }}
- Labels: {{ issue.labels }}
- URL: {{ issue.url }}

Description:
{% if issue.description %}
{{ issue.description }}
{% else %}
No description provided.
{% endif %}

---

## Project: symphony

Go multi-project orchestrator daemon. Key conventions:
- Package manager: Go modules (`go mod`)
- Stack: Go 1.22+, `log/slog`, `gopkg.in/yaml.v3`
- Entry point: `cmd/symphony/main.go`
- Build: `make build` or `go build ./...`
- Install: `make install` (installs to `~/.local/bin/symphony`)
- Tests: `go test ./...`
- Lint: `go vet ./...`

---

## Instructions

This is an unattended orchestration session. Never ask a human to perform follow-up actions. Only stop early for a true blocker (missing required auth/permissions/secrets).

## Status map

- `Todo` → immediately transition to `In Progress` before any work
- `In Progress` → implementation actively underway
- `Human Review` → PR attached and validated; waiting on human approval
- `Merging` → approved; run `gh pr merge --squash --auto` and move to `Done`
- `Rework` → reviewer requested changes; full reset from `origin/main`
- `Done` → terminal; do nothing

## Step 0: Route by current status

1. Fetch the issue via Linear MCP or `linear_graphql` to confirm current state.
2. Route to matching flow:
   - `Todo` → move to `In Progress` first, then create workpad, then execute.
   - `In Progress` → find existing workpad comment and continue.
   - `Human Review` → read the workpad's last Notes entry; if it already records a "리뷰 대기 중" (or equivalent waiting) note and there is no new review feedback or comment since that timestamp → **end the turn immediately without adding any note**. If feedback exists, move to `Rework`.
   - `Merging` → run `gh pr merge --squash --auto`, then move to `Done`.
   - `Rework` → full reset flow.
   - `Done` → shut down.

## Step 1: Workpad setup

Find or create a single persistent comment with header `## Codex Workpad` on the issue. Never create a second workpad — reuse the existing one. Record all progress there.

Workpad template:

```md
## Codex Workpad

```text
<hostname>:<abs-path>@<short-sha>
```

### Plan
- [ ] 1. Task

### Acceptance Criteria
- [ ] Criterion

### Validation
- [ ] `go build ./...`
- [ ] `go test ./...`

### Notes
- <KST timestamp, e.g. 2026-03-06 10:30 KST>: <note in Korean>

### Confusions
- <only when something was unclear>
```

## Step 2: Execution (In Progress)

1. Sync: `git fetch origin && git rebase origin/main`, record result in workpad.
   Create a working branch: `git checkout -b {{ issue.identifier | lower }}-<short-slug>` (e.g. `{{ issue.identifier | lower }}-implement-feature`). Branch name **must** start with `{{ issue.identifier | lower }}-` for Linear's GitHub integration to auto-link the PR.
2. Reproduce the issue or confirm expected behavior before writing code.
3. Implement against the plan checklist; keep workpad updated after each milestone.
4. Run `go build ./...` and `go test ./...` — must be clean before committing.
5. Commit with clear messages. Push: `git push origin HEAD`.
6. Open a PR: `gh pr create --draft --title "..." --body "..."`, add label `symphony`.
   - Branch name **must** follow the pattern `{{ issue.identifier | lower }}-<short-slug>` so Linear's GitHub integration auto-links the PR to the issue.
7. Run full PR feedback sweep (inline comments + review summaries).
8. Merge latest `origin/main`, resolve conflicts, rerun checks.
9. When all criteria are met → move issue to `Human Review`.

## Step 3: Human Review / Merging

- `Human Review`: do not change code; poll for feedback.
  1. Read the workpad's last Notes entry.
  2. If it already records a "리뷰 대기 중" (or equivalent waiting) note **and** no new review comment has been posted since that note's timestamp → **end the turn immediately without adding any note**.
  3. If new feedback exists → update workpad and move to `Rework`.
  4. If approved and moved to `Merging` → `gh pr merge --squash --auto` → move to `Done`.

## Step 4: Rework

Full reset: close existing PR, delete workpad comment, create new branch from `origin/main`, restart from Step 1.

## Language & Timezone

- All workpad content (Plan, Notes, Confusions) must be written in **Korean**.
- All PR titles, bodies, and review comments must be written in **Korean**.
- All timestamps must use **KST (UTC+9)** in the format `YYYY-MM-DD HH:mm KST`.
- Issue titles, identifiers, code, and CLI output remain as-is (English); only your written content is Korean.

## Guardrails

- Do not edit the issue body/description.
- One workpad comment per issue; never post a separate "done" comment.
- Temporary proof edits must be reverted before commit.
- Out-of-scope discoveries → file a separate Backlog issue.
- Do not move to `Human Review` until: build clean, tests green, PR feedback sweep complete, PR checks passing.
