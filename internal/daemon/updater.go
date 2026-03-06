package daemon

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	updateHelperUpdated  = 0
	updateHelperNoChange = 10
)

// CheckForUpdates prepares an updated binary out-of-process and exits so an external supervisor can restart it.
func CheckForUpdates(mgr *Manager, repoDir string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("updater.panic", "error", r)
		}
	}()

	updated, err := prepareUpdate(repoDir)
	if err != nil {
		slog.Warn("updater.prepare_failed", "error", err)
		return
	}
	if !updated {
		return
	}

	slog.Info("updater.updated_waiting_for_idle")
	<-mgr.RequestRestartWhenIdle()
	mgr.Shutdown()
	mgr.Wait()
	slog.Info("updater.updated_restarting_via_supervisor")
	os.Exit(0)
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

func prepareUpdate(repoDir string) (bool, error) {
	if repoDir == "" {
		return false, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("resolve executable path: %w", err)
	}
	installDir := filepath.Dir(exe)

	cmd := exec.Command(exe, "self-update-helper", "--repo-dir", repoDir, "--install-dir", installDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	err = cmd.Run()
	code := exitCode(err)
	if err != nil {
		if code == updateHelperNoChange {
			return false, nil
		}
		return false, fmt.Errorf("run self-update helper: %w", err)
	}
	return code == updateHelperUpdated, nil
}

// RunSelfUpdateHelper executes the isolated update flow used by daemon auto-update.
func RunSelfUpdateHelper(repoDir, installDir string) int {
	if repoDir == "" || installDir == "" {
		slog.Error("updater.invalid_helper_args", "repo_dir", repoDir, "install_dir", installDir)
		return 1
	}

	before := getGitHash(repoDir)
	if before == "" {
		slog.Warn("updater.git_hash_failed", "repo_dir", repoDir)
		return 1
	}

	fetch := exec.Command("git", "fetch", "--quiet")
	fetch.Dir = repoDir
	if out, err := fetch.CombinedOutput(); err != nil {
		slog.Warn("updater.git_fetch_failed", "stderr", string(out))
		return 1
	}

	pull := exec.Command("git", "pull", "--ff-only")
	pull.Dir = repoDir
	if out, err := pull.CombinedOutput(); err != nil {
		slog.Warn("updater.git_pull_failed", "stderr", string(out))
		return 1
	}

	after := getGitHash(repoDir)
	if before == after || after == "" {
		return updateHelperNoChange
	}

	tempDir, err := os.MkdirTemp("", "symphony-update-*")
	if err != nil {
		slog.Error("updater.tempdir_failed", "error", err)
		return 1
	}
	defer os.RemoveAll(tempDir)

	builtBinary := filepath.Join(tempDir, "symphony")
	build := exec.Command("go", "build", "-o", builtBinary, "./cmd/symphony")
	build.Dir = repoDir
	if out, err := build.CombinedOutput(); err != nil {
		slog.Error("updater.build_failed", "output", truncateStr(string(out), 500), "error", err)
		return 1
	}

	finalPath := filepath.Join(installDir, filepath.Base(builtBinary))
	if err := installBuiltBinary(builtBinary, finalPath); err != nil {
		slog.Error("updater.install_failed", "target", finalPath, "error", err)
		return 1
	}

	slog.Info("updater.git_updated", "before", before, "after", after)
	return updateHelperUpdated
}

func installBuiltBinary(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create install dir: %w", err)
	}

	staged := dst + ".new"
	if err := copyFile(src, staged, 0o755); err != nil {
		return err
	}
	if err := os.Rename(staged, dst); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source binary: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create staged binary: %w", err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy binary: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close staged binary: %w", err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("chmod staged binary: %w", err)
	}
	return nil
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

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if ok := errors.As(err, &exitErr); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
