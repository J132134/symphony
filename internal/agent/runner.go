package agent

import (
	"context"
	"path/filepath"
	"strings"
)

// Runner is the interface that both CodexRunner and ClaudeRunner implement.
type Runner interface {
	StartSession(ctx context.Context, workspacePath string, cfg *Config) (string, error)
	RunTurn(ctx context.Context, threadID, turnID, prompt, issueIdentifier, issueTitle string, cfg *Config, cb EventCallback) TurnResult
	InterruptTurn(ctx context.Context, threadID, turnID string, cfg *Config) error
	StopSession()
	PID() string
	SessionID() string
	ThreadID() string
}

// NewRunnerForCommand returns a ClaudeRunner when the command base name is
// "claude" or "claude-code", and a CodexRunner otherwise.
func NewRunnerForCommand(command string) Runner {
	parts := strings.Fields(strings.TrimSpace(command))
	if len(parts) > 0 {
		name := filepath.Base(parts[0])
		if name == "claude" || name == "claude-code" {
			return NewClaudeRunner()
		}
	}
	return NewCodexRunner()
}
