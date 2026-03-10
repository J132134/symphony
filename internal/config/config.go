// Package config parses WORKFLOW.md YAML front matter into a typed SymphonyConfig.
package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SymphonyConfig provides typed access to WORKFLOW.md front matter.
// All string values starting with "$" are resolved from environment variables.
type SymphonyConfig struct {
	raw        map[string]any
	activeNorm map[string]bool
	termNorm   map[string]bool
	pauseNorm  map[string]bool
}

func New(raw map[string]any) *SymphonyConfig {
	if raw == nil {
		raw = map[string]any{}
	}
	c := &SymphonyConfig{raw: raw}
	c.activeNorm = normalizeStateSet(c.ActiveStates())
	c.termNorm = normalizeStateSet(c.TerminalStates())
	c.pauseNorm = normalizeStateSet(c.PauseStates())
	return c
}

func normalizeStateSet(states []string) map[string]bool {
	m := make(map[string]bool, len(states))
	for _, s := range states {
		m[NormalizeState(s)] = true
	}
	return m
}

// ActiveNorm returns a pre-computed set of normalized active state names.
func (c *SymphonyConfig) ActiveNorm() map[string]bool { return c.activeNorm }

// TermNorm returns a pre-computed set of normalized terminal state names.
func (c *SymphonyConfig) TermNorm() map[string]bool { return c.termNorm }

// PauseNorm returns a pre-computed set of normalized pause state names.
func (c *SymphonyConfig) PauseNorm() map[string]bool { return c.pauseNorm }

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
	if strings.TrimSpace(resolved) == "" {
		return ""
	}
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

func (c *SymphonyConfig) getBool(key string, def bool) bool {
	v := c.get(key)
	if v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := strconv.ParseBool(t)
		if err == nil {
			return b
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
func (c *SymphonyConfig) TrackerPostComments() bool {
	return c.getBool("tracker.post_comments", true)
}
func (c *SymphonyConfig) TrackerOnSuccessState() string {
	return strings.TrimSpace(c.getString("tracker.on_success_state", ""))
}
func (c *SymphonyConfig) TrackerOnFailureState() string {
	return strings.TrimSpace(c.getString("tracker.on_failure_state", ""))
}
func (c *SymphonyConfig) TrackerPRURLTemplate() string {
	return strings.TrimSpace(c.getString("tracker.pr_url_template", ""))
}
func (c *SymphonyConfig) TrackerAssignee() string {
	return c.getString("tracker.assignee", "")
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
func (c *SymphonyConfig) PauseStates() []string {
	v := c.get("tracker.pause_states")
	if v == nil {
		return []string{"Human Review"}
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
	expanded := c.expandPath(v)
	if strings.TrimSpace(expanded) == "" {
		return ""
	}
	return filepath.Clean(expanded)
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

func (c *SymphonyConfig) DrainTimeoutMs() int {
	if explicit := c.getInt("daemon.drain_timeout_ms", 0); explicit > 0 {
		return explicit
	}
	return c.StallTimeoutMs() + c.HooksTimeoutMs()
}

// -- Agent --

func (c *SymphonyConfig) MaxConcurrentAgents() int {
	return c.getInt("agent.max_concurrent_agents", 10)
}
func (c *SymphonyConfig) MaxAttempts() int {
	return c.getInt("agent.max_attempts", 3)
}
func (c *SymphonyConfig) MaxTurns() int {
	return c.getInt("agent.max_turns", 20)
}
func (c *SymphonyConfig) MaxRetryBackoffMs() int {
	return c.getInt("agent.max_retry_backoff_ms", 300_000)
}

func parsePositiveInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, t > 0
	case int64:
		n := int(t)
		return n, n > 0
	case float64:
		if t <= 0 || math.Trunc(t) != t {
			return 0, false
		}
		return int(t), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil || n <= 0 {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func parseStateQuotaMap(v any) (map[string]int, []string) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, []string{"agent.max_concurrent_agents_by_state must be a map of state names to positive integers"}
	}

	result := make(map[string]int, len(m))
	var errs []string
	for rawState, rawLimit := range m {
		state := NormalizeState(rawState)
		if state == "" {
			errs = append(errs, "agent.max_concurrent_agents_by_state contains an empty state name")
			continue
		}

		limit, ok := parsePositiveInt(rawLimit)
		if !ok {
			errs = append(errs, fmt.Sprintf("agent.max_concurrent_agents_by_state.%s must be a positive integer", rawState))
			continue
		}
		result[state] = limit
	}
	if len(result) == 0 {
		return nil, errs
	}
	return result, errs
}

func (c *SymphonyConfig) MaxConcurrentAgentsByState() map[string]int {
	result, _ := parseStateQuotaMap(c.get("agent.max_concurrent_agents_by_state"))
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
func (c *SymphonyConfig) ThreadStartTimeoutMs() int {
	return c.getInt("codex.thread_start_timeout_ms", 60_000)
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
	if strings.TrimSpace(c.WorkspaceRoot()) == "" {
		errs = append(errs, "workspace.root must be non-empty")
	} else {
		errs = append(errs, validateCreatableWritableDir(c.WorkspaceRoot(), "workspace.root")...)
	}
	if c.PollIntervalMs() <= 0 {
		errs = append(errs, "polling.interval_ms must be greater than 0")
	}
	if c.PollIntervalIdleMs() < c.PollIntervalMs() {
		errs = append(errs, "polling.idle_interval_ms must be greater than or equal to polling.interval_ms")
	}
	if c.MaxTurns() <= 0 {
		errs = append(errs, "agent.max_turns must be greater than 0")
	}
	if c.MaxConcurrentAgents() <= 0 {
		errs = append(errs, "agent.max_concurrent_agents must be greater than 0")
	}
	_, quotaErrs := parseStateQuotaMap(c.get("agent.max_concurrent_agents_by_state"))
	errs = append(errs, quotaErrs...)
	if raw := c.get("daemon.drain_timeout_ms"); raw != nil && c.getInt("daemon.drain_timeout_ms", 0) <= 0 {
		errs = append(errs, "daemon.drain_timeout_ms must be greater than 0")
	}
	return errs
}
