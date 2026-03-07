// Package workspace manages per-issue working directories and lifecycle hooks.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Workspace represents a per-issue working directory.
type Workspace struct {
	Path       string
	Key        string // sanitized identifier
	CreatedNow bool
}

var identRe = regexp.MustCompile(`[^A-Za-z0-9._\-]`)

const (
	symphonyStateDir   = ".symphony"
	afterRunOutputFile = "after_run.stdout"
	hookKillGrace      = 10 * time.Second
)

// SanitizeIdentifier replaces unsafe characters with underscores.
func SanitizeIdentifier(id string) string {
	return identRe.ReplaceAllString(id, "_")
}

// Manager manages workspace directories.
type Manager struct {
	root           string
	hooks          map[string]string
	hooksTimeoutMs int
}

func NewManager(root string, hooks map[string]string, hooksTimeoutMs int) (*Manager, error) {
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace root %s: %w", root, err)
	}
	if hooks == nil {
		hooks = map[string]string{}
	}
	return &Manager{root: root, hooks: hooks, hooksTimeoutMs: hooksTimeoutMs}, nil
}

// Setup creates or reuses a workspace for the given identifier,
// running the after_create hook if newly created.
func (m *Manager) Setup(ctx context.Context, identifier string) (*Workspace, error) {
	key := SanitizeIdentifier(identifier)
	wsPath := filepath.Join(m.root, key)

	// Path containment check.
	clean := filepath.Clean(wsPath)
	if !strings.HasPrefix(clean+string(filepath.Separator), m.root+string(filepath.Separator)) &&
		clean != m.root {
		return nil, fmt.Errorf("path containment violation: %s not under %s", clean, m.root)
	}

	created := false
	if _, err := os.Stat(wsPath); os.IsNotExist(err) {
		if err := os.MkdirAll(wsPath, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", wsPath, err)
		}
		created = true
	}

	ws := &Workspace{Path: wsPath, Key: key, CreatedNow: created}

	if created {
		if script, ok := m.hooks["after_create"]; ok {
			if _, err := m.runHook(ctx, "after_create", script, wsPath); err != nil {
				// Fatal: clean up the partially created workspace.
				_ = os.RemoveAll(wsPath)
				return nil, fmt.Errorf("after_create hook: %w", err)
			}
		}
	}

	return ws, nil
}

// PrepareForRun runs the before_run hook (failure is fatal).
func (m *Manager) PrepareForRun(ctx context.Context, ws *Workspace) error {
	script, ok := m.hooks["before_run"]
	if !ok {
		return nil
	}
	_, err := m.runHook(ctx, "before_run", script, ws.Path)
	return err
}

// FinishRun runs the after_run hook (failure is logged but not fatal).
func (m *Manager) FinishRun(ctx context.Context, ws *Workspace) (string, error) {
	script, ok := m.hooks["after_run"]
	if !ok {
		return "", nil
	}
	// Non-fatal: return error for caller to log.
	stdout, err := m.runHook(ctx, "after_run", script, ws.Path)
	if persistErr := m.persistAfterRunOutput(ws.Path, stdout); persistErr != nil {
		if err != nil {
			return stdout, fmt.Errorf("%w; persist after_run output: %v", err, persistErr)
		}
		return stdout, persistErr
	}
	return stdout, err
}

// Cleanup runs before_remove (non-fatal) then deletes the workspace.
func (m *Manager) Cleanup(ctx context.Context, ws *Workspace) error {
	if script, ok := m.hooks["before_remove"]; ok {
		_, _ = m.runHook(ctx, "before_remove", script, ws.Path)
	}
	if err := os.RemoveAll(ws.Path); err != nil {
		return fmt.Errorf("remove workspace %s: %w", ws.Path, err)
	}
	return nil
}

// GetTurnContext returns a concise summary of workspace progress for follow-up prompts.
func (m *Manager) GetTurnContext(ws *Workspace) (string, error) {
	if ws == nil || strings.TrimSpace(ws.Path) == "" {
		return "", fmt.Errorf("workspace is required")
	}

	diffStat, err := GitOutput(ws.Path, "diff", "HEAD", "--stat")
	if err != nil {
		return "", err
	}
	gitLog, err := GitOutput(ws.Path, "log", "--oneline", "-5")
	if err != nil {
		return "", err
	}
	hookOutput, err := readAfterRunOutput(ws.Path)
	if err != nil {
		return "", err
	}

	var sections []string
	if trimmed := strings.TrimSpace(diffStat); trimmed != "" {
		sections = append(sections, "Git diff summary:\n"+trimmed)
	}
	if trimmed := strings.TrimSpace(gitLog); trimmed != "" {
		sections = append(sections, "Recent commits:\n"+trimmed)
	}
	if trimmed := strings.TrimSpace(hookOutput); trimmed != "" {
		sections = append(sections, "Latest after_run hook output:\n"+trimmed)
	}
	if len(sections) == 0 {
		return "", nil
	}
	return strings.Join(sections, "\n\n"), nil
}

func (m *Manager) runHook(ctx context.Context, name, script, wsPath string) (string, error) {
	timeout := time.Duration(m.hooksTimeoutMs) * time.Millisecond
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command("bash", "-lc", script)
	cmd.Dir = wsPath
	cmd.Env = append(os.Environ(), "SYMPHONY_WORKSPACE="+wsPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("hook '%s' start: %w", name, err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var err error
	var cause error
	select {
	case err = <-done:
	case <-hctx.Done():
		cause = hctx.Err()
		if waitErr, finished := gracefullyStopProcessGroup(cmd, done, hookKillGrace); finished {
			err = waitErr
		} else {
			err = <-done
		}
	}

	stdoutText := strings.TrimSpace(stdout.String())
	stderrText := strings.TrimSpace(stderr.String())
	if cause != nil {
		if ctx.Err() != nil {
			return stdoutText, fmt.Errorf("hook '%s' cancelled: %w", name, cause)
		}
		return stdoutText, fmt.Errorf("hook '%s' timed out after %dms", name, m.hooksTimeoutMs)
	}
	if err != nil {
		detail := strings.TrimSpace(strings.Join([]string{stdoutText, stderrText}, "\n"))
		return stdoutText, fmt.Errorf("hook '%s' failed (exit %v): %s", name, cmd.ProcessState, detail)
	}
	return stdoutText, nil
}

func gracefullyStopProcessGroup(cmd *exec.Cmd, done <-chan error, grace time.Duration) (error, bool) {
	if cmd == nil || cmd.Process == nil {
		return nil, false
	}
	_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGTERM)

	select {
	case err := <-done:
		return err, true
	case <-time.After(grace):
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		return nil, false
	}
}

func signalProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, sig)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (m *Manager) persistAfterRunOutput(wsPath, stdout string) error {
	stateDir := filepath.Join(wsPath, symphonyStateDir)
	outputPath := filepath.Join(stateDir, afterRunOutputFile)
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove after_run output %s: %w", outputPath, err)
		}
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	if err := os.WriteFile(outputPath, []byte(stdout+"\n"), 0o644); err != nil {
		return fmt.Errorf("write after_run output %s: %w", outputPath, err)
	}
	return nil
}

func readAfterRunOutput(wsPath string) (string, error) {
	outputPath := filepath.Join(wsPath, symphonyStateDir, afterRunOutputFile)
	data, err := os.ReadFile(outputPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read after_run output %s: %w", outputPath, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// GitOutput runs a git command in wsPath and returns trimmed stdout.
func GitOutput(wsPath string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", wsPath}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
