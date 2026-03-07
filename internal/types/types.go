// Package types defines shared domain types used across packages.
// Keeping them here breaks import cycles between orchestrator, tracker, and agent.
package types

import "time"

// TokenUsage tracks input/output/total tokens for a turn delta.
type TokenUsage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

type BlockerRef struct {
	ID         string
	Identifier string
	State      string
}

type Comment struct {
	Body      string
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

type Issue struct {
	ID          string
	Identifier  string
	Title       string
	Description string
	Priority    *int
	State       string
	BranchName  string
	URL         string
	Labels      []string
	BlockedBy   []BlockerRef
	LastComment *Comment
	CreatedAt   *time.Time
	UpdatedAt   *time.Time
}
