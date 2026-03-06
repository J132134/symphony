package config

import (
	"net"
	"os"
	"strings"
	"testing"
)

func TestTrackerFeedbackConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg := New(nil)

	if !cfg.TrackerPostComments() {
		t.Fatal("TrackerPostComments should default to true")
	}
	if got := cfg.TrackerOnSuccessState(); got != "" {
		t.Fatalf("TrackerOnSuccessState = %q, want empty", got)
	}
	if got := cfg.TrackerOnFailureState(); got != "" {
		t.Fatalf("TrackerOnFailureState = %q, want empty", got)
	}
	if got := cfg.TrackerPRURLTemplate(); got != "" {
		t.Fatalf("TrackerPRURLTemplate = %q, want empty", got)
	}
	if got := cfg.MaxAttempts(); got != 3 {
		t.Fatalf("MaxAttempts = %d, want 3", got)
	}
}

func TestTrackerFeedbackConfigOverrides(t *testing.T) {
	t.Parallel()

	cfg := New(map[string]any{
		"tracker": map[string]any{
			"post_comments":    false,
			"on_success_state": "Human Review",
			"on_failure_state": "Rework",
			"pr_url_template":  "https://example.com/{branch}",
		},
		"agent": map[string]any{
			"max_attempts": 5,
		},
	})

	if cfg.TrackerPostComments() {
		t.Fatal("TrackerPostComments should honor explicit false")
	}
	if got := cfg.TrackerOnSuccessState(); got != "Human Review" {
		t.Fatalf("TrackerOnSuccessState = %q, want Human Review", got)
	}
	if got := cfg.TrackerOnFailureState(); got != "Rework" {
		t.Fatalf("TrackerOnFailureState = %q, want Rework", got)
	}
	if got := cfg.TrackerPRURLTemplate(); got != "https://example.com/{branch}" {
		t.Fatalf("TrackerPRURLTemplate = %q", got)
	}
	if got := cfg.MaxAttempts(); got != 5 {
		t.Fatalf("MaxAttempts = %d, want 5", got)
	}
}

func TestSymphonyConfigValidateRejectsEmptyWorkspaceRoot(t *testing.T) {
	t.Parallel()

	cfg := New(map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "proj",
		},
		"workspace": map[string]any{
			"root": "",
		},
		"server": map[string]any{
			"port": 18081,
		},
	})

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "workspace.root")
}

func TestSymphonyConfigValidateRejectsInvalidRuntimeValues(t *testing.T) {
	t.Parallel()

	ln, port := listenOnRandomPort(t)
	defer ln.Close()

	cfg := New(map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "proj",
		},
		"workspace": map[string]any{
			"root": t.TempDir(),
		},
		"server": map[string]any{
			"port": port,
		},
		"polling": map[string]any{
			"interval_ms":      0,
			"idle_interval_ms": -1,
		},
		"agent": map[string]any{
			"max_concurrent_agents": 0,
			"max_turns":             0,
		},
		"codex": map[string]any{
			"command": "codex app-server",
		},
	})

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "server.port")
	requireErrorContaining(t, errs, "already in use")
	requireErrorContaining(t, errs, "polling.interval_ms")
	requireErrorContaining(t, errs, "idle_interval_ms")
	requireErrorContaining(t, errs, "agent.max_concurrent_agents")
	requireErrorContaining(t, errs, "agent.max_turns")
}

func TestSymphonyConfigValidateRejectsUncreatableWorkspaceRootAndPortRange(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	blocker := parent + "/blocked"
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	cfg := New(map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "proj",
		},
		"workspace": map[string]any{
			"root": blocker + "/child",
		},
		"server": map[string]any{
			"port": 70000,
		},
		"polling": map[string]any{
			"interval_ms":      1000,
			"idle_interval_ms": 1000,
		},
		"agent": map[string]any{
			"max_concurrent_agents": 1,
			"max_turns":             1,
		},
		"codex": map[string]any{
			"command": "codex app-server",
		},
	})

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "workspace.root")
	requireErrorContaining(t, errs, "server.port must be between 1 and 65535")
}

func listenOnRandomPort(t *testing.T) (net.Listener, int) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener addr type %T", ln.Addr())
	}
	return ln, addr.Port
}

func requireErrorContaining(t *testing.T, errs []string, want string) {
	t.Helper()

	for _, err := range errs {
		if strings.Contains(err, want) {
			return
		}
	}
	t.Fatalf("Validate() errors %q do not contain %q", errs, want)
}
