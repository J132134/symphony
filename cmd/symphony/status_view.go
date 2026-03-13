package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"symphony/internal/status"
	"symphony/internal/version"
)

const (
	clearScreen = "\x1b[H\x1b[2J"
	hideCursor  = "\x1b[?25l"
	showCursor  = "\x1b[?25h"
)

func wantsLiveStatus(stdout io.Writer) bool {
	file, ok := stdout.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func watchStatus(ctx context.Context, stdout io.Writer, client summaryClient, poll time.Duration) error {
	state := liveStatusState{
		poll:        poll,
		nextRefresh: time.Now(),
	}
	if err := state.refresh(client); err != nil {
		state.lastErr = err
	}

	if _, err := io.WriteString(stdout, hideCursor); err != nil {
		return err
	}
	defer func() {
		_, _ = io.WriteString(stdout, showCursor)
	}()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if _, err := io.WriteString(stdout, clearScreen); err != nil {
			return err
		}
		if _, err := io.WriteString(stdout, renderStatusDashboard(state.summary, state.lastErr, time.Now(), state.nextRefresh, state.poll)); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if !time.Now().Before(state.nextRefresh) {
				if err := state.refresh(client); err != nil {
					state.lastErr = err
				}
			}
		}
	}
}

type liveStatusState struct {
	summary     status.Summary
	lastErr     error
	poll        time.Duration
	nextRefresh time.Time
}

func (s *liveStatusState) refresh(client summaryClient) error {
	summary, err := client.Summary()
	s.nextRefresh = time.Now().Add(s.poll)
	if err != nil {
		return err
	}
	s.summary = summary
	s.lastErr = nil
	return nil
}

func renderStatusDashboard(summary status.Summary, lastErr error, now, nextRefresh time.Time, poll time.Duration) string {
	var b strings.Builder

	fmt.Fprintln(&b, "SYMPHONY STATUS")
	fmt.Fprintf(&b, "Status: %s\n", strings.ToUpper(summary.Status))
	fmt.Fprintf(&b, "Agents: %d\n", summary.SubprocessCount)
	fmt.Fprintf(&b, "Projects: %d\n", summary.ProjectCount)
	fmt.Fprintf(&b, "Tokens: in %s | out %s | total %s\n", formatInt64(summary.InputTokens), formatInt64(summary.OutputTokens), formatInt64(summary.TotalTokens))
	fmt.Fprintf(&b, "Rate limits: %s\n", formatPauseStatus(summary, now))
	if summary.UpdatedAt != "" {
		fmt.Fprintf(&b, "Updated: %s\n", summary.UpdatedAt)
	}
	if summary.Version != "" {
		fmt.Fprintf(&b, "Version: %s\n", buildVersionLabel(summary))
	}
	if lastErr != nil {
		fmt.Fprintf(&b, "Daemon error: %s\n", lastErr)
	}
	fmt.Fprintf(&b, "Next refresh: %s\n", formatRefreshCountdown(nextRefresh, now, poll))

	running := flattenRunningIssues(summary.Projects)
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "- Running")
	if len(running) == 0 {
		fmt.Fprintln(&b, "No running agents")
	} else {
		fmt.Fprintln(&b, "PROJECT          ID         STAGE               P   AGE / TURN      TOKENS       SESSION        ACTIVITY")
		fmt.Fprintln(&b, "----------------------------------------------------------------------------------------------------------")
		for _, issue := range running {
			activity := issue.issue.CurrentActivity
			if activity == "" {
				activity = issue.issue.LastEvent
			}
			fmt.Fprintf(
				&b,
				"%-16s %-10s %-19s %-3s %-15s %-12s %-14s %s\n",
				truncate(issue.project, 16),
				issue.issue.Identifier,
				truncate(humanizeStatus(issue.issue.Status), 19),
				formatPriority(issue.issue.Priority),
				formatAgeTurn(issue.issue, now),
				formatCompactTokens(issue.issue.TotalTokens),
				truncate(shortSession(issue.issue.SessionID), 14),
				truncate(activity, 40),
			)
		}
		fmt.Fprintln(&b, "")
		fmt.Fprintln(&b, "Running details")
		for _, issue := range running {
			fmt.Fprintln(&b, renderRunningIssueDetail(issue.issue, issue.project))
		}
	}

	retries := flattenRetryEntries(summary.Projects)
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "- Backoff queue")
	if len(retries) == 0 {
		fmt.Fprintln(&b, "No queued retries")
	} else {
		fmt.Fprintln(&b, "PROJECT          ID         KIND           DUE IN        ERROR")
		fmt.Fprintln(&b, "--------------------------------------------------------------------------")
		for _, entry := range retries {
			fmt.Fprintf(
				&b,
				"%-16s %-10s %-14s %-13s %s\n",
				truncate(entry.project, 16),
				entry.retry.Identifier,
				truncate(entry.retry.Kind, 14),
				formatDueIn(entry.retry.DueAt, now),
				truncate(entry.retry.Error, 40),
			)
		}
	}

	return b.String()
}

type projectIssue struct {
	project string
	issue   status.RunningIssueSummary
}

type projectRetry struct {
	project string
	retry   status.RetrySummary
}

