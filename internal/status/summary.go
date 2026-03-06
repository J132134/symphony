package status

import (
	"sort"
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
	ProjectCount    int              `json:"project_count"`
	Projects        []ProjectSummary `json:"projects"`
	UpdatedAt       string           `json:"updated_at"`
}

type SummarySource interface {
	GetSummary() Summary
}

type ProjectSummary struct {
	Name               string   `json:"name"`
	Status             string   `json:"status"`
	SubprocessCount    int      `json:"subprocess_count"`
	RunningIssueIDs    []string `json:"running_issue_ids"`
	RetryCount         int      `json:"retry_count"`
	Paused             bool     `json:"paused"`
	PausedUntil        string   `json:"paused_until,omitempty"`
	PauseReason        string   `json:"pause_reason,omitempty"`
	TrackerConnected   bool     `json:"tracker_connected"`
	LastTrackerSuccess string   `json:"last_tracker_success,omitempty"`
	LastTrackerError   string   `json:"last_tracker_error,omitempty"`
	LastError          string   `json:"last_error,omitempty"`
}

func BuildSummary(states map[string]*orchestrator.State) Summary {
	build := version.Current()
	summary := Summary{
		Status:    "idle",
		Version:   build.Version,
		GitHash:   build.GitHash,
		Dirty:     build.Dirty,
		Projects:  make([]ProjectSummary, 0, len(states)),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	now := time.Now().UTC()

	var hasNetworkIssue bool
	var hasError bool
	var hasPaused bool

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
			SubprocessCount: len(st.Running),
			RetryCount:      len(st.RetryQueue),
		}
		for _, attempt := range st.Running {
			project.RunningIssueIDs = append(project.RunningIssueIDs, attempt.Identifier)
			summary.RunningIssueIDs = append(summary.RunningIssueIDs, attempt.Identifier)
		}
		sort.Strings(project.RunningIssueIDs)

		project.TrackerConnected, project.LastTrackerSuccess, project.LastTrackerError = st.TrackerStatusLocked()
		project.Paused, project.PausedUntil, project.PauseReason = st.PauseStatusLocked(now)
		if project.Paused {
			hasPaused = true
		}
		if !project.TrackerConnected {
			hasNetworkIssue = true
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
		} else if project.RetryCount > 0 {
			project.Status = "error"
			hasError = true
		} else if project.SubprocessCount > 0 {
			project.Status = "running"
		} else if project.Paused {
			project.Status = "paused"
		}

		summary.SubprocessCount += project.SubprocessCount
		summary.RetryCount += project.RetryCount
		summary.Projects = append(summary.Projects, project)
	}

	sort.Strings(summary.RunningIssueIDs)
	summary.ProjectCount = len(summary.Projects)

	switch {
	case hasError:
		summary.Status = "error"
	case hasNetworkIssue:
		summary.Status = "network_lost"
	case summary.SubprocessCount > 0:
		summary.Status = "running"
	case hasPaused:
		summary.Status = "paused"
	default:
		summary.Status = "idle"
	}

	return summary
}
