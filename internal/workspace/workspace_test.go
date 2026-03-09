package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
	if err := os.WriteFile(filepath.Join(ws.Path, ".git"), []byte("gitdir: "+filepath.Join(ws.Path, ".missing")+"\n"), 0o644); err != nil {
		t.Fatalf("write fake git file: %v", err)
	}

	_, err = manager.GetTurnContext(ws)
	if err == nil {
		t.Fatal("GetTurnContext() error = nil, want git failure")
	}
	if !strings.Contains(err.Error(), "git diff HEAD --stat") {
		t.Fatalf("GetTurnContext() error = %q, want git diff context", err)
	}
}

func TestGitWritablePathsReturnsStandardRepoGitDirOnce(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	initGitWorkspace(t, wsPath)

	got, err := GitWritablePaths(wsPath)
	if err != nil {
		t.Fatalf("GitWritablePaths: %v", err)
	}

	want := gitOutput(t, wsPath, "rev-parse", "--path-format=absolute", "--git-dir")
	if len(got) != 1 {
		t.Fatalf("len(GitWritablePaths) = %d, want 1 (%v)", len(got), got)
	}
	if got[0] != want {
		t.Fatalf("GitWritablePaths[0] = %q, want %q", got[0], want)
	}
}

func TestGitWritablePathsReturnsWorktreeAdminAndCommonDirs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mainPath := filepath.Join(root, "main")
	if err := os.MkdirAll(mainPath, 0o755); err != nil {
		t.Fatalf("mkdir main repo: %v", err)
	}
	initGitWorkspace(t, mainPath)

	worktreePath := filepath.Join(root, "linked-worktree")
	runGit(t, mainPath, "branch", "feature/worktree")
	runGit(t, mainPath, "worktree", "add", worktreePath, "feature/worktree")

	got, err := GitWritablePaths(worktreePath)
	if err != nil {
		t.Fatalf("GitWritablePaths: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(GitWritablePaths) = %d, want 2 (%v)", len(got), got)
	}

	wantGitDir := gitOutput(t, worktreePath, "rev-parse", "--path-format=absolute", "--git-dir")
	wantCommonDir := gitOutput(t, worktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if got[0] != wantGitDir {
		t.Fatalf("GitWritablePaths[0] = %q, want %q", got[0], wantGitDir)
	}
	if got[1] != wantCommonDir {
		t.Fatalf("GitWritablePaths[1] = %q, want %q", got[1], wantCommonDir)
	}
}

func TestGitWritablePathsReturnsNilForPlainDirectory(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()

	got, err := GitWritablePaths(wsPath)
	if err != nil {
		t.Fatalf("GitWritablePaths: %v", err)
	}
	if got != nil {
		t.Fatalf("GitWritablePaths = %v, want nil", got)
	}
}

func TestGitWritablePathsReturnsErrorWhenGitMetadataUnavailable(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsPath, ".git"), []byte("gitdir: "+filepath.Join(wsPath, ".missing")+"\n"), 0o644); err != nil {
		t.Fatalf("write fake git file: %v", err)
	}

	_, err := GitWritablePaths(wsPath)
	if err == nil {
		t.Fatal("GitWritablePaths() error = nil, want git failure")
	}
	if !strings.Contains(err.Error(), "git rev-parse --path-format=absolute --git-dir") {
		t.Fatalf("GitWritablePaths() error = %q, want git rev-parse context", err)
	}
}

func TestFinishRunHonorsParentDrainDeadline(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	wsPath := filepath.Join(root, "J-31")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	manager, err := NewManager(root, map[string]string{
		"after_run": "sleep 1",
	}, 2_000)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = manager.FinishRun(ctx, &Workspace{Path: wsPath, Key: "J-31"})
	if err == nil {
		t.Fatal("FinishRun() error = nil, want deadline failure")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("FinishRun elapsed = %v, want parent deadline to cut it short", elapsed)
	}
}

func TestRunHookGracefullyStopsProcessGroupOnDeadline(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "darwin" {
		t.Skip("non-interactive bash signal handling is not reliable enough on macOS for this assertion")
	}

	root := t.TempDir()
	wsPath := filepath.Join(root, "J-31")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	manager, err := NewManager(root, nil, 2_000)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	marker := filepath.Join(wsPath, "term.txt")
	script := fmt.Sprintf("trap 'echo term > %q; exit 0' TERM; while true; do sleep 1; done", marker)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = manager.runHook(ctx, "after_run", script, wsPath)
	if err == nil {
		t.Fatal("runHook() error = nil, want deadline failure")
	}
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Fatalf("runHook elapsed = %v, want graceful stop within parent deadline window", elapsed)
	}
	if got, readErr := os.ReadFile(marker); readErr != nil {
		t.Fatalf("expected TERM trap marker, read error = %v", readErr)
	} else if strings.TrimSpace(string(got)) != "term" {
		t.Fatalf("TERM trap marker = %q, want %q", strings.TrimSpace(string(got)), "term")
	}
}

func TestCleanupHonorsParentDrainDeadline(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	wsPath := filepath.Join(root, "J-31")
	if err := os.MkdirAll(wsPath, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	manager, err := NewManager(root, map[string]string{
		"before_remove": "sleep 1",
	}, 2_000)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := manager.Cleanup(ctx, &Workspace{Path: wsPath, Key: "J-31"}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Cleanup elapsed = %v, want parent deadline to cut it short", elapsed)
	}
	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Fatalf("workspace should be removed, err=%v", err)
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

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
