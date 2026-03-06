// Package config parses WORKFLOW.md YAML front matter into a typed SymphonyConfig.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SymphonyConfig provides typed access to WORKFLOW.md front matter.
// All string values starting with "$" are resolved from environment variables.
type SymphonyConfig struct {
	raw map[string]any
}

func New(raw map[string]any) *SymphonyConfig {
	if raw == nil {
		raw = map[string]any{}
	}
	return &SymphonyConfig{raw: raw}
}

// NormalizeState trims and lowercases a state name (spec §4.2).
func NormalizeState(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// -- internal helpers --

func (c *SymphonyConfig) resolveEnv(v string) string {
	if strings.HasPrefix(v, "$") {
		return os.Getenv(v[1:])
	}
	return v
}

func (c *SymphonyConfig) expandPath(v string) string {
	resolved := c.resolveEnv(v)
	if strings.HasPrefix(resolved, "~") {
		home, _ := os.UserHomeDir()
		resolved = home + resolved[1:]
	}
	return resolved
}

// get does nested dotted key access (e.g. "tracker.api_key").
func (c *SymphonyConfig) get(key string) any {
	parts := strings.Split(key, ".")
	var node any = c.raw
	for _, p := range parts {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = m[p]
	}
	if s, ok := node.(string); ok {
		return c.resolveEnv(s)
	}
	return node
}

func (c *SymphonyConfig) getString(key, def string) string {
	v := c.get(key)
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func (c *SymphonyConfig) getInt(key string, def int) int {
	v := c.get(key)
	if v == nil {
		return def
	}
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, err := strconv.Atoi(t)
		if err == nil {
			return n
		}
	}
	return def
}

func parseStateList(v any) []string {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case []any:
		result := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					result = append(result, s)
				}
			}
		}
		return result
	case string:
		parts := strings.Split(t, ",")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				result = append(result, p)
			}
		}
		return result
	}
	return nil
}

// -- Tracker --

func (c *SymphonyConfig) TrackerKind() string {
	return c.getString("tracker.kind", "linear")
}
func (c *SymphonyConfig) TrackerAPIKey() string {
	return c.getString("tracker.api_key", "")
}
func (c *SymphonyConfig) TrackerProjectSlug() string {
	return c.getString("tracker.project_slug", "")
}
func (c *SymphonyConfig) TrackerEndpoint() string {
	return c.getString("tracker.endpoint", "https://api.linear.app/graphql")
}
func (c *SymphonyConfig) ActiveStates() []string {
	v := c.get("tracker.active_states")
	if v == nil {
		return []string{"Todo", "In Progress"}
	}
	return parseStateList(v)
}
func (c *SymphonyConfig) TerminalStates() []string {
	v := c.get("tracker.terminal_states")
	if v == nil {
		return []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}
	}
	return parseStateList(v)
}

// -- Polling --

func (c *SymphonyConfig) PollIntervalMs() int {
	return c.getInt("polling.interval_ms", 10_000)
}
func (c *SymphonyConfig) PollIntervalIdleMs() int {
	return c.getInt("polling.idle_interval_ms", 60_000)
}

// -- Workspace --

func (c *SymphonyConfig) WorkspaceRoot() string {
	v := c.getString("workspace.root", "~/.symphony/workspaces")
	return filepath.Clean(c.expandPath(v))
}

// -- Hooks --

func (c *SymphonyConfig) Hooks() map[string]string {
	v := c.get("hooks")
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]string{}
	}
	result := make(map[string]string)
	for k, val := range m {
		if k == "timeout_ms" {
			continue
		}
		if s, ok := val.(string); ok {
			result[k] = c.resolveEnv(s)
		}
	}
	return result
}
func (c *SymphonyConfig) HooksTimeoutMs() int {
	return c.getInt("hooks.timeout_ms", 60_000)
}

// -- Agent --

func (c *SymphonyConfig) MaxConcurrentAgents() int {
	return c.getInt("agent.max_concurrent_agents", 10)
}
func (c *SymphonyConfig) MaxTurns() int {
	return c.getInt("agent.max_turns", 3)
}
func (c *SymphonyConfig) MaxRetryAttempts() int {
	return c.getInt("agent.max_retry_attempts", 0)
}
func (c *SymphonyConfig) MaxRetryBackoffMs() int {
	return c.getInt("agent.max_retry_backoff_ms", 300_000)
}
func (c *SymphonyConfig) MaxConcurrentAgentsByState() map[string]int {
	v := c.get("agent.max_concurrent_agents_by_state")
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]int)
	for k, val := range m {
		key := NormalizeState(k)
		switch t := val.(type) {
		case int:
			if t > 0 {
				result[key] = t
			}
		case float64:
			if int(t) > 0 {
				result[key] = int(t)
			}
		case string:
			n, err := strconv.Atoi(t)
			if err == nil && n > 0 {
				result[key] = n
			}
		}
	}
	return result
}

// -- Codex --

func (c *SymphonyConfig) CodexCommand() string {
	return c.getString("codex.command", "codex app-server")
}
func (c *SymphonyConfig) ApprovalPolicy() string {
	return c.getString("codex.approval_policy", "auto-edit")
}
func (c *SymphonyConfig) TurnTimeoutMs() int {
	return c.getInt("codex.turn_timeout_ms", 3_600_000)
}
func (c *SymphonyConfig) ReadTimeoutMs() int {
	return c.getInt("codex.read_timeout_ms", 5_000)
}
func (c *SymphonyConfig) StallTimeoutMs() int {
	return c.getInt("codex.stall_timeout_ms", 300_000)
}
func (c *SymphonyConfig) TurnSandboxPolicy() string {
	v := c.get("codex.turn_sandbox_policy")
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func (c *SymphonyConfig) ThreadSandbox() string {
	return c.getString("codex.thread_sandbox", "")
}

// -- Server --

func (c *SymphonyConfig) ServerPort() int {
	return c.getInt("server.port", 7777)
}

// -- Validation --

func (c *SymphonyConfig) Validate() []string {
	var errs []string
	if c.TrackerKind() != "linear" {
		errs = append(errs, "tracker.kind must be 'linear'")
	}
	if c.TrackerAPIKey() == "" {
		errs = append(errs, "tracker.api_key is required")
	}
	if c.TrackerProjectSlug() == "" {
		errs = append(errs, "tracker.project_slug is required")
	}
	if c.CodexCommand() == "" {
		errs = append(errs, "codex.command must be non-empty")
	}
	if c.MaxRetryAttempts() < 0 {
		errs = append(errs, "agent.max_retry_attempts must be >= 0")
	}
	return errs
}
