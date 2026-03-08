package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"symphony/internal/update"
	"symphony/internal/version"
)

var (
	prepareUpdateFn = defaultPrepareUpdate
	updaterExitFn   = os.Exit
	renameFileFn    = os.Rename
	removeFileFn    = os.Remove
)

// CheckForUpdates checks GitHub Releases for a newer binary, installs it, and
// exits so launchd can restart the process.
func CheckForUpdates(mgr *Manager) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("updater.panic", "error", r)
		}
	}()

	updated, err := prepareUpdateFn()
	if err != nil {
		slog.Warn("updater.prepare_failed", "error", err)
		return
	}
	if !updated {
		return
	}

	slog.Info("updater.updated_draining")
	mgr.Shutdown()
	mgr.Wait()
	slog.Info("updater.updated_restarting_via_supervisor")
	updaterExitFn(0)
}

func defaultPrepareUpdate() (bool, error) {
	cur := version.Current()
	checker := update.Checker{
		Owner: "J132134",
		Repo:  "symphony",
		Asset: assetName(),
	}

	result, err := checker.Check(cur.Version)
	if err != nil {
		return false, fmt.Errorf("check for update: %w", err)
	}
	if !result.Available {
		slog.Debug("updater.no_update", "current", result.CurrentVer, "latest", result.LatestVer)
		return false, nil
	}
	slog.Info("updater.update_available", "current", result.CurrentVer, "latest", result.LatestVer)

	tempPath, err := checker.Download(result.DownloadURL, result.ChecksumURL)
	if err != nil {
		return false, fmt.Errorf("download update: %w", err)
	}
	defer removeFileFn(tempPath)

	exe, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("resolve executable path: %w", err)
	}

	if err := installBuiltBinary(tempPath, exe); err != nil {
		return false, fmt.Errorf("install update: %w", err)
	}

	slog.Info("updater.installed", "version", result.LatestVer, "path", exe)
	return true, nil
}

func assetName() string {
	return fmt.Sprintf("symphony-%s-%s", runtime.GOOS, runtime.GOARCH)
}

func installBuiltBinary(src, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create install dir: %w", err)
	}

	mode, err := installMode(dst)
	if err != nil {
		return err
	}

	staged, err := os.CreateTemp(dir, filepath.Base(dst)+".new-*")
	if err != nil {
		return fmt.Errorf("create staged binary: %w", err)
	}
	stagedPath := staged.Name()
	if err := staged.Close(); err != nil {
		removeFileFn(stagedPath)
		return fmt.Errorf("close staged temp file: %w", err)
	}
	if err := copyFile(src, stagedPath, mode); err != nil {
		return err
	}

	backupPath := dst + ".bak"
	if err := ensureRollbackTarget(backupPath); err != nil {
		removeFileFn(stagedPath)
		return err
	}

	hadExisting := false
	if _, err := os.Lstat(dst); err == nil {
		hadExisting = true
		if err := renameFileFn(dst, backupPath); err != nil {
			removeFileFn(stagedPath)
			return fmt.Errorf("move current binary to backup: %w", err)
		}
	} else if !os.IsNotExist(err) {
		removeFileFn(stagedPath)
		return fmt.Errorf("stat current binary: %w", err)
	}

	if err := renameFileFn(stagedPath, dst); err != nil {
		removeFileFn(stagedPath)
		if hadExisting {
			if restoreErr := renameFileFn(backupPath, dst); restoreErr != nil {
				return fmt.Errorf("replace binary: %w (rollback failed: %v)", err, restoreErr)
			}
		}
		return fmt.Errorf("replace binary: %w", err)
	}

	if hadExisting {
		_ = removeFileFn(backupPath)
	}
	return nil
}

func installMode(dst string) (os.FileMode, error) {
	info, err := os.Lstat(dst)
	if os.IsNotExist(err) {
		return 0o755, nil
	}
	if err != nil {
		return 0, fmt.Errorf("stat current binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("current binary is not a regular file: %s", dst)
	}
	return info.Mode().Perm(), nil
}

func ensureRollbackTarget(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat backup path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("backup path is not a regular file: %s", path)
	}
	if err := removeFileFn(path); err != nil {
		return fmt.Errorf("remove stale backup: %w", err)
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
		_ = removeFileFn(dst)
		return fmt.Errorf("copy binary: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		_ = removeFileFn(dst)
		return fmt.Errorf("sync staged binary: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = removeFileFn(dst)
		return fmt.Errorf("close staged binary: %w", err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		_ = removeFileFn(dst)
		return fmt.Errorf("chmod staged binary: %w", err)
	}
	return nil
}

// RunUpdateLoop runs CheckForUpdates every intervalMinutes until stop is closed.
func RunUpdateLoop(mgr *Manager, intervalMinutes int, stop <-chan struct{}) {
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
			CheckForUpdates(mgr)
		}
	}
}
