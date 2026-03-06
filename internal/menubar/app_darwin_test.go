//go:build darwin

package menubar

import (
	"strings"
	"testing"

	"symphony/internal/status"
)

func TestMenuBarStatusPaused(t *testing.T) {
	t.Parallel()

	icon, label := menuBarStatus("paused", nil, 0)
	if icon != "⏸" {
		t.Fatalf("icon = %q, want ⏸", icon)
	}
	if label != "Paused" {
		t.Fatalf("label = %q, want Paused", label)
	}
}

func TestPauseTooltipLineIncludesProjectReasonAndUntil(t *testing.T) {
	t.Parallel()

	line := pauseTooltipLine(status.Summary{
		Projects: []status.ProjectSummary{
			{
				Name:        "alpha",
				Paused:      true,
				PausedUntil: "2026-03-07T01:30:00Z",
				PauseReason: "global_rate_limit",
			},
		},
	})

	for _, want := range []string{"alpha", "global_rate_limit", "2026-03-07T01:30:00Z"} {
		if !strings.Contains(line, want) {
			t.Fatalf("pause tooltip %q missing %q", line, want)
		}
	}
}
