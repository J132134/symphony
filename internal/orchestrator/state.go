package orchestrator

import (
	"context"
	"strings"
	"sync"
	"time"

	"symphony/internal/types"
)

const retryAbandonedCommentMarker = "<!-- symphony:retry-abandoned -->"

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
	IssueID        string
	Identifier     string
	Attempt        int
	FailureCount   int
	WorkspacePath  string
	StartedAt      time.Time
	Status         RunStatus
	Error          string
	Session        LiveSession
	IssueState     string // last known tracker state for per-state concurrency
	GlobalSlotHeld bool

	cancel context.CancelFunc
}

type RetryEntry struct {
	IssueID      string
	Identifier   string
	Attempt      int
	FailureCount int
	DueAt        time.Time
	Error        string
	timer        *time.Timer
}

type AbandonedEntry struct {
	Identifier   string
	State        string
	FailureCount int
	Error        string
	AbandonedAt  time.Time
}

func (e *AbandonedEntry) ResumeAfter(issue *types.Issue) time.Time {
	if e == nil {
		return time.Time{}
	}
	resumeAfter := e.AbandonedAt
	if issue == nil {
		return resumeAfter
	}
	comment := issue.LastComment
	if comment == nil || !isRetryAbandonComment(comment.Body) {
		return resumeAfter
	}
	if comment.UpdatedAt != nil && comment.UpdatedAt.After(resumeAfter) {
		return *comment.UpdatedAt
	}
	if comment.CreatedAt != nil && comment.CreatedAt.After(resumeAfter) {
		return *comment.CreatedAt
	}
	return resumeAfter
}

func isRetryAbandonComment(body string) bool {
	return strings.Contains(body, retryAbandonedCommentMarker)
}

// State holds all orchestrator runtime state.
type State struct {
	mu sync.Mutex

	PollIntervalMs      int
	PollIntervalIdleMs  int
	MaxConcurrentAgents int
	Draining            bool

	Running    map[string]*RunAttempt
	Claimed    map[string]struct{}
	RetryQueue map[string]*RetryEntry
	Abandoned  map[string]*AbandonedEntry

	CompletedCount int
	Totals         TokenUsage

	LastTrackerSuccessAt *time.Time
	LastTrackerErrorAt   *time.Time
	LastTrackerError     string
}

func NewState() *State {
	return &State{
		PollIntervalMs:      10_000,
		PollIntervalIdleMs:  60_000,
		MaxConcurrentAgents: 10,
		Running:             make(map[string]*RunAttempt),
		Claimed:             make(map[string]struct{}),
		RetryQueue:          make(map[string]*RetryEntry),
		Abandoned:           make(map[string]*AbandonedEntry),
	}
}

// Lock/Unlock expose the internal mutex for external packages (e.g. status server).
func (s *State) Lock()   { s.mu.Lock() }
func (s *State) Unlock() { s.mu.Unlock() }

func (s *State) RecordTrackerSuccess(at time.Time) {
	at = at.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.LastTrackerSuccessAt = &at
	s.LastTrackerErrorAt = nil
	s.LastTrackerError = ""
}

func (s *State) RecordTrackerFailure(at time.Time, err error) {
	at = at.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.LastTrackerErrorAt = &at
	if err != nil {
		s.LastTrackerError = err.Error()
	} else {
		s.LastTrackerError = "unknown tracker error"
	}
}

func (s *State) TrackerStatus() (connected bool, lastSuccess string, lastError string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.trackerStatusLocked()
}

// TrackerStatusLocked reports tracker connectivity while the caller already holds s.mu.
func (s *State) TrackerStatusLocked() (connected bool, lastSuccess string, lastError string) {
	return s.trackerStatusLocked()
}

func (s *State) trackerStatusLocked() (connected bool, lastSuccess string, lastError string) {
	connected = true
	if s.LastTrackerErrorAt != nil && (s.LastTrackerSuccessAt == nil || !s.LastTrackerSuccessAt.After(*s.LastTrackerErrorAt)) {
		connected = false
		lastError = s.LastTrackerError
		if lastError == "" {
			lastError = "unknown tracker error"
		}
	}
	if s.LastTrackerSuccessAt != nil {
		lastSuccess = s.LastTrackerSuccessAt.Format(time.RFC3339)
	}
	return connected, lastSuccess, lastError
}
