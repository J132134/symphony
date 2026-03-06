package orchestrator

import (
	"sync"
	"time"
)

// SessionLimiter caps daemon-wide concurrent agent sessions across projects.
type SessionLimiter struct {
	mu          sync.Mutex
	limit       int
	inUse       int
	pausedUntil time.Time
}

func NewSessionLimiter(limit int) *SessionLimiter {
	if limit <= 0 {
		limit = 1
	}
	return &SessionLimiter{limit: limit}
}

func (l *SessionLimiter) TryAcquire() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isPausedLocked(time.Now().UTC()) {
		return false
	}
	if l.inUse >= l.limit {
		return false
	}
	l.inUse++
	return true
}

func (l *SessionLimiter) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
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
