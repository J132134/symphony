package config

import (
	"net"
	"os"
	"path/filepath"
	"strings"
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

func TestLoadDaemonConfigUsesWebhookDefaults(t *testing.T) {
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

	if cfg.Webhook.Enabled {
		t.Fatal("Webhook.Enabled should default to false")
	}
	if got := cfg.Webhook.Port; got != 7777 {
		t.Fatalf("Webhook.Port = %d, want 7777", got)
	}
	if got := cfg.Webhook.BindAddress; got != "127.0.0.1" {
		t.Fatalf("Webhook.BindAddress = %q, want 127.0.0.1", got)
	}
	if got := cfg.Webhook.SigningSecret; got != "" {
		t.Fatalf("Webhook.SigningSecret = %q, want empty", got)
	}
}

func TestLoadDaemonConfigOverridesWebhook(t *testing.T) {
	t.Setenv("LINEAR_WEBHOOK_SECRET", "top-secret")

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
		"webhook:\n" +
		"  enabled: true\n" +
		"  port: 8787\n" +
		"  bind_address: 0.0.0.0\n" +
		"  signing_secret: $LINEAR_WEBHOOK_SECRET\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadDaemonConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if !cfg.Webhook.Enabled {
		t.Fatal("Webhook.Enabled = false, want true")
	}
	if got := cfg.Webhook.Port; got != 8787 {
		t.Fatalf("Webhook.Port = %d, want 8787", got)
	}
	if got := cfg.Webhook.BindAddress; got != "0.0.0.0" {
		t.Fatalf("Webhook.BindAddress = %q, want 0.0.0.0", got)
	}
	if got := cfg.Webhook.SigningSecret; got != "top-secret" {
		t.Fatalf("Webhook.SigningSecret = %q, want top-secret", got)
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

func TestDaemonConfigValidateAllowsSharedWebhookAndStatusPort(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	cfg := &DaemonConfig{
		Projects: []ProjectConfig{{Name: "alpha", Workflow: workflowPath}},
		StatusServer: StatusServerConfig{
			Enabled: true,
			Port:    65530,
		},
		Webhook: WebhookConfig{
			Enabled: true,
			Port:    65530,
		},
		ProjectHealth: ProjectHealthConfig{
			RestartBudgetCount:         3,
			RestartBudgetWindowMinutes: 15,
			ProbeIntervalSeconds:       60,
		},
	}

	errs := cfg.Validate()
	for _, err := range errs {
		if strings.Contains(err, "must not match status_server.port") {
			t.Fatalf("Validate() = %v, shared-port error should be removed", errs)
		}
	}
}

func TestDaemonConfigValidateRejectsMissingWebhookPort(t *testing.T) {
	t.Parallel()

	cfg := &DaemonConfig{
		Projects: []ProjectConfig{{Name: "alpha", Workflow: "/tmp/workflow.md"}},
		Webhook: WebhookConfig{
			Enabled: true,
			Port:    0,
		},
	}

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "webhook.port is required")
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
	basePath := filepath.Join(configDir, "base.md")
	if err := os.WriteFile(basePath, []byte("# base\n"), 0o644); err != nil {
		t.Fatalf("write workflow base: %v", err)
	}

	configPath := filepath.Join(configDir, "config.yaml")
	configYAML := "" +
		"projects:\n" +
		"  - name: alpha\n" +
		"    workflow_base: base.md\n" +
		"    workflow: workflows/WORKFLOW.md\n" +
		"status_server:\n" +
		"  enabled: false\n"
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
	if got, want := cfg.Projects[0].WorkflowBase, basePath; got != want {
		t.Fatalf("WorkflowBase = %q, want %q", got, want)
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

func TestDaemonConfigValidateRejectsUnreadableWorkflowBase(t *testing.T) {
	t.Parallel()

	cfg := &DaemonConfig{
		Projects: []ProjectConfig{{
			Name:         "alpha",
			WorkflowBase: "/tmp/missing-base.md",
			Workflow:     "/tmp/workflow.md",
		}},
	}

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "workflow_base")
}

func TestDaemonConfigProjectByWorkflowPath(t *testing.T) {
	t.Parallel()

	cfg := &DaemonConfig{
		Projects: []ProjectConfig{
			{Name: "alpha", WorkflowBase: "/tmp/base.md", Workflow: "/tmp/a/WORKFLOW.md"},
			{Name: "beta", Workflow: "/tmp/b/WORKFLOW.md"},
		},
	}

	project, ok := cfg.ProjectByWorkflowPath("/tmp/a/WORKFLOW.md")
	if !ok {
		t.Fatal("ProjectByWorkflowPath() = false, want true")
	}
	if project.Name != "alpha" {
		t.Fatalf("project.Name = %q, want alpha", project.Name)
	}
	if project.WorkflowBase != "/tmp/base.md" {
		t.Fatalf("project.WorkflowBase = %q, want /tmp/base.md", project.WorkflowBase)
	}
	if _, ok := cfg.ProjectByWorkflowPath("/tmp/missing/WORKFLOW.md"); ok {
		t.Fatal("ProjectByWorkflowPath() = true for missing workflow, want false")
	}
}
