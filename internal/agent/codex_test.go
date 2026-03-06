package agent

import (
	"testing"
	"time"
)

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
