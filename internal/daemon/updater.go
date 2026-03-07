package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"symphony/internal/update"
	"symphony/internal/version"
)

var (
	prepareUpdateFn  = defaultPrepareUpdate
	validateUpdateFn = validateUpdateAsset
	updaterExitFn    = os.Exit
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

	tempPath, err := checker.Download(result.DownloadURL)
	if err != nil {
		return false, fmt.Errorf("download update: %w", err)
	}
	defer os.Remove(tempPath)

	if err := validateUpdateFn(tempPath); err != nil {
		return false, fmt.Errorf("validate update asset: %w", err)
	}

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

func validateUpdateAsset(path string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}

	out, err := exec.Command("codesign", "-dv", "--verbose=4", path).CombinedOutput()
	details := string(out)
	if err != nil {
		return fmt.Errorf("inspect macOS code signature: %w: %s", err, strings.TrimSpace(details))
	}
	if err := validateMacOSSignatureDetails(details); err != nil {
		return err
	}
	return nil
}

func validateMacOSSignatureDetails(details string) error {
	if strings.EqualFold(codesignField(details, "Signature"), "adhoc") {
		return fmt.Errorf("macOS update asset must be Developer ID signed, got ad-hoc signature")
	}

	if !codesignAuthorityContains(details, "Developer ID Application:") {
		return fmt.Errorf("macOS update asset must be signed with a Developer ID Application certificate")
	}

	teamID := codesignField(details, "TeamIdentifier")
	if teamID == "" || strings.EqualFold(teamID, "not set") {
		return fmt.Errorf("macOS update asset must include a TeamIdentifier")
	}

	return nil
}

func codesignField(details, key string) string {
	prefix := key + "="
	for _, line := range strings.Split(details, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func codesignAuthorityContains(details, want string) bool {
	for _, line := range strings.Split(details, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Authority=") && strings.Contains(line, want) {
			return true
		}
	}
	return false
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
