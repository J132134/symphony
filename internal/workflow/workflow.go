// Package workflow parses WORKFLOW.md (YAML front matter + Jinja2/pongo2 template).
package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/flosch/pongo2/v6"
	"gopkg.in/yaml.v3"
)

// Definition holds the parsed WORKFLOW.md.
type Definition struct {
	Config   map[string]any
	Template *pongo2.Template
	RawBody  string
	FilePath string
}

const overlayBodyMarker = "{{ workflow_overlay_body }}"

// Load parses a WORKFLOW.md file.
func Load(path string) (*Definition, error) {
	overlay, err := parseFile(path)
	if err != nil {
		return nil, err
	}
	basePath := resolveReferencedBasePath(filepath.Dir(path), overlay.cfg)
	return loadMergedDefinition(basePath, path, overlay)
}

// LoadMerged parses an optional base workflow plus a project overlay workflow.
func LoadMerged(basePath, overlayPath string) (*Definition, error) {
	overlay, err := parseFile(overlayPath)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(basePath) == "" {
		basePath = resolveReferencedBasePath(filepath.Dir(overlayPath), overlay.cfg)
	}
	return loadMergedDefinition(basePath, overlayPath, overlay)
}

func loadMergedDefinition(basePath, overlayPath string, overlay *parsedFile) (*Definition, error) {
	base, err := parseFile(basePath)
	if err != nil {
		return nil, err
	}
	cfg := mergeMaps(base.cfg, overlay.cfg)
	body := mergeBodies(base.body, overlay.body)

	// pongo2 uses Django-style filter syntax (|filter:"arg").
	// Convert Jinja2-style (|filter(arg)) to pongo2-compatible.
	tmpl, err := pongo2.FromString(convertFilterSyntax(body))
	if err != nil {
		return nil, fmt.Errorf("template parse: %w", err)
	}

	filePath := overlayPath
	if strings.TrimSpace(filePath) == "" {
		filePath = basePath
	}

	return &Definition{
		Config:   cfg,
		Template: tmpl,
		RawBody:  body,
		FilePath: filePath,
	}, nil
}

type parsedFile struct {
	cfg  map[string]any
	body string
}

func parseFile(path string) (*parsedFile, error) {
	if strings.TrimSpace(path) == "" {
		return &parsedFile{cfg: map[string]any{}}, nil
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	text := string(b)
	lines := strings.Split(text, "\n")

	var rawYAML, rawBody string

	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		// Find closing delimiter.
		closing := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				closing = i
				break
			}
		}
		if closing < 0 {
			return nil, fmt.Errorf("no closing '---' in %s", path)
		}
		rawYAML = strings.Join(lines[1:closing], "\n")
		rawBody = strings.Join(lines[closing+1:], "\n")
	} else {
		rawBody = text
	}

	var cfg map[string]any
	if rawYAML != "" {
		if err := yaml.Unmarshal([]byte(rawYAML), &cfg); err != nil {
			return nil, fmt.Errorf("yaml parse: %w", err)
		}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	return &parsedFile{cfg: cfg, body: rawBody}, nil
}

func resolveReferencedBasePath(dir string, cfg map[string]any) string {
	if len(cfg) == 0 {
		return ""
	}
	raw, _ := cfg["workflow_base"].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(raw, "~/"))
		}
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	if strings.TrimSpace(dir) == "" {
		return filepath.Clean(raw)
	}
	return filepath.Clean(filepath.Join(dir, raw))
}

func mergeBodies(baseBody, overlayBody string) string {
	baseBody = strings.TrimSpace(baseBody)
	overlayBody = strings.TrimSpace(overlayBody)

	switch {
	case baseBody == "":
		return overlayBody
	case overlayBody == "":
		return baseBody
	case strings.Contains(baseBody, overlayBodyMarker):
		return strings.ReplaceAll(baseBody, overlayBodyMarker, overlayBody)
	default:
		return baseBody + "\n\n" + overlayBody
	}
}

func mergeMaps(base, overlay map[string]any) map[string]any {
	switch {
	case len(base) == 0 && len(overlay) == 0:
		return map[string]any{}
	case len(base) == 0:
		return cloneMap(overlay)
	case len(overlay) == 0:
		return cloneMap(base)
	}

	merged := cloneMap(base)
	for key, value := range overlay {
		baseValue, ok := merged[key]
		if !ok {
			merged[key] = cloneValue(value)
			continue
		}
		baseMap, baseOK := asStringAnyMap(baseValue)
		overlayMap, overlayOK := asStringAnyMap(value)
		if baseOK && overlayOK {
			merged[key] = mergeMaps(baseMap, overlayMap)
			continue
		}
		merged[key] = cloneValue(value)
	}
	return merged
}

func cloneMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return map[string]any{}
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = cloneValue(value)
	}
	return dst
}

func cloneValue(value any) any {
	if m, ok := asStringAnyMap(value); ok {
		return cloneMap(m)
	}
	if list, ok := value.([]any); ok {
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = cloneValue(item)
		}
		return out
	}
	return value
}

func asStringAnyMap(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	return m, ok
}

// IssueContext is the template context for an issue.
type IssueContext struct {
	ID           string
	Identifier   string
	Title        string
	Description  string
	Priority     *int
	State        string
	Labels       []string
	URL          string
	BranchName   string
	TurnContext  string
	Continuation bool // true for cross-session continuation dispatches
}

// Render renders the prompt template for the given issue and attempt number.
func Render(def *Definition, issue IssueContext, attempt int) (string, error) {
	var workflowConfig map[string]any
	if def != nil {
		workflowConfig = def.Config
	}
	return render(def.Template, pongo2.Context{
		"issue": map[string]any{
			"id":          issue.ID,
			"identifier":  issue.Identifier,
			"title":       issue.Title,
			"description": nonEmpty(issue.Description),
			"priority":    issue.Priority,
			"state":       issue.State,
			"labels":      issue.Labels,
			"url":         nonEmpty(issue.URL),
			"branch_name": nonEmpty(issue.BranchName),
		},
		"workflow":     workflowConfig,
		"attempt":      attempt,
		"continuation": issue.Continuation,
		"turn_context": nonEmpty(issue.TurnContext),
	})
}

// RenderContinuation renders the multi-turn continuation prompt for turn > 1.
// If def.Config contains agent.continuation_prompt, it renders that as a pongo2 template.
// Otherwise it falls back to a default English format string.
func RenderContinuation(def *Definition, issue IssueContext, turnNum, maxTurns int) (string, error) {
	tmplStr := continuationPromptTemplate(def)
	if tmplStr == "" {
		return defaultContinuationPrompt(issue.Identifier, issue.Title, turnNum, maxTurns, issue.TurnContext), nil
	}
	tmpl, err := pongo2.FromString(tmplStr)
	if err != nil {
		return "", fmt.Errorf("continuation_prompt template parse: %w", err)
	}
	var workflowConfig map[string]any
	if def != nil {
		workflowConfig = def.Config
	}
	ctx := pongo2.Context{
		"issue": map[string]any{
			"id":          issue.ID,
			"identifier":  issue.Identifier,
			"title":       issue.Title,
			"description": nonEmpty(issue.Description),
			"state":       issue.State,
			"labels":      issue.Labels,
			"url":         nonEmpty(issue.URL),
		},
		"workflow":     workflowConfig,
		"turn_num":     turnNum,
		"max_turns":    maxTurns,
		"turn_context": nonEmpty(issue.TurnContext),
	}
	return render(tmpl, ctx)
}

func continuationPromptTemplate(def *Definition) string {
	if def == nil {
		return ""
	}
	agent, ok := def.Config["agent"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := agent["continuation_prompt"].(string)
	return strings.TrimSpace(s)
}

func defaultContinuationPrompt(identifier, title string, turnNum, maxTurns int, turnContext string) string {
	if strings.TrimSpace(turnContext) == "" {
		return fmt.Sprintf("Continue working on %s: %s. This is turn %d of %d.",
			identifier, title, turnNum, maxTurns)
	}
	return fmt.Sprintf(
		"Continue working on %s: %s.\n\nProgress so far:\n%s\n\nThis is turn %d of %d. Continue where you left off without repeating completed work.",
		identifier, title, turnContext, turnNum, maxTurns,
	)
}

func render(tmpl *pongo2.Template, ctx pongo2.Context) (string, error) {
	out, err := tmpl.Execute(ctx)
	if err != nil {
		return "", fmt.Errorf("template render: %w", err)
	}
	return out, nil
}

// convertFilterSyntax converts Jinja2-style filter calls to pongo2-style.
// e.g.  "| join(", ")"  →  "|join:", ""
var filterCallRe = regexp.MustCompile(`\|\s*(\w+)\(([^)]*)\)`)

func convertFilterSyntax(s string) string {
	return filterCallRe.ReplaceAllStringFunc(s, func(match string) string {
		m := filterCallRe.FindStringSubmatch(match)
		if len(m) < 3 {
			return match
		}
		return fmt.Sprintf("|%s:%s", m[1], strings.TrimSpace(m[2]))
	})
}

func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
