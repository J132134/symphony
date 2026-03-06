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
