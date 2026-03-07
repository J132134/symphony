//go:build darwin

package menubar

import "testing"

func TestInitialSummaryStartsIdle(t *testing.T) {
	summary := initialSummary()

	if summary.Status != "idle" {
		t.Fatalf("status = %q, want idle", summary.Status)
	}
	if summary.RunningIssueIDs == nil {
		t.Fatal("running_issue_ids = nil, want initialized slice")
	}
}

func TestMenuBarStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    string
		lastErr   bool
		wantIcon  string
		wantLabel string
	}{
		{
			name:      "idle",
			status:    "idle",
			wantIcon:  "○",
			wantLabel: "Idle",
		},
		{
			name:      "network_lost",
			status:    "network_lost",
			wantIcon:  "⏸",
			wantLabel: "Network lost",
		},
		{
			name:      "running",
			status:    "running",
			wantIcon:  spinnerFrames[0],
			wantLabel: "Running",
		},
		{
			name:      "daemon_unreachable",
			status:    "idle",
			lastErr:   true,
			wantIcon:  "⏸",
			wantLabel: "Daemon unreachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var err error
			if tt.lastErr {
				err = testErr("boom")
			}

			icon, label := menuBarStatus(tt.status, err, 0)
			if icon != tt.wantIcon {
				t.Fatalf("icon = %q, want %q", icon, tt.wantIcon)
			}
			if label != tt.wantLabel {
				t.Fatalf("label = %q, want %q", label, tt.wantLabel)
			}
		})
	}
}

type testErr string

func (e testErr) Error() string { return string(e) }
