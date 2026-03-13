package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderExposesTurnContextTemplateVariable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := "{% if turn_context %}## 이전 작업 요약\n{{ turn_context }}{% endif %}"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	def, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	out, err := Render(def, IssueContext{TurnContext: "Git diff summary:\nfoo.txt"}, 2)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "## 이전 작업 요약") || !strings.Contains(out, "foo.txt") {
		t.Fatalf("Render output missing turn context:\n%s", out)
	}
}

func TestRenderContinuationFallbackWithTurnContext(t *testing.T) {
	t.Parallel()

	def := &Definition{Config: map[string]any{}}
	issue := IssueContext{Identifier: "J-24", Title: "Multi-turn prompt", TurnContext: "Git diff summary:\nfoo.txt"}
	prompt, err := RenderContinuation(def, issue, 2, 20)
	if err != nil {
		t.Fatalf("RenderContinuation: %v", err)
	}
	for _, want := range []string{
		"Continue working on J-24: Multi-turn prompt.",
		"Progress so far:",
		"Git diff summary:\nfoo.txt",
		"This is turn 2 of 20.",
		"Continue where you left off without repeating completed work.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRenderContinuationFallbackWithoutTurnContext(t *testing.T) {
	t.Parallel()

	def := &Definition{Config: map[string]any{}}
	issue := IssueContext{Identifier: "J-24", Title: "Multi-turn prompt"}
	prompt, err := RenderContinuation(def, issue, 2, 20)
	if err != nil {
		t.Fatalf("RenderContinuation: %v", err)
	}
	want := "Continue working on J-24: Multi-turn prompt. This is turn 2 of 20."
	if prompt != want {
		t.Fatalf("prompt = %q, want %q", prompt, want)
	}
}

func TestRenderContinuationUsesConfiguredTemplate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	content := `---
agent:
  continuation_prompt: "{{ issue.identifier }} 계속: 턴 {{ turn_num }}/{{ max_turns }}{% if turn_context %} | {{ turn_context }}{% endif %}"
---
body`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	def, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	issue := IssueContext{Identifier: "J-99", TurnContext: "진행 요약"}
	prompt, err := RenderContinuation(def, issue, 3, 10)
	if err != nil {
		t.Fatalf("RenderContinuation: %v", err)
	}
	want := "J-99 계속: 턴 3/10 | 진행 요약"
	if prompt != want {
		t.Fatalf("prompt = %q, want %q", prompt, want)
	}
}

func TestRenderOmitsTurnContextBlockWhenEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := "{% if turn_context %}visible{% endif %}"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	def, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	out, err := Render(def, IssueContext{}, 1)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("Render output = %q, want empty", out)
	}
}

func TestLoadMergedCombinesConfigAndInjectsOverlayBody(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.md")
	overlayPath := filepath.Join(dir, "overlay.md")

	base := `---
tracker:
  api_key: token
  active_states:
    - Todo
agent:
  max_turns: 20
  continuation_prompt: "base"
---
공통 시작

{{ workflow_overlay_body }}

공통 종료`
	overlay := `---
tracker:
  project_slug: abc123
agent:
  max_concurrent_agents: 3
---
프로젝트 지침`

	if err := os.WriteFile(basePath, []byte(base), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	def, err := LoadMerged(basePath, overlayPath)
	if err != nil {
		t.Fatalf("LoadMerged: %v", err)
	}

	tracker, _ := def.Config["tracker"].(map[string]any)
	if got, _ := tracker["api_key"].(string); got != "token" {
		t.Fatalf("tracker.api_key = %q, want token", got)
	}
	if got, _ := tracker["project_slug"].(string); got != "abc123" {
		t.Fatalf("tracker.project_slug = %q, want abc123", got)
	}

	agent, _ := def.Config["agent"].(map[string]any)
	if got, _ := agent["max_turns"].(int); got != 20 {
		t.Fatalf("agent.max_turns = %v, want 20", agent["max_turns"])
	}
	if got, _ := agent["max_concurrent_agents"].(int); got != 3 {
		t.Fatalf("agent.max_concurrent_agents = %v, want 3", agent["max_concurrent_agents"])
	}

	for _, want := range []string{"공통 시작", "프로젝트 지침", "공통 종료"} {
		if !strings.Contains(def.RawBody, want) {
			t.Fatalf("merged body missing %q:\n%s", want, def.RawBody)
		}
	}
}

func TestLoadUsesWorkflowBaseReferenceFromOverlay(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.md")
	overlayPath := filepath.Join(dir, "WORKFLOW.md")

	base := `---
tracker:
  kind: linear
project:
  name: shared
---
{{ workflow.project.name }}`
	overlay := `---
workflow_base: base.md
tracker:
  project_slug: xyz
project:
  name: overlay
---
`

	if err := os.WriteFile(basePath, []byte(base), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	def, err := Load(overlayPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	out, err := Render(def, IssueContext{}, 1)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.TrimSpace(out) != "overlay" {
		t.Fatalf("Render output = %q, want overlay", out)
	}
}
