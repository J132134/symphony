// Package workflow parses WORKFLOW.md (YAML front matter + Jinja2/pongo2 template).
package workflow

import (
	"fmt"
	"os"
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

// Load parses a WORKFLOW.md file.
func Load(path string) (*Definition, error) {
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

	// pongo2 uses Django-style filter syntax (|filter:"arg").
	// Convert Jinja2-style (|filter(arg)) to pongo2-compatible.
	body := convertFilterSyntax(rawBody)

	tmpl, err := pongo2.FromString(body)
	if err != nil {
		return nil, fmt.Errorf("template parse: %w", err)
	}

	return &Definition{
		Config:   cfg,
		Template: tmpl,
		RawBody:  rawBody,
		FilePath: path,
	}, nil
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
