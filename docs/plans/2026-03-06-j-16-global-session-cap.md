# J-16 Global Session Cap Implementation Plan

**Goal:** Add a daemon-wide concurrent session cap so multi-project runs cannot grow without bound, with a dynamic default and `config.yaml` override.
**Architecture:** Keep per-project `WORKFLOW.md` limits unchanged, and add a daemon-level shared limiter that all orchestrators must pass before dispatching a new run. Parse the daemon-level cap from `~/.config/symphony/config.yaml`, defaulting it from local CPU capacity when unset.
**Tech Stack:** Go 1.22+, `log/slog`, `gopkg.in/yaml.v3`

---

### Task 1: Add daemon config for total concurrent sessions

**Files:**
- Modify: `internal/config/daemon.go`
- Test: `internal/config/daemon_test.go`

**Step 1: Write config parsing and defaulting tests**

Add tests that verify:
- unset `agent.max_total_concurrent_sessions` uses the dynamic default
- explicit `agent.max_total_concurrent_sessions` is loaded as-is
- invalid configured values are rejected by validation

**Step 2: Implement config parsing**

Add an `agent` section to `DaemonConfig`, parse `max_total_concurrent_sessions`, and expose a getter that falls back to the dynamic default.

**Step 3: Run targeted test**

Run: `go test ./internal/config ./internal/daemon ./internal/orchestrator`

### Task 2: Enforce a shared daemon-wide limiter

**Files:**
- Add: `internal/orchestrator/limiter.go`
- Modify: `internal/orchestrator/orchestrator.go`
- Modify: `internal/orchestrator/state.go`
- Modify: `internal/daemon/manager.go`

**Step 1: Add a shared limiter type**

Create a small concurrency-safe limiter with `TryAcquire`, `Release`, `Limit`, and `Available`.

**Step 2: Wire limiter through daemon manager**

Create the limiter once in `Manager` using the daemon config, then pass it into every project `Orchestrator`.

**Step 3: Gate dispatch and retry paths**

Acquire a global slot before dispatching a run, and always release it when the worker finishes or is canceled.

### Task 3: Surface and document the new behavior

**Files:**
- Modify: `cmd/symphony/main.go`
- Modify: `README.md`

**Step 1: Update daemon startup logging**

Include the effective daemon-wide cap in startup logs for operational visibility.

**Step 2: Update docs**

Document the new `config.yaml` field, its default behavior, and how it interacts with per-project `WORKFLOW.md` limits.

**Step 3: Run final verification**

Run: `gofmt -w ... && go build ./... && go test ./...`
