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
func CheckForUpdates(mgr *Manager, repoDir string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("updater.panic", "error", r)
		}
	}()

	if !tryGitUpdate(repoDir) {
		return
	}

	slog.Info("updater.updated_waiting_for_idle")
	<-mgr.RequestRestartWhenIdle()
	slog.Info("updater.updated_restarting", "exit_code", RestartExitCode)
	os.Exit(RestartExitCode)
}

func getGitHash(dir string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
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
	if repoDir == "" {
		return false
	}

	before := getGitHash(repoDir)

	fetch := exec.Command("git", "fetch", "--quiet")
	fetch.Dir = repoDir
	if out, err := fetch.CombinedOutput(); err != nil {
		slog.Warn("updater.git_fetch_failed", "stderr", string(out))
		return false
	}

	pull := exec.Command("git", "pull", "--ff-only")
	pull.Dir = repoDir
	if out, err := pull.CombinedOutput(); err != nil {
		slog.Warn("updater.git_pull_failed", "stderr", string(out))
		return false
	}

	after := getGitHash(repoDir)
	if before == after || after == "" {
		return false
	}

	exe, err := os.Executable()
	if err != nil {
		slog.Error("updater.executable_path_failed", "error", err)
		return false
	}
	installDir := filepath.Dir(exe)
	build := exec.Command("make", "install", "INSTALL_DIR="+installDir)
	build.Dir = repoDir
	out, err := build.CombinedOutput()
	if err != nil {
		slog.Error("updater.make_install_failed", "output", truncateStr(string(out), 500), "error", err)
		return false
	}

	slog.Info("updater.git_updated", "before", before, "after", after)
	return true
}

// RunUpdateLoop runs CheckForUpdates every intervalMinutes until stop is closed.
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
