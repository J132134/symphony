package config

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

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

func TestLoadDaemonConfigOverridesProjectHealth(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	configYAML := "" +
		"projects:\n" +
		"  - name: alpha\n" +
		"    workflow: " + workflowPath + "\n" +
		"project_health:\n" +
		"  restart_budget_count: 5\n" +
		"  restart_budget_window_minutes: 30\n" +
		"  probe_interval_seconds: 10\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadDaemonConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.ProjectHealth.RestartBudgetCount != 5 {
		t.Fatalf("restart_budget_count = %d, want 5", cfg.ProjectHealth.RestartBudgetCount)
	}
	if cfg.ProjectHealth.RestartBudgetWindowMinutes != 30 {
		t.Fatalf("restart_budget_window_minutes = %d, want 30", cfg.ProjectHealth.RestartBudgetWindowMinutes)
	}
	if cfg.ProjectHealth.ProbeIntervalSeconds != 10 {
		t.Fatalf("probe_interval_seconds = %d, want 10", cfg.ProjectHealth.ProbeIntervalSeconds)
	}
}

func TestLoadDaemonConfigResolvesWebhookSecretFromEnv(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	t.Setenv("SYMPHONY_LINEAR_WEBHOOK_SECRET", "super-secret")

	configPath := filepath.Join(dir, "config.yaml")
	configYAML := "" +
		"projects:\n" +
		"  - name: alpha\n" +
		"    workflow: " + workflowPath + "\n" +
		"status_server:\n" +
		"  enabled: true\n" +
		"  port: 7777\n" +
		"  webhook_secret: $SYMPHONY_LINEAR_WEBHOOK_SECRET\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadDaemonConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.StatusServer.WebhookSecret != "super-secret" {
		t.Fatalf("WebhookSecret = %q, want super-secret", cfg.StatusServer.WebhookSecret)
	}
}

func TestLoadDaemonConfigUsesWebhookSecretEnvFallback(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	t.Setenv("SYMPHONY_LINEAR_WEBHOOK_SECRET", "fallback-secret")

	configPath := filepath.Join(dir, "config.yaml")
	configYAML := "" +
		"projects:\n" +
		"  - name: alpha\n" +
		"    workflow: " + workflowPath + "\n" +
		"status_server:\n" +
		"  enabled: true\n" +
		"  port: 7777\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadDaemonConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.StatusServer.WebhookSecret != "fallback-secret" {
		t.Fatalf("WebhookSecret = %q, want fallback-secret", cfg.StatusServer.WebhookSecret)
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

func TestDaemonConfigValidateRejectsInvalidProjectHealth(t *testing.T) {
	t.Parallel()

	cfg := &DaemonConfig{
		Projects: []ProjectConfig{{Name: "alpha", Workflow: "/tmp/workflow.md"}},
		ProjectHealth: ProjectHealthConfig{
			RestartBudgetCount:         0,
			RestartBudgetWindowMinutes: 0,
			ProbeIntervalSeconds:       0,
		},
	}

	errs := cfg.Validate()
	if len(errs) < 3 {
		t.Fatalf("Validate() = %v, want project health errors", errs)
	}
}

func TestLoadDaemonConfigPreservesEmptyWorkflowForValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := "projects:\n  - name: alpha\n    workflow: \"\"\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadDaemonConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "workflow path is required")
}

func TestLoadDaemonConfigResolvesRelativePathsFromConfigDirectory(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	workflowDir := filepath.Join(configDir, "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatalf("mkdir workflow dir: %v", err)
	}
	workflowPath := filepath.Join(workflowDir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	configPath := filepath.Join(configDir, "config.yaml")
	configYAML := "projects:\n  - name: alpha\n    workflow: workflows/WORKFLOW.md\nstatus_server:\n  enabled: false\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	outsideDir := t.TempDir()
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatalf("chdir outside: %v", err)
	}
	defer func() {
		_ = os.Chdir(prevWD)
	}()

	cfg, err := LoadDaemonConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if got, want := cfg.Projects[0].Workflow, workflowPath; got != want {
		t.Fatalf("Workflow = %q, want %q", got, want)
	}
}

func TestDaemonConfigValidateRejectsInvalidPathsAndPort(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowDir := filepath.Join(dir, "workflow-dir")
	if err := os.Mkdir(workflowDir, 0o755); err != nil {
		t.Fatalf("mkdir workflow dir: %v", err)
	}

	ln, port := listenOnRandomPort(t)
	defer ln.Close()

	cfg := &DaemonConfig{
		Projects: []ProjectConfig{{
			Name:     "alpha",
			Workflow: workflowDir,
		}},
		AutoUpdate: AutoUpdateConfig{
			Enabled: true,
		},
		StatusServer: StatusServerConfig{
			Enabled: true,
			Port:    port,
		},
	}

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "workflow")
	requireErrorContaining(t, errs, "readable file")
	requireErrorContaining(t, errs, "status_server.port")
	requireErrorContaining(t, errs, "already in use")
}

func TestDaemonConfigValidateRejectsUnreadableWorkflowAndPortRange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	if err := os.Chmod(workflowPath, 0); err != nil {
		t.Fatalf("chmod workflow: %v", err)
	}
	defer func() {
		_ = os.Chmod(workflowPath, 0o600)
	}()

	if f, err := os.Open(workflowPath); err == nil {
		_ = f.Close()
		t.Skip("current environment can still read chmod 000 files")
	}

	cfg := &DaemonConfig{
		Projects: []ProjectConfig{{
			Name:     "alpha",
			Workflow: workflowPath,
		}},
		StatusServer: StatusServerConfig{
			Enabled: true,
			Port:    70000,
		},
	}

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "workflow")
	requireErrorContaining(t, errs, "not readable")
	requireErrorContaining(t, errs, "status_server.port must be between 1 and 65535")
}
