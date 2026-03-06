package config

import "testing"

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
