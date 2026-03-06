package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDaemonConfigUsesDynamicSessionDefault(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	configYAML := "projects:\n  - name: alpha\n    workflow: " + workflowPath + "\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadDaemonConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if got, want := cfg.MaxTotalConcurrentSessions(), DefaultMaxTotalConcurrentSessions(); got != want {
		t.Fatalf("MaxTotalConcurrentSessions() = %d, want %d", got, want)
	}
}

func TestLoadDaemonConfigOverridesSessionLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	configYAML := "projects:\n  - name: alpha\n    workflow: " + workflowPath + "\nagent:\n  max_total_concurrent_sessions: 5\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadDaemonConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if got := cfg.MaxTotalConcurrentSessions(); got != 5 {
		t.Fatalf("MaxTotalConcurrentSessions() = %d, want 5", got)
	}
}

func TestDaemonConfigValidateRejectsInvalidConfiguredSessionLimit(t *testing.T) {
	t.Parallel()

	cfg := &DaemonConfig{
		Projects:                             []ProjectConfig{{Name: "alpha", Workflow: "/tmp/workflow.md"}},
		Agent:                                DaemonAgentConfig{MaxTotalConcurrentSessions: 0},
		maxTotalConcurrentSessionsConfigured: true,
	}

	errs := cfg.Validate()
	if len(errs) == 0 {
		t.Fatal("Validate() returned no errors for invalid session limit")
	}
}
