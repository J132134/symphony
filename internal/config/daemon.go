package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type ProjectConfig struct {
	Name     string
	Workflow string // resolved absolute path
}

type AutoUpdateConfig struct {
	Enabled         bool
	IntervalMinutes int
	RepoDir         string // git repo path for pull + build
}

type StatusServerConfig struct {
	Enabled bool
	Port    int
}

type DaemonConfig struct {
	Projects     []ProjectConfig
	AutoUpdate   AutoUpdateConfig
	StatusServer StatusServerConfig
	ConfigPath   string
}

func LoadDaemonConfig(path string) (*DaemonConfig, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get home dir: %w", err)
		}
		path = filepath.Join(home, ".config", "symphony", "config.yaml")
	}

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
			wf, _ := pm["workflow"].(string)
			wf = resolvePath(wf)
			cfg.Projects = append(cfg.Projects, ProjectConfig{Name: name, Workflow: wf})
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
		if rd, ok := au["repo_dir"].(string); ok && rd != "" {
			cfg.AutoUpdate.RepoDir = resolvePath(rd)
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

	return cfg, nil
}

func (c *DaemonConfig) Validate() []string {
	var errs []string
	if len(c.Projects) == 0 {
		return []string{"no projects configured"}
	}
	names := map[string]int{}
	for _, p := range c.Projects {
		names[p.Name]++
		if p.Name == "" {
			errs = append(errs, "each project must have a name")
		}
		if p.Workflow == "" {
			errs = append(errs, fmt.Sprintf("project '%s': workflow path is required", p.Name))
		} else if _, err := os.Stat(p.Workflow); err != nil {
			errs = append(errs, fmt.Sprintf("project '%s': workflow not found: %s", p.Name, p.Workflow))
		}
	}
	for name, count := range names {
		if count > 1 {
			errs = append(errs, fmt.Sprintf("duplicate project name: %s", name))
		}
	}
	return errs
}

func resolvePath(v string) string {
	if len(v) > 0 && v[0] == '$' {
		v = os.Getenv(v[1:])
	}
	if len(v) > 0 && v[0] == '~' {
		home, _ := os.UserHomeDir()
		v = home + v[1:]
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return v
	}
	return abs
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
