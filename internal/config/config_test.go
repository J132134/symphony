package config

import "testing"

func TestSymphonyConfigMaxRetryAttemptsDefault(t *testing.T) {
	t.Parallel()

	cfg := New(nil)
	if got := cfg.MaxRetryAttempts(); got != 0 {
		t.Fatalf("MaxRetryAttempts() = %d, want 0", got)
	}
}

func TestSymphonyConfigValidateRejectsNegativeRetryAttempts(t *testing.T) {
	t.Parallel()

	cfg := New(map[string]any{
		"tracker": map[string]any{
			"kind":         "linear",
			"api_key":      "token",
			"project_slug": "proj",
		},
		"codex": map[string]any{
			"command": "codex app-server",
		},
		"agent": map[string]any{
			"max_retry_attempts": -1,
		},
	})

	errs := cfg.Validate()
	if len(errs) == 0 {
		t.Fatal("Validate() returned no errors for negative retry attempts")
	}
}
