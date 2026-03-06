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
	Health             string   `json:"health,omitempty"`
	SubprocessCount    int      `json:"subprocess_count"`
	RunningIssueIDs    []string `json:"running_issue_ids"`
	RetryCount         int      `json:"retry_count"`
	CrashCount         int      `json:"crash_count,omitempty"`
	QuarantinedAt      string   `json:"quarantined_at,omitempty"`
	TrackerConnected   bool     `json:"tracker_connected"`
	LastTrackerSuccess string   `json:"last_tracker_success,omitempty"`
	LastTrackerError   string   `json:"last_tracker_error,omitempty"`
	LastError          string   `json:"last_error,omitempty"`
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
		}
		for _, attempt := range st.Running {
			project.RunningIssueIDs = append(project.RunningIssueIDs, attempt.Identifier)
		}
		sort.Strings(project.RunningIssueIDs)

		project.TrackerConnected, project.LastTrackerSuccess, project.LastTrackerError = st.TrackerStatusLocked()
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
		} else if project.SubprocessCount > 0 {
			project.Status = "running"
		}
		projects = append(projects, project)
	}

	return BuildSummaryFromProjects(projects)
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
		summary.RunningIssueIDs = append(summary.RunningIssueIDs, project.RunningIssueIDs...)
		summary.Projects = append(summary.Projects, project)

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
