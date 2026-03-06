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

func TestLoadDaemonConfigPreservesEmptyRepoDirForValidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# workflow\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	configYAML := "projects:\n  - name: alpha\n    workflow: " + workflowPath + "\nauto_update:\n  enabled: true\n  repo_dir: \"\"\nstatus_server:\n  enabled: false\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadDaemonConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "auto_update.repo_dir")
	requireErrorContaining(t, errs, "non-empty")
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

	repoDir := filepath.Join(configDir, "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir repo dir: %v", err)
	}

	configPath := filepath.Join(configDir, "config.yaml")
	configYAML := "projects:\n  - name: alpha\n    workflow: workflows/WORKFLOW.md\nauto_update:\n  enabled: true\n  repo_dir: repo\nstatus_server:\n  enabled: false\n"
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
	if got, want := cfg.AutoUpdate.RepoDir, repoDir; got != want {
		t.Fatalf("RepoDir = %q, want %q", got, want)
	}
}

func TestDaemonConfigValidateRejectsInvalidPathsAndPort(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workflowDir := filepath.Join(dir, "workflow-dir")
	if err := os.Mkdir(workflowDir, 0o755); err != nil {
		t.Fatalf("mkdir workflow dir: %v", err)
	}

	repoDir := filepath.Join(dir, "not-a-repo")
	if err := os.Mkdir(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo dir: %v", err)
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
			RepoDir: repoDir,
		},
		StatusServer: StatusServerConfig{
			Enabled: true,
			Port:    port,
		},
	}

	errs := cfg.Validate()
	requireErrorContaining(t, errs, "workflow")
	requireErrorContaining(t, errs, "readable file")
	requireErrorContaining(t, errs, "auto_update.repo_dir")
	requireErrorContaining(t, errs, "git repository")
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
