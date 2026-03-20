package agent

import "testing"

func TestNewRunnerForCommandDefaultsToCodex(t *testing.T) {
	t.Parallel()

	r := NewRunnerForCommand("codex app-server")
	if _, ok := r.(*CodexRunner); !ok {
		t.Fatalf("NewRunnerForCommand(\"codex app-server\") returned %T, want *CodexRunner", r)
	}
}

func TestNewRunnerForCommandSelectsClaudeForClaude(t *testing.T) {
	t.Parallel()

	r := NewRunnerForCommand("claude")
	if _, ok := r.(*ClaudeRunner); !ok {
		t.Fatalf("NewRunnerForCommand(\"claude\") returned %T, want *ClaudeRunner", r)
	}
}

func TestNewRunnerForCommandSelectsClaudeForClaudeCode(t *testing.T) {
	t.Parallel()

	r := NewRunnerForCommand("claude-code")
	if _, ok := r.(*ClaudeRunner); !ok {
		t.Fatalf("NewRunnerForCommand(\"claude-code\") returned %T, want *ClaudeRunner", r)
	}
}

func TestNewRunnerForCommandSelectsClaudeForFullPath(t *testing.T) {
	t.Parallel()

	r := NewRunnerForCommand("/usr/local/bin/claude")
	if _, ok := r.(*ClaudeRunner); !ok {
		t.Fatalf("NewRunnerForCommand(\"/usr/local/bin/claude\") returned %T, want *ClaudeRunner", r)
	}
}

func TestNewRunnerForCommandEmptyDefaultsToCodex(t *testing.T) {
	t.Parallel()

	r := NewRunnerForCommand("")
	if _, ok := r.(*CodexRunner); !ok {
		t.Fatalf("NewRunnerForCommand(\"\") returned %T, want *CodexRunner", r)
	}
}
