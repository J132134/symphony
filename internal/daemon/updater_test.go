package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunSelfUpdateHelperNoChangeDoesNotInstall(t *testing.T) {
	toolsDir := t.TempDir()
	repoDir := t.TempDir()
	installDir := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "git-hash")
	markerFile := filepath.Join(t.TempDir(), "go-marker")

	if err := os.WriteFile(stateFile, []byte("old-hash\n"), 0o644); err != nil {
		t.Fatalf("write git hash: %v", err)
	}
	writeFakeTool(t, filepath.Join(toolsDir, "git"), fakeGitScript)
	writeFakeTool(t, filepath.Join(toolsDir, "go"), fakeGoScript)

	t.Setenv("PATH", toolsDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_GIT_STATE_FILE", stateFile)
	t.Setenv("FAKE_GO_MARKER_FILE", markerFile)

	code := RunSelfUpdateHelper(repoDir, installDir)
	if code != updateHelperNoChange {
		t.Fatalf("code = %d, want %d", code, updateHelperNoChange)
	}
	if _, err := os.Stat(filepath.Join(installDir, "symphony")); !os.IsNotExist(err) {
		t.Fatalf("install path should stay untouched, err=%v", err)
	}
	if _, err := os.Stat(markerFile); !os.IsNotExist(err) {
		t.Fatalf("go build should not run when git hash is unchanged, err=%v", err)
	}
}

func TestRunSelfUpdateHelperInstallsBuiltBinary(t *testing.T) {
	toolsDir := t.TempDir()
	repoDir := t.TempDir()
	installDir := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "git-hash")
	finalPath := filepath.Join(installDir, "symphony")

	if err := os.WriteFile(stateFile, []byte("old-hash\n"), 0o644); err != nil {
		t.Fatalf("write git hash: %v", err)
	}
	if err := os.WriteFile(finalPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("seed existing binary: %v", err)
	}
	writeFakeTool(t, filepath.Join(toolsDir, "git"), fakeGitScript)
	writeFakeTool(t, filepath.Join(toolsDir, "go"), fakeGoScript)

	t.Setenv("PATH", toolsDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_GIT_STATE_FILE", stateFile)
	t.Setenv("FAKE_GIT_NEXT_HASH", "new-hash")
	t.Setenv("FAKE_GO_BINARY_CONTENT", "new-binary")

	code := RunSelfUpdateHelper(repoDir, installDir)
	if code != updateHelperUpdated {
		t.Fatalf("code = %d, want %d", code, updateHelperUpdated)
	}

	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Fatalf("installed binary = %q, want %q", got, "new-binary")
	}
}

func TestRunSelfUpdateHelperBuildFailureKeepsExistingBinary(t *testing.T) {
	toolsDir := t.TempDir()
	repoDir := t.TempDir()
	installDir := t.TempDir()
	stateFile := filepath.Join(t.TempDir(), "git-hash")
	finalPath := filepath.Join(installDir, "symphony")

	if err := os.WriteFile(stateFile, []byte("old-hash\n"), 0o644); err != nil {
		t.Fatalf("write git hash: %v", err)
	}
	if err := os.WriteFile(finalPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("seed existing binary: %v", err)
	}
	writeFakeTool(t, filepath.Join(toolsDir, "git"), fakeGitScript)
	writeFakeTool(t, filepath.Join(toolsDir, "go"), fakeGoScript)

	t.Setenv("PATH", toolsDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_GIT_STATE_FILE", stateFile)
	t.Setenv("FAKE_GIT_NEXT_HASH", "new-hash")
	t.Setenv("FAKE_GO_FAIL", "1")

	code := RunSelfUpdateHelper(repoDir, installDir)
	if code == updateHelperUpdated || code == updateHelperNoChange {
		t.Fatalf("code = %d, want build failure", code)
	}

	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read existing binary: %v", err)
	}
	if string(got) != "old-binary" {
		t.Fatalf("existing binary = %q, want %q", got, "old-binary")
	}
}

func writeFakeTool(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake tool %s: %v", path, err)
	}
}

const fakeGitScript = `#!/bin/sh
set -eu

case "$1" in
  rev-parse)
    cat "$FAKE_GIT_STATE_FILE"
    ;;
  fetch)
    exit 0
    ;;
  pull)
    if [ -n "${FAKE_GIT_NEXT_HASH:-}" ]; then
      printf '%s\n' "$FAKE_GIT_NEXT_HASH" > "$FAKE_GIT_STATE_FILE"
    fi
    exit 0
    ;;
  *)
    echo "unexpected git args: $*" >&2
    exit 99
    ;;
esac
`

const fakeGoScript = `#!/bin/sh
set -eu

if [ -n "${FAKE_GO_FAIL:-}" ]; then
  echo "build failed" >&2
  exit 1
fi

out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then
    out="$arg"
    break
  fi
  prev="$arg"
done

if [ -z "$out" ]; then
  echo "missing -o output path" >&2
  exit 98
fi

mkdir -p "$(dirname "$out")"
printf '%s' "${FAKE_GO_BINARY_CONTENT:-built-binary}" > "$out"
chmod 755 "$out"

if [ -n "${FAKE_GO_MARKER_FILE:-}" ]; then
  : > "$FAKE_GO_MARKER_FILE"
fi
`
