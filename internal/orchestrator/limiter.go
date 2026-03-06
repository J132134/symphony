package orchestrator

import (
	"sync"
	"time"
)

// SessionLimiter caps daemon-wide concurrent agent sessions across projects.
type SessionLimiter struct {
	mu      sync.Mutex
	limit   int
	inUse   int
	urgent  int
	holders map[string]slotHolder

	pausedUntil time.Time
}

type slotHolder struct {
	urgent    bool
	onPreempt func()
}

func NewSessionLimiter(limit int) *SessionLimiter {
	if limit <= 0 {
		limit = 1
	}
	return &SessionLimiter{
		limit:   limit,
		holders: make(map[string]slotHolder),
	}
}

func (l *SessionLimiter) TryAcquire() bool {
	return l.tryAcquire("", false, nil, false)
}

func (l *SessionLimiter) TryAcquireIssue(issueID string, urgent bool, onPreempt func()) bool {
	return l.tryAcquire(issueID, urgent, onPreempt, false)
}

func (l *SessionLimiter) ForceAcquireIssue(issueID string, urgent bool, onPreempt func()) bool {
	return l.tryAcquire(issueID, urgent, onPreempt, true)
}

func (l *SessionLimiter) tryAcquire(issueID string, urgent bool, onPreempt func(), force bool) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isPausedLocked(time.Now().UTC()) {
		return false
	}
	if issueID != "" {
		if _, exists := l.holders[issueID]; exists {
			return true
		}
	}
	if !force {
		if urgent {
			// urgent sessions bypass the hard limit, but still participate in inUse accounting.
		} else if l.urgent > 0 || l.inUse >= l.limit {
			return false
		}
	}
	if !urgent && force && l.urgent > 0 {
		return false
	}
	l.inUse++
	if urgent {
		l.urgent++
	}
	if issueID != "" {
		l.holders[issueID] = slotHolder{
			urgent:    urgent,
			onPreempt: onPreempt,
		}
	}
	return true
}

func (l *SessionLimiter) Release() {
	l.ReleaseIssue("")
}

func (l *SessionLimiter) ReleaseIssue(issueID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if issueID != "" {
		holder, ok := l.holders[issueID]
		if !ok {
			return
		}
		delete(l.holders, issueID)
		if holder.urgent && l.urgent > 0 {
			l.urgent--
		}
		if l.inUse > 0 {
			l.inUse--
		}
		return
	}
	if l.inUse > 0 {
		l.inUse--
	}
}

func (l *SessionLimiter) Limit() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

func (l *SessionLimiter) InUse() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inUse
}

func (l *SessionLimiter) Available() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isPausedLocked(time.Now().UTC()) {
		return 0
	}
	return l.limit - l.inUse
}

func (l *SessionLimiter) HasUrgent() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.urgent > 0
}

func (l *SessionLimiter) PreemptNonUrgent(excludeIssueID string) []string {
	if l == nil {
		return nil
	}

	type callback struct {
		issueID string
		fn      func()
	}

	l.mu.Lock()
	callbacks := make([]callback, 0, len(l.holders))
	for issueID, holder := range l.holders {
		if issueID == excludeIssueID || holder.urgent || holder.onPreempt == nil {
			continue
		}
		callbacks = append(callbacks, callback{issueID: issueID, fn: holder.onPreempt})
	}
	l.mu.Unlock()

	preempted := make([]string, 0, len(callbacks))
	for _, cb := range callbacks {
		cb.fn()
		preempted = append(preempted, cb.issueID)
	}
	return preempted
}

func (l *SessionLimiter) PauseUntil(until time.Time) {
	if l == nil || until.IsZero() {
		return
	}

	until = until.UTC()

	l.mu.Lock()
	defer l.mu.Unlock()
	if until.After(l.pausedUntil) {
		l.pausedUntil = until
	}
}

func (l *SessionLimiter) PausedUntil() (time.Time, bool) {
	if l == nil {
		return time.Time{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.isPausedLocked(time.Now().UTC()) {
		return time.Time{}, false
	}
	return l.pausedUntil, true
}

func (l *SessionLimiter) isPausedLocked(now time.Time) bool {
	if l.pausedUntil.IsZero() {
		return false
	}
	if !l.pausedUntil.After(now) {
		l.pausedUntil = time.Time{}
		return false
	}
	return true
}
