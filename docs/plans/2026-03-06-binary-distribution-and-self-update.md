# Binary Distribution And Self-Update Implementation Plan

**Goal:** Replace source-based installation with a release-binary distribution flow so client Macs install only the `symphony` executable and update by checking a remote manifest instead of pulling `origin/main`.
**Architecture:** Publish versioned release artifacts per platform and keep a small channel manifest such as `manifest/stable.json` on a static URL. Add a local install state file plus a `self-update` path in the CLI that fetches the manifest, compares semantic versions, downloads the matching artifact, verifies its checksum, and atomically switches the local `current` binary symlink. Reuse the same updater from `symphony daemon` for periodic background updates.
**Tech Stack:** Go 1.22+, standard library `net/http`, `encoding/json`, `archive/tar`, `compress/gzip`, existing CLI/daemon packages, GitHub Releases or equivalent static hosting

---

### Task 1: Add version metadata and installer state model

**Files:**
- Create: `internal/version/version.go`
- Create: `internal/update/types.go`
- Create: `internal/update/config.go`
- Test: `internal/update/config_test.go`

**Step 1: Define build/runtime version metadata**
Expose current app version, commit, and build date from one place so CLI output and updater logic compare against the same source of truth.

**Step 2: Add local install state model**
Define config/state for:
- update channel (`stable`, `canary`)
- manifest URL
- current installed version
- last checked timestamp
- optional cached ETag

**Step 3: Run focused test - expect FAIL**
Run: `go test ./internal/update`

**Step 4: Implement load/save behavior**
Persist updater state under `~/.config/symphony/install.yaml` with safe defaults for first install.

**Step 5: Run focused test - expect PASS**
Run: `go test ./internal/update`

**Step 6: Commit**

---

### Task 2: Implement manifest check and artifact installer

**Files:**
- Create: `internal/update/client.go`
- Create: `internal/update/install.go`
- Create: `internal/update/semver.go`
- Test: `internal/update/client_test.go`
- Test: `internal/update/install_test.go`

**Step 1: Define manifest contract**
Support a manifest payload with channel, version, published time, and per-platform artifact metadata:
- download URL
- SHA256 checksum
- archive format

**Step 2: Implement conditional manifest fetch**
Use HTTP GET with timeout and optional `If-None-Match` header so unchanged manifests return `304 Not Modified`.

**Step 3: Implement update decision**
Compare current version and remote version, then resolve the matching artifact for `darwin-arm64` and future supported targets.

**Step 4: Implement staged install**
Download to a temporary directory, verify SHA256, extract the binary into `~/.local/share/symphony/versions/<version>/`, then atomically repoint a `current` symlink.

**Step 5: Run focused test - expect PASS**
Run: `go test ./internal/update`

**Step 6: Commit**

---

### Task 3: Expose install and update commands in the CLI

**Files:**
- Modify: `cmd/symphony/main.go`
- Modify: `README.md`

**Step 1: Add user-facing commands**
Add:
- `symphony version`
- `symphony self-update`
- `symphony channel`

**Step 2: Wire updater output**
Print whether the current install is up to date, whether an update was applied, and where the active binary path lives.

**Step 3: Run targeted verification**
Run: `go build ./...`

**Step 4: Commit**

---

### Task 4: Integrate periodic update checks into daemon mode

**Files:**
- Modify: `internal/daemon/manager.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/defaults.go`
- Test: `internal/daemon/manager_test.go`
- Modify: `README.md`

**Step 1: Reconcile daemon config with installer state**
Keep `auto_update` as a daemon behavior toggle while the actual install source of truth stays in the local install state file.

**Step 2: Reuse updater loop**
At daemon start, perform one non-blocking check. While running, poll on the configured interval and apply updates only after a successful staged install.

**Step 3: Define restart behavior**
When daemon mode updates the binary, log the new version and either:
- exit with a restart-required code for external supervisors, or
- restart itself explicitly if the process model already supports it

**Step 4: Run focused test**
Run: `go test ./internal/daemon`

**Step 5: Commit**

---

### Task 5: Add release packaging and manifest publication flow

**Files:**
- Create: `.github/workflows/release.yml`
- Create: `scripts/release/update-manifest.sh`
- Create: `manifest/stable.json`
- Modify: `README.md`

**Step 1: Publish immutable artifacts first**
On tag push, build platform archives, compute checksums, and upload assets to GitHub Releases.

**Step 2: Update channel manifest second**
After assets are available, rewrite `manifest/stable.json` to point at the release asset URLs and commit or publish it through the chosen static channel.

**Step 3: Document deployment invariants**
Document that release assets must be uploaded before the manifest changes, otherwise clients can observe a broken version pointer.

**Step 4: Run final verification**
Run: `go build ./...`
Run: `go test ./...`

**Step 5: Commit**
