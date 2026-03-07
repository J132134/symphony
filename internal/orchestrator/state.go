package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"symphony/internal/types"
)

type RunStatus string
type RetryKind string

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

const (
	RetryKindFailure  RetryKind = "failure"
	RetryKindCapacity RetryKind = "capacity"
)

const retryAbandonCommentMarker = "<!-- symphony:retry-abandoned -->"

// TokenUsage is an alias for types.TokenUsage.
type TokenUsage = types.TokenUsage

type LiveSession struct {
	SessionID    string
	ThreadID     string
	TurnID       string
	AgentPID     string
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	TurnCount    int
	LastEventAt  *time.Time
}

type WorkerCancelReason string

const (
	CancelReasonNone      WorkerCancelReason = ""
	CancelReasonDrain     WorkerCancelReason = "drain"
	CancelReasonShutdown  WorkerCancelReason = "shutdown"
	CancelReasonStall     WorkerCancelReason = "stall"
	CancelReasonTerminal  WorkerCancelReason = "terminal"
	CancelReasonReconcile WorkerCancelReason = "reconcile"
)

type RunAttempt struct {
	IssueID        string
	Identifier     string
	Attempt        int
	FailureCount   int
	WorkspacePath  string
	StartedAt      time.Time
	Error          string
	Session        LiveSession
	IssueState     string // last known tracker state for per-state concurrency
	GlobalSlotHeld bool
	Urgent         bool
	Preempted      bool

	mu            sync.Mutex
	status        RunStatus
	CancelReason  WorkerCancelReason
	CleanupOnExit bool
	DrainDeadline *time.Time

	cancel context.CancelFunc
}

func (a *RunAttempt) SetStatus(s RunStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status = s
}

func (a *RunAttempt) GetStatus() RunStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

func (a *RunAttempt) UpdateLastEvent(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Session.LastEventAt = &t
}

func (a *RunAttempt) GetLastEventAt() *time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Session.LastEventAt
}

func (a *RunAttempt) AddTokens(in, out, total int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Session.InputTokens += in
	a.Session.OutputTokens += out
	a.Session.TotalTokens += total
}

func (a *RunAttempt) SetSessionIdentity(threadID, sessionID, agentPID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Session.ThreadID = threadID
	a.Session.SessionID = sessionID
	a.Session.AgentPID = agentPID
}

func (a *RunAttempt) UpdateSessionRuntime(sessionID, agentPID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sessionID != "" {
		a.Session.SessionID = sessionID
	}
	if agentPID != "" {
		a.Session.AgentPID = agentPID
	}
}

func (a *RunAttempt) SetTurnCount(turnCount int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Session.TurnCount = turnCount
}

func (a *RunAttempt) SessionSnapshot() LiveSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Session
}

func (a *RunAttempt) SetCancelReason(reason WorkerCancelReason) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.CancelReason = reason
}

func (a *RunAttempt) GetCancelReason() WorkerCancelReason {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CancelReason
}

func (a *RunAttempt) MarkCleanupOnExit() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.CleanupOnExit = true
}

func (a *RunAttempt) ShouldCleanupOnExit() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CleanupOnExit
}

func (a *RunAttempt) SetDrainDeadline(deadline time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.DrainDeadline = &deadline
}

func (a *RunAttempt) ClearDrainDeadline() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.DrainDeadline = nil
}

func (a *RunAttempt) DrainContext() (context.Context, context.CancelFunc) {
	a.mu.Lock()
	deadline := a.DrainDeadline
	a.mu.Unlock()
	if deadline == nil {
		return context.WithCancel(context.Background())
	}
	return context.WithDeadline(context.Background(), *deadline)
}

func (a *RunAttempt) SetActiveTurn(threadID, turnID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Session.ThreadID = threadID
	a.Session.TurnID = turnID
}

func (a *RunAttempt) ClearActiveTurn(turnID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Session.TurnID == turnID {
		a.Session.TurnID = ""
	}
}

func (a *RunAttempt) ActiveTurn() (threadID, turnID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Session.ThreadID, a.Session.TurnID
}

var errWorkerCancelled = errors.New("worker cancelled")

func isWorkerCancelled(err error) bool {
	if err == nil {
		return false
	}
	return isCtxErr(err) || errors.Is(err, errWorkerCancelled)
}

func workerCancelledError(reason WorkerCancelReason, detail string) error {
	if detail == "" {
		detail = string(reason)
		if detail == "" {
			detail = "cancelled"
		}
	}
	return fmt.Errorf("%w: %s", errWorkerCancelled, detail)
}

type RetryEntry struct {
	IssueID      string
	Identifier   string
	Kind         RetryKind
	Attempt      int
	FailureCount int
	DeferCount   int
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

func isRetryAbandonComment(body string) bool {
	return strings.Contains(body, retryAbandonCommentMarker)
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

	PausedUntil         *time.Time
	PauseReason         string
	RateLimitPauseCount int
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
