package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

type ProjectConfig struct {
	Name         string
	WorkflowBase string // resolved absolute path
	Workflow     string // resolved absolute path
}

type AutoUpdateConfig struct {
	Enabled         bool
	IntervalMinutes int
}

type StatusServerConfig struct {
	Enabled bool
	Port    int
}

type WebhookConfig struct {
	Enabled       bool
	Port          int
	BindAddress   string
	SigningSecret string
}

type ProjectHealthConfig struct {
	RestartBudgetCount         int
	RestartBudgetWindowMinutes int
	ProbeIntervalSeconds       int
}

type DaemonAgentConfig struct {
	MaxTotalConcurrentSessions int
}

type DaemonConfig struct {
	Projects      []ProjectConfig
	AutoUpdate    AutoUpdateConfig
	Agent         DaemonAgentConfig
	StatusServer  StatusServerConfig
	Webhook       WebhookConfig
	ProjectHealth ProjectHealthConfig
	ConfigPath    string

	maxTotalConcurrentSessionsConfigured bool
	validatePort                         func(int, string) []string
}

func LoadDaemonConfig(path string) (*DaemonConfig, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get home dir: %w", err)
		}
		path = filepath.Join(home, ".config", "symphony", "config.yaml")
	}
	configDir := filepath.Dir(path)

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var raw map[string]any
	if err := yaml.NewDecoder(f).Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if raw == nil {
		raw = map[string]any{}
	}

	cfg := &DaemonConfig{ConfigPath: path}

	// projects
	if projs, ok := raw["projects"].([]any); ok {
		for _, p := range projs {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			name, _ := pm["name"].(string)
			wfBase, _ := pm["workflow_base"].(string)
			wf, _ := pm["workflow"].(string)
			wfBase = resolvePath(configDir, wfBase)
			wf = resolvePath(configDir, wf)
			cfg.Projects = append(cfg.Projects, ProjectConfig{
				Name:         name,
				WorkflowBase: wfBase,
				Workflow:     wf,
			})
		}
	}

	// auto_update
	cfg.AutoUpdate = AutoUpdateConfig{Enabled: true, IntervalMinutes: 30}
	if au, ok := raw["auto_update"].(map[string]any); ok {
		if enabled, ok := au["enabled"].(bool); ok {
			cfg.AutoUpdate.Enabled = enabled
		}
		if mins, ok := au["interval_minutes"]; ok {
			cfg.AutoUpdate.IntervalMinutes = toInt(mins, 30)
		}
	}

	// agent
	cfg.Agent = DaemonAgentConfig{MaxTotalConcurrentSessions: DefaultMaxTotalConcurrentSessions()}
	if ag, ok := raw["agent"].(map[string]any); ok {
		if limit, ok := ag["max_total_concurrent_sessions"]; ok {
			cfg.Agent.MaxTotalConcurrentSessions = toInt(limit, cfg.Agent.MaxTotalConcurrentSessions)
			cfg.maxTotalConcurrentSessionsConfigured = true
		}
	}

	// status_server
	cfg.StatusServer = StatusServerConfig{Enabled: true, Port: 7777}
	if ss, ok := raw["status_server"].(map[string]any); ok {
		if enabled, ok := ss["enabled"].(bool); ok {
			cfg.StatusServer.Enabled = enabled
		}
		if port, ok := ss["port"]; ok {
			cfg.StatusServer.Port = toInt(port, 7777)
		}
	}

	// webhook
	cfg.Webhook = WebhookConfig{
		Enabled:       false,
		Port:          7777,
		BindAddress:   "127.0.0.1",
		SigningSecret: "",
	}
	if wh, ok := raw["webhook"].(map[string]any); ok {
		if enabled, ok := wh["enabled"].(bool); ok {
			cfg.Webhook.Enabled = enabled
		}
		if port, ok := wh["port"]; ok {
			cfg.Webhook.Port = toInt(port, cfg.Webhook.Port)
		}
		if bind, ok := wh["bind_address"].(string); ok {
			cfg.Webhook.BindAddress = bind
		}
		if secret, ok := wh["signing_secret"].(string); ok {
			cfg.Webhook.SigningSecret = resolveEnvString(secret)
		}
	}

	cfg.ProjectHealth = ProjectHealthConfig{
		RestartBudgetCount:         3,
		RestartBudgetWindowMinutes: 15,
		ProbeIntervalSeconds:       60,
	}
	if ph, ok := raw["project_health"].(map[string]any); ok {
		if count, ok := ph["restart_budget_count"]; ok {
			cfg.ProjectHealth.RestartBudgetCount = toInt(count, cfg.ProjectHealth.RestartBudgetCount)
		}
		if mins, ok := ph["restart_budget_window_minutes"]; ok {
			cfg.ProjectHealth.RestartBudgetWindowMinutes = toInt(mins, cfg.ProjectHealth.RestartBudgetWindowMinutes)
		}
		if secs, ok := ph["probe_interval_seconds"]; ok {
			cfg.ProjectHealth.ProbeIntervalSeconds = toInt(secs, cfg.ProjectHealth.ProbeIntervalSeconds)
		}
	}

	return cfg, nil
}

