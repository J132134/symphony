// Package types defines shared domain types used across packages.
// Keeping them here breaks import cycles between orchestrator, tracker, and agent.
package types

import "time"

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
