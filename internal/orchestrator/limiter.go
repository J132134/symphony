package orchestrator

import "sync"

// SessionLimiter caps daemon-wide concurrent agent sessions across projects.
type SessionLimiter struct {
	mu    sync.Mutex
	limit int
	inUse int
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
	return l.limit - l.inUse
}