func (c *DaemonConfig) Validate() []string {
	var errs []string
	portValidator := c.portValidator()
	if len(c.Projects) == 0 {
		return []string{"no projects configured"}
	}
	names := map[string]int{}
	for _, p := range c.Projects {
		names[p.Name]++
		if strings.TrimSpace(p.Name) == "" {
			errs = append(errs, "each project must have a name")
		}
		if strings.TrimSpace(p.Workflow) == "" {
			errs = append(errs, fmt.Sprintf("project '%s': workflow path is required", p.Name))
		} else {
			errs = append(errs, validateReadableFile(p.Workflow, fmt.Sprintf("project '%s': workflow", p.Name))...)
		}
		if strings.TrimSpace(p.WorkflowBase) != "" {
			errs = append(errs, validateReadableFile(p.WorkflowBase, fmt.Sprintf("project '%s': workflow_base", p.Name))...)
		}
	}
	for name, count := range names {
		if count > 1 {
			errs = append(errs, fmt.Sprintf("duplicate project name: %s", name))
		}
	}
	if c.StatusServer.Enabled {
		errs = append(errs, portValidator(c.StatusServer.Port, "status_server.port")...)
	}
	if c.Webhook.Enabled {
		if c.Webhook.Port == 0 {
			errs = append(errs, "webhook.port is required when webhook.enabled is true")
		}
		errs = append(errs, portValidator(c.Webhook.Port, "webhook.port")...)
	}
	if c.maxTotalConcurrentSessionsConfigured && c.Agent.MaxTotalConcurrentSessions <= 0 {
		errs = append(errs, "agent.max_total_concurrent_sessions must be greater than 0")
	}
	if c.ProjectHealth.RestartBudgetCount <= 0 {
		errs = append(errs, "project_health.restart_budget_count must be greater than 0")
	}
	if c.ProjectHealth.RestartBudgetWindowMinutes <= 0 {
		errs = append(errs, "project_health.restart_budget_window_minutes must be greater than 0")
	}
	if c.ProjectHealth.ProbeIntervalSeconds <= 0 {
		errs = append(errs, "project_health.probe_interval_seconds must be greater than 0")
	}
	return errs
}

func (c *DaemonConfig) SetPortValidatorForTesting(fn func(int, string) []string) {
	c.validatePort = fn
}

func (c *DaemonConfig) portValidator() func(int, string) []string {
	if c != nil && c.validatePort != nil {
		return c.validatePort
	}
	return validateTCPPortAvailable
}

func (c *DaemonConfig) MaxTotalConcurrentSessions() int {
	if c == nil || c.Agent.MaxTotalConcurrentSessions <= 0 {
		return DefaultMaxTotalConcurrentSessions()
	}
	return c.Agent.MaxTotalConcurrentSessions
}

func (c *DaemonConfig) ProjectByWorkflowPath(path string) (ProjectConfig, bool) {
	if c == nil || strings.TrimSpace(path) == "" {
		return ProjectConfig{}, false
	}
	target := filepath.Clean(path)
	for _, project := range c.Projects {
		if filepath.Clean(project.Workflow) == target {
			return project, true
		}
	}
	return ProjectConfig{}, false
}

func DefaultMaxTotalConcurrentSessions() int {
	cpus := runtime.NumCPU()
	switch {
	case cpus <= 2:
		return 1
	case cpus <= 4:
		return 2
	default:
		limit := cpus / 2
		if limit > 8 {
			return 8
		}
		return limit
	}
}

func resolvePath(baseDir, v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	v = resolveEnvString(v)
	if len(v) > 0 && v[0] == '~' {
		home, _ := os.UserHomeDir()
		v = home + v[1:]
	}
	if !filepath.IsAbs(v) && strings.TrimSpace(baseDir) != "" {
		v = filepath.Join(baseDir, v)
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return v
	}
	return abs
}

func resolveEnvString(v string) string {
	if strings.HasPrefix(v, "$") {
		return os.Getenv(v[1:])
	}
	return v
}

func toInt(v any, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case float64:
		return int(t)
	}
	return def
}