func flattenRunningIssues(projects []status.ProjectSummary) []projectIssue {
	var issues []projectIssue
	for _, project := range projects {
		for _, issue := range project.RunningIssues {
			issues = append(issues, projectIssue{project: project.Name, issue: issue})
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].project == issues[j].project {
			return issues[i].issue.Identifier < issues[j].issue.Identifier
		}
		return issues[i].project < issues[j].project
	})
	return issues
}

func flattenRetryEntries(projects []status.ProjectSummary) []projectRetry {
	var entries []projectRetry
	for _, project := range projects {
		for _, retry := range project.RetryEntries {
			entries = append(entries, projectRetry{project: project.Name, retry: retry})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].retry.DueAt == entries[j].retry.DueAt {
			if entries[i].project == entries[j].project {
				return entries[i].retry.Identifier < entries[j].retry.Identifier
			}
			return entries[i].project < entries[j].project
		}
		return entries[i].retry.DueAt < entries[j].retry.DueAt
	})
	return entries
}

func formatPauseStatus(summary status.Summary, now time.Time) string {
	if summary.PausedUntil == "" {
		return "unavailable"
	}
	until, err := time.Parse(time.RFC3339, summary.PausedUntil)
	if err != nil {
		return summary.PauseReason
	}
	if until.Before(now) {
		return "available"
	}
	if summary.PauseReason == "" {
		return "paused until " + until.Format("15:04:05")
	}
	return fmt.Sprintf("%s until %s", strings.ReplaceAll(summary.PauseReason, "_", " "), until.Format("15:04:05"))
}

func formatRefreshCountdown(nextRefresh, now time.Time, poll time.Duration) string {
	if nextRefresh.IsZero() {
		return poll.String()
	}
	if nextRefresh.Before(now) {
		return "0s"
	}
	remaining := nextRefresh.Sub(now).Round(time.Second)
	if remaining < time.Second {
		remaining = time.Second
	}
	return remaining.String()
}

func formatAgeTurn(issue status.RunningIssueSummary, now time.Time) string {
	age := "n/a"
	if issue.StartedAt != "" {
		if startedAt, err := time.Parse(time.RFC3339, issue.StartedAt); err == nil {
			age = humanDuration(now.Sub(startedAt))
		}
	}
	if issue.TurnCount > 0 {
		return fmt.Sprintf("%s / %d", age, issue.TurnCount)
	}
	return age
}

func formatDueIn(dueAt string, now time.Time) string {
	if dueAt == "" {
		return "n/a"
	}
	due, err := time.Parse(time.RFC3339, dueAt)
	if err != nil {
		return dueAt
	}
	if due.Before(now) {
		return "ready"
	}
	return humanDuration(due.Sub(now))
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	hours := int(d / time.Hour)
	minutes := int((d % time.Hour) / time.Minute)
	seconds := int((d % time.Minute) / time.Second)
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

func formatPriority(priority *int) string {
	if priority == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *priority)
}

func formatCompactTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func formatInt64(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
}

func shortSession(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if len(sessionID) <= 14 {
		return sessionID
	}
	return sessionID[:5] + "..." + sessionID[len(sessionID)-6:]
}

func humanizeStatus(v string) string {
	v = strings.TrimSpace(strings.ReplaceAll(v, "_", " "))
	if v == "" {
		return "Unknown"
	}
	parts := strings.Fields(v)
	for i, part := range parts {
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func truncate(v string, max int) string {
	if max <= 0 || len(v) <= max {
		return v
	}
	if max <= 3 {
		return v[:max]
	}
	return v[:max-3] + "..."
}

func buildVersionLabel(summary status.Summary) string {
	label := summary.Version
	if short := version.ShortHash(summary.GitHash); short != "" {
		if label != "" {
			label += " "
		}
		label += short
		if summary.Dirty {
			label += " dirty"
		}
	}
	return label
}

func renderRunningIssueDetail(issue status.RunningIssueSummary, project string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "[%s] %s\n", project, issue.Identifier)
	fmt.Fprintf(&b, "  stage: %s", humanizeStatus(issue.Status))
	if issue.IssueState != "" {
		fmt.Fprintf(&b, " | tracker: %s", issue.IssueState)
	}
	if issue.Attempt > 0 {
		fmt.Fprintf(&b, " | attempt: %d", issue.Attempt)
	}
	if issue.Branch != "" {
		fmt.Fprintf(&b, "\n  branch: %s", issue.Branch)
	}
	if flags := formatIssueExecutionFlags(issue); flags != "" {
		fmt.Fprintf(&b, "\n  execution: %s", flags)
	}
	if issue.CurrentActivity != "" {
		fmt.Fprintf(&b, "\n  current: %s", issue.CurrentActivity)
		if issue.CurrentActivityAt != "" {
			fmt.Fprintf(&b, "\n  current at: %s", issue.CurrentActivityAt)
		}
	}
	if runtime := formatIssueRuntime(issue); runtime != "" {
		fmt.Fprintf(&b, "\n  runtime: %s", runtime)
	}
	if issue.LastEvent != "" && issue.LastEvent != issue.CurrentActivity {
		fmt.Fprintf(&b, "\n  last event: %s", issue.LastEvent)
	}
	if len(issue.RecentEvents) > 0 {
		b.WriteString("\n  recent:")
		for _, event := range issue.RecentEvents {
			fmt.Fprintf(&b, "\n    - %s", formatIssueEvent(event))
		}
	}

	return b.String()
}
