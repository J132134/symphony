package config

import (
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
		"codex": map[string]any{
			"command": "codex app-server",
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
	if got := cfg.CodexCommand(); got != "codex app-server" {
		t.Fatalf("CodexCommand() = %q, want codex app-server", got)
	}
}

func TestPauseStatesDefaultsToHumanReview(t *testing.T) {
	t.Parallel()

	cfg := New(nil)

	got := cfg.PauseStates()
	if len(got) != 1 || got[0] != "Human Review" {
		t.Fatalf("PauseStates() = %v, want [Human Review]", got)
	}
	if !cfg.PauseNorm()["human review"] {
		t.Fatal("PauseNorm() should include normalized human review")
	}
}

func TestPauseStatesOverrideAndNormalize(t *testing.T) {
	t.Parallel()

	cfg := New(map[string]any{
		"tracker": map[string]any{
			"pause_states": []any{" Plan Review ", "Human Review", "QA Hold"},
		},
	})

	got := cfg.PauseStates()
	want := []string{"Plan Review", "Human Review", "QA Hold"}
	if len(got) != len(want) {
		t.Fatalf("PauseStates() len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PauseStates()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, state := range []string{"plan review", "human review", "qa hold"} {
		if !cfg.PauseNorm()[state] {
			t.Fatalf("PauseNorm() missing %q", state)
		}
	}
}

func TestDrainTimeoutDefaultsToStallPlusHooks(t *testing.T) {
	t.Parallel()

	cfg := New(map[string]any{
		"codex": map[string]any{
			"stall_timeout_ms": 300_000,
		},
		"hooks": map[string]any{
			"timeout_ms": 60_000,
		},
	})

	if got := cfg.DrainTimeoutMs(); got != 360_000 {
		t.Fatalf("DrainTimeoutMs() = %d, want 360000", got)
	}
}

func TestDrainTimeoutHonorsExplicitOverride(t *testing.T) {
	t.Parallel()

	cfg := New(map[string]any{
		"daemon": map[string]any{
			"drain_timeout_ms": 123_000,
		},
		"codex": map[string]any{
			"stall_timeout_ms": 300_000,
		},
		"hooks": map[string]any{
			"timeout_ms": 60_000,
		},
	})

	if got := cfg.DrainTimeoutMs(); got != 123_000 {
		t.Fatalf("DrainTimeoutMs() = %d, want 123000", got)
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
	})

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "workspace.root")
}

func TestSymphonyConfigValidateRejectsInvalidRuntimeValues(t *testing.T) {
	t.Parallel()

	cfg := New(map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "proj",
		},
		"workspace": map[string]any{
			"root": t.TempDir(),
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
	requireErrorContaining(t, errs, "polling.interval_ms")
	requireErrorContaining(t, errs, "idle_interval_ms")
	requireErrorContaining(t, errs, "agent.max_concurrent_agents")
	requireErrorContaining(t, errs, "agent.max_turns")
}

func TestSymphonyConfigValidateRejectsInvalidDrainTimeout(t *testing.T) {
	t.Parallel()

	cfg := New(map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "proj",
		},
		"workspace": map[string]any{
			"root": t.TempDir(),
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
		"daemon": map[string]any{
			"drain_timeout_ms": 0,
		},
	})

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "daemon.drain_timeout_ms")
}

func TestSymphonyConfigValidateRejectsUncreatableWorkspaceRoot(t *testing.T) {
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
