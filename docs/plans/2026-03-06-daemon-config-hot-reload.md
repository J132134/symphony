# Daemon Config Hot Reload Implementation Plan

**Goal:** Reload daemon configuration when `~/.config/symphony/config.yaml` changes without restarting the process.
**Architecture:** Add a daemon runtime supervisor that owns the current manager/status-server/update-loop set, watches the config file on an interval, validates newly loaded config, and swaps runtimes only after a valid reload. Invalid config changes should be logged while the previous runtime keeps running.
**Tech Stack:** Go 1.22+, `log/slog`, existing daemon/status/config packages

---

### Task 1: Add daemon runtime supervisor

**Files:**
- Create: `internal/daemon/runtime.go`
- Test: `internal/daemon/runtime_test.go`

**Step 1: Write reloadable runtime wrapper**
Define a supervisor that:
- starts one child runtime with its own cancellable context
- polls the daemon config file every 5 seconds
- detects file changes via stat fingerprint
- reloads config only when load + validation succeed
- keeps the old runtime alive when reload fails

**Step 2: Run test - expect FAIL**
Run: `go test ./internal/daemon`

**Step 3: Implement child runtime launch**
Use existing `Manager`, `status.Server`, and `RunUpdateLoop` to build one managed daemon instance per config revision.

**Step 4: Run test - expect PASS**

**Step 5: Commit**

---

### Task 2: Wire runtime into CLI entrypoint

**Files:**
- Modify: `cmd/symphony/main.go`

**Step 1: Replace one-shot daemon startup**
Call the new runtime supervisor from `cmdDaemon` after initial config validation.

**Step 2: Run targeted verification**
Run: `go build ./...`

**Step 3: Commit**

---

### Task 3: Document behavior

**Files:**
- Modify: `README.md`

**Step 1: Update daemon docs**
Mention that `symphony daemon` reloads `config.yaml` automatically after changes.

**Step 2: Run full verification**
Run: `go test ./...`

**Step 3: Commit**
