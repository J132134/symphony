# Menubar Daemon Status Implementation Plan

**Goal:** Add a macOS menubar UI that shows daemon health, version, and active subprocess info without changing `symphony daemon` behavior.
**Architecture:** Extend the daemon manager with a read-only snapshot that summarizes project runtime health, tracker connectivity, running subprocesses, and build revision. Expose that snapshot through the existing status server, then add a Darwin-only `symphony menubar` command that polls the local endpoint and renders a systray UI.
**Tech Stack:** Go 1.22+, existing daemon/status packages, `runtime/debug`, `github.com/getlantern/systray`

---

### Task 1: Add daemon snapshot and tracker connectivity state

**Files:**
- Create: `internal/version/version.go`
- Modify: `internal/orchestrator/state.go`
- Modify: `internal/orchestrator/orchestrator.go`
- Modify: `internal/daemon/manager.go`
- Test: `internal/daemon/manager_test.go`

**Step 1: Track tracker fetch health**
Add tracker success/error timestamps to orchestrator state and update them on candidate fetch, reconcile fetch, and retry fetch calls so menu UI can detect network loss.

**Step 2: Build daemon snapshot**
Add a daemon snapshot type that includes overall status, build revision, per-project runtime state, and running subprocess summaries.

**Step 3: Run focused test**
Run: `go test ./internal/daemon`

### Task 2: Expose snapshot via status server

**Files:**
- Create: `internal/status/summary.go`
- Modify: `internal/status/server.go`
- Test: `internal/status/server_test.go`

**Step 1: Add menu-specific API**
Expose `GET /api/v1/summary` and return the daemon snapshot when the source supports it.

**Step 2: Run focused test**
Run: `go test ./internal/status`

### Task 3: Add macOS menubar command

**Files:**
- Modify: `cmd/symphony/main.go`
- Create: `internal/menubar/app_darwin.go`
- Create: `internal/menubar/app_stub.go`
- Create: `internal/menubar/client.go`
- Modify: `go.mod`
- Modify: `README.md`

**Step 1: Implement systray UI**
Add `symphony menubar` with spinner / warning / pause indicators, tooltip, dashboard shortcut, and quit menu.

**Step 2: Verify cross-platform build behavior**
Provide a non-Darwin stub so `go build ./...` remains clean outside macOS.

**Step 3: Run full verification**
Run: `go build ./...`
Run: `go test ./...`
