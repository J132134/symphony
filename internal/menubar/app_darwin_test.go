//go:build darwin

package menubar

import (
	"errors"
	"strings"
	"testing"

	"symphony/internal/status"
)

func TestBuildTooltipIncludesRetryBreakdown(t *testing.T) {
	t.Parallel()

	summary := status.Summary{
		SubprocessCount:    2,
		RetryCount:         3,
		FailureRetryCount:  2,
		CapacityWaitCount:  1,
		UpdatedAt:          "2026-03-06T14:00:00Z",
		RunningIssueIDs:    []string{"J-18", "J-21"},
	}

	tooltip := buildTooltip(summary, "Error", "abc123", "J-18, J-21", errors.New("daemon unreachable"))
	for _, want := range []string{
		"Status: Error",
		"Version: abc123",
		"Subprocesses: 2",
		"Issues: J-18, J-21",
		"Retries: 3",
		"Failure Retries: 2",
		"Capacity Waits: 1",
		"Updated: 2026-03-06T14:00:00Z",
		"Error: daemon unreachable",
	} {
		if !strings.Contains(tooltip, want) {
			t.Fatalf("tooltip missing %q:\n%s", want, tooltip)
		}
	}
}

func TestBuildTooltipOmitsRetryBreakdownWhenIdle(t *testing.T) {
	t.Parallel()

	summary := status.Summary{
		SubprocessCount: 0,
		RetryCount:      0,
	}

	tooltip := buildTooltip(summary, "Idle", "abc123", "none", nil)
	for _, unwanted := range []string{
		"Retries:",
		"Failure Retries:",
		"Capacity Waits:",
		"Error:",
		"Updated:",
	} {
		if strings.Contains(tooltip, unwanted) {
			t.Fatalf("tooltip unexpectedly contains %q:\n%s", unwanted, tooltip)
		}
	}
}
