package config

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func validateReadableFile(path, label string) []string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{fmt.Sprintf("%s not found: %s", label, path)}
		}
		return []string{fmt.Sprintf("%s cannot be accessed: %v", label, err)}
	}
	if info.IsDir() {
		return []string{fmt.Sprintf("%s must be a readable file: %s", label, path)}
	}

	f, err := os.Open(path)
	if err != nil {
		return []string{fmt.Sprintf("%s is not readable: %s (%v)", label, path, err)}
	}
	_ = f.Close()
	return nil
}

func validateGitRepoDir(path, label string) []string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{fmt.Sprintf("%s not found: %s", label, path)}
		}
		return []string{fmt.Sprintf("%s cannot be accessed: %v", label, err)}
	}
	if !info.IsDir() {
		return []string{fmt.Sprintf("%s must be a directory: %s", label, path)}
	}

	cmd := exec.Command("git", "-C", path, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return []string{fmt.Sprintf("%s must be a git repository: %s", label, path)}
	}
	return nil
}

func validateCreatableWritableDir(path, label string) []string {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if !info.IsDir() {
			return []string{fmt.Sprintf("%s must be a directory: %s", label, path)}
		}
		if err := checkWritableDir(path); err != nil {
			return []string{fmt.Sprintf("%s is not writable: %s (%v)", label, path, err)}
		}
		return nil
	case !os.IsNotExist(err):
		return []string{fmt.Sprintf("%s cannot be accessed: %v", label, err)}
	}

	ancestor, err := nearestExistingAncestor(path)
	if err != nil {
		return []string{fmt.Sprintf("%s cannot be created: %v", label, err)}
	}
	if err := checkWritableDir(ancestor); err != nil {
		return []string{fmt.Sprintf("%s cannot be created under %s: %v", label, ancestor, err)}
	}
	return nil
}

func nearestExistingAncestor(path string) (string, error) {
	current := filepath.Clean(path)
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", current)
			}
			return current, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing parent for %s", path)
		}
		current = parent
	}
}

func checkWritableDir(path string) error {
	f, err := os.CreateTemp(path, ".symphony-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func validateTCPPortAvailable(port int, label string) []string {
	if port < 1 || port > 65535 {
		return []string{fmt.Sprintf("%s must be between 1 and 65535", label)}
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return []string{fmt.Sprintf("%s is already in use: %d", label, port)}
	}
	_ = ln.Close()
	return nil
}
