package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"symphony/internal/config"
	"symphony/internal/status"
	"symphony/internal/version"
)

type summaryClient interface {
	Summary() (status.Summary, error)
}

var newSummaryClient = func(baseURL string) summaryClient {
	return status.NewClient(baseURL)
}

func cmdStatus(args []string) {
	if err := runStatus(os.Stdout, args); err != nil {
		fatalf("status: %v", err)
	}
}

func runStatus(stdout io.Writer, args []string) error {
	opts := parseFlags(args, map[string]string{
		"--config": "",
		"--url":    "",
		"--poll":   "3s",
	})
	jsonOutput := hasFlag(args, "--json")
	once := hasFlag(args, "--once")

	baseURL, err := resolveStatusBaseURL(opts["--url"], opts["--config"])
	if err != nil {
		return err
	}

	client := newSummaryClient(baseURL)

	if jsonOutput {
		summary, err := client.Summary()
		if err != nil {
			return err
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(summary)
	}

	poll := 3 * time.Second
	if parsed, err := time.ParseDuration(opts["--poll"]); err == nil && parsed > 0 {
		poll = parsed
	}
	if !once && wantsLiveStatus(stdout) {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return watchStatus(ctx, stdout, client, poll)
	}

	summary, err := client.Summary()
	if err != nil {
		return err
	}
	_, err = io.WriteString(stdout, formatStatusSummary(summary))
	return err
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

func resolveStatusBaseURL(rawURL, configPath string) (string, error) {
	if trimmed := strings.TrimSpace(rawURL); trimmed != "" {
		return trimmed, nil
	}

	cfg, err := config.LoadDaemonConfig(configPath)
	if err == nil {
		if !cfg.StatusServer.Enabled {
			path := cfg.ConfigPath
			if path == "" {
				path = "daemon config"
			}
			return "", fmt.Errorf("status server is disabled in %s", path)
		}
		return fmt.Sprintf("http://127.0.0.1:%d", cfg.StatusServer.Port), nil
	}
	if strings.TrimSpace(configPath) != "" || !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	return status.DefaultBaseURL, nil
}

func formatStatusSummary(summary status.Summary) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Status: %s\n", summary.Status)
	fmt.Fprintf(&b, "Projects: %d\n", summary.ProjectCount)
	fmt.Fprintf(&b, "Subprocesses: %d\n", summary.SubprocessCount)
	fmt.Fprintf(&b, "Retries: %d\n", summary.RetryCount)
	if summary.UpdatedAt != "" {
		fmt.Fprintf(&b, "Updated: %s\n", summary.UpdatedAt)
	}
	versionLabel := summary.Version
	if short := version.ShortHash(summary.GitHash); short != "" {
		if versionLabel != "" {
			versionLabel += " "
		}
		versionLabel += short
		if summary.Dirty {
			versionLabel += " dirty"
		}
	}
	if versionLabel != "" {
		fmt.Fprintf(&b, "Version: %s\n", versionLabel)
	}

	for _, project := range summary.Projects {
		fmt.Fprintf(&b, "\n[%s] %s", project.Name, project.Status)
		if project.Health != "" {
			fmt.Fprintf(&b, " (%s)", project.Health)
		}
		b.WriteByte('\n')
		fmt.Fprintf(&b, "  tracker: %s\n", trackerLabel(project))
		fmt.Fprintf(&b, "  subprocesses: %d\n", project.SubprocessCount)
		fmt.Fprintf(&b, "  retries: %d\n", project.RetryCount)
		if project.LastError != "" {
			fmt.Fprintf(&b, "  last error: %s\n", project.LastError)
		}
		if len(project.RunningIssues) == 0 {
			continue
		}
		b.WriteString("  running issues:\n")
		for _, issue := range project.RunningIssues {
			fmt.Fprintf(&b, "    - %s | %s", issue.Identifier, issue.Status)
			if issue.TurnCount > 0 {
				fmt.Fprintf(&b, " | turn %d", issue.TurnCount)
			}
			if issue.LastEventAt != "" {
				fmt.Fprintf(&b, " | last event %s", issue.LastEventAt)
			}
			b.WriteByte('\n')
			if issue.IssueState != "" {
				fmt.Fprintf(&b, "      tracker state: %s\n", issue.IssueState)
			}
			if issue.CurrentActivity != "" {
				fmt.Fprintf(&b, "      current: %s\n", issue.CurrentActivity)
				if issue.CurrentActivityAt != "" {
					fmt.Fprintf(&b, "      current at: %s\n", issue.CurrentActivityAt)
				}
			}
			if runtime := formatIssueRuntime(issue); runtime != "" {
				fmt.Fprintf(&b, "      runtime: %s\n", runtime)
			}
			if issue.LastEvent != "" && issue.LastEvent != issue.CurrentActivity {
				fmt.Fprintf(&b, "      last event: %s\n", issue.LastEvent)
			}
			if len(issue.RecentEvents) > 0 {
				b.WriteString("      recent events:\n")
				for _, event := range issue.RecentEvents {
					fmt.Fprintf(&b, "        * %s\n", formatIssueEvent(event))
				}
			}
		}
	}

	return b.String()
}

func trackerLabel(project status.ProjectSummary) string {
	if project.TrackerConnected {
		if project.LastTrackerSuccess != "" {
			return "connected (" + project.LastTrackerSuccess + ")"
		}
		return "connected"
	}
	if project.LastTrackerError != "" {
		return "disconnected (" + project.LastTrackerError + ")"
	}
	return "disconnected"
}

func formatIssueRuntime(issue status.RunningIssueSummary) string {
	var parts []string
	if issue.SessionID != "" {
		parts = append(parts, "session "+issue.SessionID)
	}
	if issue.PID != "" {
		parts = append(parts, "pid "+issue.PID)
	}
	if tokens := formatIssueTokenUsage(issue); tokens != "" {
		parts = append(parts, tokens)
	}
	return strings.Join(parts, " | ")
}

func formatIssueTokenUsage(issue status.RunningIssueSummary) string {
	if issue.InputTokens <= 0 && issue.OutputTokens <= 0 && issue.TotalTokens <= 0 {
		return ""
	}

	parts := make([]string, 0, 3)
	if issue.InputTokens > 0 {
		parts = append(parts, "in "+formatInt64(issue.InputTokens))
	}
	if issue.OutputTokens > 0 {
		parts = append(parts, "out "+formatInt64(issue.OutputTokens))
	}
	if issue.TotalTokens > 0 {
		parts = append(parts, "total "+formatInt64(issue.TotalTokens))
	}
	return "tokens " + strings.Join(parts, " / ")
}

func formatIssueEvent(event status.RunningEventDetail) string {
	var parts []string
	if event.OccurredAt != "" {
		parts = append(parts, event.OccurredAt)
	}
	detail := strings.TrimSpace(event.Detail)
	if detail == "" {
		detail = strings.TrimSpace(event.Message)
	}
	if detail == "" {
		detail = strings.TrimSpace(event.Name)
	}
	if detail != "" {
		parts = append(parts, detail)
	}
	return strings.Join(parts, " | ")
}
