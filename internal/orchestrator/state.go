package orchestrator

import (
	"context"
	"sync"
	"time"
)

type RunStatus string

const (
	StatusPreparingWorkspace  RunStatus = "preparing_workspace"
	StatusBuildingPrompt      RunStatus = "building_prompt"
	StatusLaunchingAgent      RunStatus = "launching_agent_process"
	StatusInitializingSession RunStatus = "initializing_session"
	StatusStreamingTurn       RunStatus = "streaming_turn"
	StatusFinishing           RunStatus = "finishing"
	StatusSucceeded           RunStatus = "succeeded"
	StatusFailed              RunStatus = "failed"
	StatusTimedOut            RunStatus = "timed_out"
	StatusStalled             RunStatus = "stalled"
	StatusCanceled            RunStatus = "canceled_by_reconciliation"
)

type TokenUsage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

type LiveSession struct {
	SessionID    string
	ThreadID     string
	AgentPID     string
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	TurnCount    int
	LastEventAt  *time.Time
}

type RunAttempt struct {
	IssueID       string
	Identifier    string
	Attempt       int
	WorkspacePath string
	StartedAt     time.Time
	Status        RunStatus
	Error         string
	Session       LiveSession
	IssueState    string // last known tracker state for per-state concurrency

	cancel context.CancelFunc
}

type RetryEntry struct {
	IssueID    string
	Identifier string
	Attempt    int
	DueAt      time.Time
	Error      string
	timer      *time.Timer
}

// State holds all orchestrator runtime state.
type State struct {
	mu sync.Mutex

	PollIntervalMs      int
	PollIntervalIdleMs  int
	MaxConcurrentAgents int

	Running    map[string]*RunAttempt
	Claimed    map[string]struct{}
	RetryQueue map[string]*RetryEntry

	CompletedCount int
	Totals         TokenUsage
}

func NewState() *State {
	return &State{
		PollIntervalMs:      10_000,
		PollIntervalIdleMs:  60_000,
		MaxConcurrentAgents: 10,
		Running:             make(map[string]*RunAttempt),
		Claimed:             make(map[string]struct{}),
		RetryQueue:          make(map[string]*RetryEntry),
	}
}

// Lock/Unlock expose the internal mutex for external packages (e.g. status server).
func (s *State) Lock()   { s.mu.Lock() }
func (s *State) Unlock() { s.mu.Unlock() }
