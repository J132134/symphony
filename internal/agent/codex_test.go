package agent

import (
	"context"
	"testing"
	"time"
)

func TestRunnerDispatchLineKeepsTerminalNotificationWhenNotifQueueFull(t *testing.T) {
	r := NewRunner()
	fillNotifQueue(r)

	r.dispatchLine(mustNotification(t, methodTurnCompleted, nil))

	if got := len(r.notifCh); got != cap(r.notifCh) {
		t.Fatalf("notifCh len = %d, want %d", got, cap(r.notifCh))
	}
	if got := len(r.priorityNotifCh); got != 1 {
		t.Fatalf("priorityNotifCh len = %d, want 1", got)
	}
}

func TestRunnerDispatchLineKeepsRateLimitNotificationWhenNotifQueueFull(t *testing.T) {
	r := NewRunner()
	fillNotifQueue(r)

	r.dispatchLine(mustNotification(t, methodRateLimits, nil))

	if got := len(r.notifCh); got != cap(r.notifCh) {
		t.Fatalf("notifCh len = %d, want %d", got, cap(r.notifCh))
	}
	if got := len(r.priorityNotifCh); got != 1 {
		t.Fatalf("priorityNotifCh len = %d, want 1", got)
	}
}

func TestRunnerConsumeUntilDoneReadsTerminalNotificationWhenNotifQueueFull(t *testing.T) {
	r := NewRunner()
	fillNotifQueue(r)
	r.dispatchLine(mustNotification(t, methodTurnCompleted, nil))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result := r.consumeUntilDone(ctx, "thread-1", "turn-1", nil)
	if !result.Success {
		t.Fatalf("Success = false, want true (error=%q)", result.Error)
	}
	if !result.CompletedNaturally {
		t.Fatalf("CompletedNaturally = false, want true")
	}
}

func TestParseRateLimitEventResetAtRFC3339(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC)
	event := parseRateLimitEvent(map[string]any{
		"limits": []any{
			map[string]any{"resetAt": "2026-03-07T00:05:00Z"},
		},
	}, now)

	if event == nil || event.ResetAt == nil {
		t.Fatal("expected resetAt to be parsed")
	}
	want := time.Date(2026, 3, 7, 0, 5, 0, 0, time.UTC)
	if !event.ResetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", event.ResetAt, want)
	}
}

func TestParseRateLimitEventRetryAfterSeconds(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC)
	event := parseRateLimitEvent(map[string]any{
		"retryAfter": 12,
	}, now)

	if event == nil || event.ResetAt == nil {
		t.Fatal("expected retryAfter to be parsed")
	}
	want := now.Add(12 * time.Second)
	if !event.ResetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", event.ResetAt, want)
	}
}

func fillNotifQueue(r *Runner) {
	for i := 0; i < cap(r.notifCh); i++ {
		r.notifCh <- &Incoming{Notif: &Notification{Method: methodRateLimits}}
	}
}

func mustNotification(t *testing.T, method string, params map[string]any) []byte {
	t.Helper()

	msg, err := FormatNotification(method, params)
	if err != nil {
		t.Fatalf("FormatNotification(%q): %v", method, err)
	}
	return msg
}
