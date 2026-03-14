package status

import (
	"sort"
	"strings"
	"time"

	"symphony/internal/orchestrator"
	"symphony/internal/version"
)

type Summary struct {
	Status          string           `json:"status"`
	Version         string           `json:"version"`
	GitHash         string           `json:"git_hash"`
	Dirty           bool             `json:"dirty"`
	SubprocessCount int              `json:"subprocess_count"`
	RunningIssueIDs []string         `json:"running_issue_ids"`
	RetryCount      int              `json:"retry_count"`
	InputTokens     int64            `json:"input_tokens,omitempty"`
	OutputTokens    int64            `json:"output_tokens,omitempty"`
	TotalTokens     int64            `json:"total_tokens,omitempty"`
	PausedUntil     string           `json:"paused_until,omitempty"`
	PauseReason     string           `json:"pause_reason,omitempty"`
	ProjectCount    int              `json:"project_count"`
	Projects        []ProjectSummary `json:"projects"`
	UpdatedAt       string           `json:"updated_at"`
}

type RunningIssueSummary struct {
	Identifier      string `json:"identifier"`
	IssueState      string `json:"issue_state,omitempty"`
	Status          string `json:"status"`
	Priority        *int   `json:"priority,omitempty"`
	TurnCount       int    `json:"turn_count,omitempty"`
	LastEventAt     string `json:"last_event_at,omitempty"`
	LastEvent       string `json:"last_event,omitempty"`
	CurrentTaskAt   string `json:"current_task_at,omitempty"`
	CurrentTask     string `json:"current_task,omitempty"`
	ServerMessageAt string `json:"server_message_at,omitempty"`
	ServerMessage   string `json:"server_message,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	PID             string `json:"pid,omitempty"`
	InputTokens     int64  `json:"input_tokens,omitempty"`
	OutputTokens    int64  `json:"output_tokens,omitempty"`
	TotalTokens     int64  `json:"total_tokens,omitempty"`
}

type RetrySummary struct {
	Identifier string `json:"identifier"`
	Kind       string `json:"kind"`
	DueAt      string `json:"due_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

type SummarySource interface {
	GetSummary() Summary
}

type ProjectSummary struct {
	Name               string                `json:"name"`
	Status             string                `json:"status"`
	Health             string                `json:"health,omitempty"`
	SubprocessCount    int                   `json:"subprocess_count"`
	RunningIssueIDs    []string              `json:"running_issue_ids"`
	RunningIssues      []RunningIssueSummary `json:"running_issues,omitempty"`
	RetryCount         int                   `json:"retry_count"`
	RetryEntries       []RetrySummary        `json:"retry_entries,omitempty"`
	CrashCount         int                   `json:"crash_count,omitempty"`
	QuarantinedAt      string                `json:"quarantined_at,omitempty"`
	TrackerConnected   bool                  `json:"tracker_connected"`
	LastTrackerSuccess string                `json:"last_tracker_success,omitempty"`
	LastTrackerError   string                `json:"last_tracker_error,omitempty"`
	LastError          string                `json:"last_error,omitempty"`
	InputTokens        int64                 `json:"input_tokens,omitempty"`
	OutputTokens       int64                 `json:"output_tokens,omitempty"`
	TotalTokens        int64                 `json:"total_tokens,omitempty"`
	PausedUntil        string                `json:"paused_until,omitempty"`
	PauseReason        string                `json:"pause_reason,omitempty"`
}

func BuildSummary(states map[string]*orchestrator.State) Summary {
	projects := make([]ProjectSummary, 0, len(states))
	projectNames := make([]string, 0, len(states))
	for name := range states {
		projectNames = append(projectNames, name)
	}
	sort.Strings(projectNames)

	for _, name := range projectNames {
		st := states[name]
		if st == nil {
			continue
		}

		st.Lock()
		project := ProjectSummary{
			Name:            name,
			Status:          "idle",
			Health:          "healthy",
			SubprocessCount: len(st.Running),
			RetryCount:      len(st.RetryQueue),
			InputTokens:     st.Totals.InputTokens,
			OutputTokens:    st.Totals.OutputTokens,
			TotalTokens:     st.Totals.TotalTokens,
		}
		for _, attempt := range st.Running {
			project.RunningIssueIDs = append(project.RunningIssueIDs, attempt.Identifier)
			project.RunningIssues = append(project.RunningIssues, SummarizeRunningIssue(attempt))
		}
		sort.Strings(project.RunningIssueIDs)
		sort.Slice(project.RunningIssues, func(i, j int) bool {
			return project.RunningIssues[i].Identifier < project.RunningIssues[j].Identifier
		})

		failureRetryCount := 0
		for _, entry := range st.RetryQueue {
			project.RetryEntries = append(project.RetryEntries, RetrySummary{
				Identifier: entry.Identifier,
				Kind:       string(entry.Kind),
				DueAt:      entry.DueAt.UTC().Format(time.RFC3339),
				Error:      entry.Error,
			})
			if entry.Kind == orchestrator.RetryKindFailure {
				failureRetryCount++
			}
		}
		sort.Slice(project.RetryEntries, func(i, j int) bool {
			if project.RetryEntries[i].DueAt == project.RetryEntries[j].DueAt {
				return project.RetryEntries[i].Identifier < project.RetryEntries[j].Identifier
			}
			return project.RetryEntries[i].DueAt < project.RetryEntries[j].DueAt
		})

		project.TrackerConnected, project.LastTrackerSuccess, project.LastTrackerError = st.TrackerStatusLocked()
		if st.PausedUntil != nil {
			project.PausedUntil = st.PausedUntil.UTC().Format(time.RFC3339)
			project.PauseReason = st.PauseReason
		}
		if project.RetryCount > 0 {
			for _, retry := range st.RetryQueue {
				if retry.Error != "" {
					project.LastError = retry.Error
					break
				}
			}
		}
		st.Unlock()

		if !project.TrackerConnected {
			project.Status = "network_lost"
		} else if failureRetryCount > 0 {
			project.Status = "error"
		} else if project.SubprocessCount > 0 {
			project.Status = "running"
		}
		projects = append(projects, project)
	}

	return BuildSummaryFromProjects(projects)
}

func SummarizeRunningIssue(attempt *orchestrator.RunAttempt) RunningIssueSummary {
	if attempt == nil {
		return RunningIssueSummary{}
	}

	issue := RunningIssueSummary{
		Identifier: attempt.Identifier,
		IssueState: attempt.IssueState,
		Status:     string(attempt.GetStatus()),
		Priority:   attempt.IssuePriority,
	}
	if !attempt.StartedAt.IsZero() {
		issue.StartedAt = attempt.StartedAt.UTC().Format(time.RFC3339)
	}

	session := attempt.SessionSnapshot()
	issue.TurnCount = session.TurnCount
	issue.LastEvent = strings.TrimSpace(session.LastEvent)
	issue.CurrentTask = strings.TrimSpace(session.CurrentTask)
	issue.ServerMessage = strings.TrimSpace(session.ServerMessage)
	issue.SessionID = session.SessionID
	issue.PID = session.AgentPID
	issue.InputTokens = session.InputTokens
	issue.OutputTokens = session.OutputTokens
	issue.TotalTokens = session.TotalTokens
	if session.LastEventAt != nil {
		issue.LastEventAt = session.LastEventAt.UTC().Format(time.RFC3339)
	}
	if session.CurrentTaskAt != nil {
		issue.CurrentTaskAt = session.CurrentTaskAt.UTC().Format(time.RFC3339)
	}
	if session.ServerMessageAt != nil {
		issue.ServerMessageAt = session.ServerMessageAt.UTC().Format(time.RFC3339)
	}

	return issue
}

func BuildSummaryFromProjects(projects []ProjectSummary) Summary {
	build := version.Current()
	summary := Summary{
		Status:    "idle",
		Version:   build.Version,
		GitHash:   build.GitHash,
		Dirty:     build.Dirty,
		Projects:  make([]ProjectSummary, 0, len(projects)),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	var hasNetworkIssue bool
	var hasError bool
	var hasQuarantined bool

	sorted := append([]ProjectSummary(nil), projects...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	for _, project := range sorted {
		if project.Health == "" {
			project.Health = "healthy"
		}
		summary.SubprocessCount += project.SubprocessCount
		summary.RetryCount += project.RetryCount
		summary.InputTokens += project.InputTokens
		summary.OutputTokens += project.OutputTokens
		summary.TotalTokens += project.TotalTokens
		summary.RunningIssueIDs = append(summary.RunningIssueIDs, project.RunningIssueIDs...)
		summary.Projects = append(summary.Projects, project)
		if project.PausedUntil != "" {
			if summary.PausedUntil == "" || project.PausedUntil > summary.PausedUntil {
				summary.PausedUntil = project.PausedUntil
				summary.PauseReason = project.PauseReason
			}
		}

		if project.Health == "quarantined" || project.Health == "probing" {
			hasQuarantined = true
		}
		switch project.Status {
		case "error":
			hasError = true
		case "network_lost":
			hasNetworkIssue = true
		}
	}

	sort.Strings(summary.RunningIssueIDs)
	summary.ProjectCount = len(summary.Projects)

	switch {
	case hasQuarantined:
		summary.Status = "quarantined"
	case hasError:
		summary.Status = "error"
	case hasNetworkIssue:
		summary.Status = "network_lost"
	case summary.SubprocessCount > 0:
		summary.Status = "running"
	default:
		summary.Status = "idle"
	}

	return summary
}
