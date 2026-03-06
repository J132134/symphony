package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFinishRunPersistsHookOutputForTurnContext(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	wsPath := filepath.Join(root, "J-24")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	initGitWorkspace(t, wsPath)

	manager, err := NewManager(root, map[string]string{
		"after_run": "printf 'hook synced\\n'",
	}, 1_000)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ws := &Workspace{Path: wsPath, Key: "J-24"}
	stdout, err := manager.FinishRun(context.Background(), ws)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if stdout != "hook synced" {
		t.Fatalf("FinishRun stdout = %q, want %q", stdout, "hook synced")
	}

	turnContext, err := manager.GetTurnContext(ws)
	if err != nil {
		t.Fatalf("GetTurnContext: %v", err)
	}
	for _, want := range []string{
		"Git diff summary:",
		"foo.txt",
		"Recent commits:",
		"feat: seed workspace",
		"Latest after_run hook output:",
		"hook synced",
	} {
		if !strings.Contains(turnContext, want) {
			t.Fatalf("turn context missing %q:\n%s", want, turnContext)
		}
	}
}

func TestGetTurnContextReturnsErrorWhenGitMetadataUnavailable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	manager, err := NewManager(root, nil, 1_000)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ws := &Workspace{Path: filepath.Join(root, "plain"), Key: "plain"}
	if err := os.MkdirAll(ws.Path, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	_, err = manager.GetTurnContext(ws)
	if err == nil {
		t.Fatal("GetTurnContext() error = nil, want git failure")
	}
	if !strings.Contains(err.Error(), "git diff HEAD --stat") {
		t.Fatalf("GetTurnContext() error = %q, want git diff context", err)
	}
}

func initGitWorkspace(t *testing.T, dir string) {
	t.Helper()

	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")

	path := filepath.Join(dir, "foo.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write foo.txt: %v", err)
	}
	runGit(t, dir, "add", "foo.txt")
	runGit(t, dir, "commit", "-m", "feat: seed workspace")

	if err := os.WriteFile(path, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("modify foo.txt: %v", err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
