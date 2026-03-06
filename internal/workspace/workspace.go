// Package workspace manages per-issue working directories and lifecycle hooks.
package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Workspace represents a per-issue working directory.
type Workspace struct {
	Path         string
	Key          string // sanitized identifier
	CreatedNow   bool
}

var identRe = regexp.MustCompile(`[^A-Za-z0-9._\-]`)

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
			if err := m.runHook(ctx, "after_create", script, wsPath); err != nil {
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
	return m.runHook(ctx, "before_run", script, ws.Path)
}

// FinishRun runs the after_run hook (failure is logged but not fatal).
func (m *Manager) FinishRun(ctx context.Context, ws *Workspace) error {
	script, ok := m.hooks["after_run"]
	if !ok {
		return nil
	}
	// Non-fatal: return error for caller to log.
	return m.runHook(ctx, "after_run", script, ws.Path)
}

// Cleanup runs before_remove (non-fatal) then deletes the workspace.
func (m *Manager) Cleanup(ctx context.Context, ws *Workspace) error {
	if script, ok := m.hooks["before_remove"]; ok {
		_ = m.runHook(ctx, "before_remove", script, ws.Path)
	}
	if err := os.RemoveAll(ws.Path); err != nil {
		return fmt.Errorf("remove workspace %s: %w", ws.Path, err)
	}
	return nil
}

// CleanupByKey is a convenience to cleanup by identifier when no Workspace object is available.
func (m *Manager) CleanupByKey(ctx context.Context, identifier string) error {
	key := SanitizeIdentifier(identifier)
	wsPath := filepath.Join(m.root, key)
	ws := &Workspace{Path: wsPath, Key: key}
	return m.Cleanup(ctx, ws)
}

// Exists reports whether a workspace exists for the given identifier.
func (m *Manager) Exists(identifier string) bool {
	key := SanitizeIdentifier(identifier)
	_, err := os.Stat(filepath.Join(m.root, key))
	return err == nil
}

func (m *Manager) runHook(ctx context.Context, name, script, wsPath string) error {
	timeout := time.Duration(m.hooksTimeoutMs) * time.Millisecond
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(hctx, "bash", "-lc", script)
	cmd.Dir = wsPath
	cmd.Env = append(os.Environ(), "SYMPHONY_WORKSPACE="+wsPath)

	out, err := cmd.CombinedOutput()
	if err != nil {
		if hctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("hook '%s' timed out after %dms", name, m.hooksTimeoutMs)
		}
		return fmt.Errorf("hook '%s' failed (exit %v): %s", name, cmd.ProcessState, strings.TrimSpace(string(out)))
	}
	return nil
}
