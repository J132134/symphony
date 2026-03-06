package daemon

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const RestartExitCode = 42

// CheckForUpdates checks for a new version and restarts if updated.
// Supports two modes: uv tool install (upgrades the binary) and git dev (pull + uv sync).
func CheckForUpdates(mgr *Manager, repoDir string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("updater.panic", "error", r)
		}
	}()

	var updated bool
	if isUVToolInstall() {
		updated = tryUVToolUpgrade()
	} else {
		updated = tryGitUpdate(repoDir)
	}

	if !updated {
		return
	}

	slog.Info("updater.updated_restarting", "exit_code", RestartExitCode)
	mgr.Shutdown()
	// Give shutdown a moment before exiting.
	time.Sleep(2 * time.Second)
	os.Exit(RestartExitCode)
}

func isUVToolInstall() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	markers := []string{".local/share/uv/tools", ".uv/tools", "uv/tools"}
	for _, m := range markers {
		if strings.Contains(exe, m) {
			return true
		}
	}
	return false
}

func getBinaryMtime() time.Time {
	path, err := exec.LookPath("symphony")
	if err != nil {
		return time.Time{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func tryUVToolUpgrade() bool {
	before := getBinaryMtime()
	cmd := exec.Command("uv", "tool", "upgrade", "symphony")
	out, err := cmd.CombinedOutput()
	slog.Info("updater.uv_tool_upgrade", "output", truncateStr(string(out), 200), "error", err)
	if err != nil {
		return false
	}
	after := getBinaryMtime()
	return !after.IsZero() && after != before
}

func getGitHash(dir string) string {
	args := []string{"rev-parse", "HEAD"}
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func tryGitUpdate(repoDir string) bool {
	before := getGitHash(repoDir)

	fetch := exec.Command("git", "fetch", "--quiet")
	if repoDir != "" {
		fetch.Dir = repoDir
	}
	if out, err := fetch.CombinedOutput(); err != nil {
		slog.Warn("updater.git_fetch_failed", "stderr", string(out))
		return false
	}

	pull := exec.Command("git", "pull", "--ff-only")
	if repoDir != "" {
		pull.Dir = repoDir
	}
	if out, err := pull.CombinedOutput(); err != nil {
		slog.Warn("updater.git_pull_failed", "stderr", string(out))
		return false
	}

	after := getGitHash(repoDir)
	if before == after || after == "" {
		return false
	}

	sync := exec.Command("uv", "sync")
	if repoDir != "" {
		sync.Dir = repoDir
	}
	_ = sync.Run()

	slog.Info("updater.git_updated", "before", before, "after", after)
	return true
}

// RunUpdateLoop runs CheckForUpdates every intervalMinutes until ctx is cancelled.
func RunUpdateLoop(mgr *Manager, intervalMinutes int, repoDir string, stop <-chan struct{}) {
	if intervalMinutes <= 0 {
		intervalMinutes = 30
	}
	ticker := time.NewTicker(time.Duration(intervalMinutes) * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			CheckForUpdates(mgr, repoDir)
		}
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// WorkingDir returns the directory of the current executable (for git mode).
func WorkingDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}
