package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"symphony/internal/config"
	"symphony/internal/tracker"
	"symphony/internal/workspace"
)

type workspaceSummary struct {
	Branch            string
	ModifiedFiles     []string
	LastCommitHash    string
	LastCommitSubject string
	RemoteURL         string
	PRURL             string
}

func (o *Orchestrator) maybePostSuccessFeedback(ctx context.Context, cfg *config.SymphonyConfig, tr *tracker.LinearClient, issueID string, attempt *RunAttempt) {
	if tr == nil {
		return
	}

	summary, summaryErr := collectWorkspaceSummary(attempt.WorkspacePath, cfg)

	if cfg.TrackerPostComments() {
		if summaryErr != nil {
			slog.Warn("orchestrator.comment_build_failed", "issue_id", issueID, "error", summaryErr)
		} else if body, err := buildSuccessComment(cfg, attempt, summary); err != nil {
			slog.Warn("orchestrator.comment_build_failed", "issue_id", issueID, "error", err)
		} else if err := tr.AddComment(ctx, issueID, body); err != nil {
			slog.Warn("orchestrator.comment_post_failed", "issue_id", issueID, "error", err)
		}
	}

	if state := cfg.TrackerOnSuccessState(); state != "" {
		if err := tr.UpdateIssueState(ctx, issueID, state); err != nil {
			slog.Warn("orchestrator.success_state_update_failed", "issue_id", issueID, "state", state, "error", err)
		} else if shouldAttachPRLink(state, summary, summaryErr) {
			linkURL := resolvePRLinkURL(summary, lookupGitHubPRURL)
			if err := tr.AddLink(ctx, issueID, "PR", linkURL); err != nil {
				slog.Warn("orchestrator.pr_link_add_failed", "issue_id", issueID, "state", state, "pr_url", linkURL, "error", err)
			}
		}
	}
}

func (o *Orchestrator) maybePostFinalFailureFeedback(ctx context.Context, cfg *config.SymphonyConfig, tr *tracker.LinearClient, issueID string, attempt *RunAttempt, err error) {
	if tr == nil {
		return
	}

	if cfg.TrackerPostComments() {
		body, buildErr := buildFailureComment(cfg, attempt, err)
		if buildErr != nil {
			slog.Warn("orchestrator.comment_build_failed", "issue_id", issueID, "error", buildErr)
		} else if postErr := tr.AddComment(ctx, issueID, body); postErr != nil {
			slog.Warn("orchestrator.comment_post_failed", "issue_id", issueID, "error", postErr)
		}
	}

	if state := cfg.TrackerOnFailureState(); state != "" {
		if err := tr.UpdateIssueState(ctx, issueID, state); err != nil {
			slog.Warn("orchestrator.failure_state_update_failed", "issue_id", issueID, "state", state, "error", err)
		}
	}
}

func buildSuccessComment(cfg *config.SymphonyConfig, attempt *RunAttempt, summary workspaceSummary) (string, error) {
	session := attempt.SessionSnapshot()

	lines := []string{
		fmt.Sprintf("✅ **Symphony agent completed** (attempt %d, turn %d/%d)", attempt.Attempt, max(1, session.TurnCount), cfg.MaxTurns()),
		"",
		fmt.Sprintf("**Duration:** %s", humanDuration(time.Since(attempt.StartedAt))),
		fmt.Sprintf("**Tokens:** %s (in: %s / out: %s)", formatInt(session.TotalTokens), formatInt(session.InputTokens), formatInt(session.OutputTokens)),
		"",
		"**Changes:**",
		fmt.Sprintf("- Modified: %s", joinOrDefault(summary.ModifiedFiles, "none detected")),
	}

	if summary.LastCommitHash != "" || summary.LastCommitSubject != "" {
		lines = append(lines, fmt.Sprintf("- Last commit: `%s`", formatLastCommit(summary.LastCommitHash, summary.LastCommitSubject)))
	}
	if summary.PRURL != "" {
		lines = append(lines, fmt.Sprintf("- PR: %s", summary.PRURL))
	}
	if summary.Branch != "" {
		lines = append(lines, "", fmt.Sprintf("**Branch:** `%s`", escapeCode(summary.Branch)))
	}

	return strings.Join(lines, "\n"), nil
}

func shouldAttachPRLink(state string, summary workspaceSummary, summaryErr error) bool {
	if summaryErr != nil || summary.PRURL == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(state), humanReviewState)
}

func resolvePRLinkURL(summary workspaceSummary, lookup func(owner, repo, branch string) string) string {
	owner, repo := parseGitHubRemote(summary.RemoteURL)
	branch := strings.TrimSpace(summary.Branch)
	if lookup != nil && owner != "" && repo != "" && branch != "" {
		if actual := strings.TrimSpace(lookup(owner, repo, branch)); actual != "" {
			return actual
		}
	}
	return summary.PRURL
}

func lookupGitHubPRURL(owner, repo, branch string) string {
	if owner == "" || repo == "" || branch == "" {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx,
		"gh", "pr", "list",
		"--repo", owner+"/"+repo,
		"--head", branch,
		"--state", "all",
		"--json", "url",
	)
	out, err := cmd.CombinedOutput()
	if err != nil || ctx.Err() != nil {
		return ""
	}

	var payload []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out, &payload); err != nil || len(payload) == 0 {
		return ""
	}
	return strings.TrimSpace(payload[0].URL)
}

func buildFailureComment(cfg *config.SymphonyConfig, attempt *RunAttempt, err error) (string, error) {
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	if errMsg == "" {
		errMsg = attempt.Error
	}
	if errMsg == "" {
		errMsg = "unknown error"
	}

	duration := humanDuration(time.Since(attempt.StartedAt))
	if attempt.GetStatus() == StatusStalled {
		duration += " (stalled)"
	}

	lines := []string{
		fmt.Sprintf("❌ **Symphony agent failed** (attempt %d/%d)", attempt.Attempt, cfg.MaxAttempts()),
		"",
		fmt.Sprintf("**Error:** %s", errMsg),
		fmt.Sprintf("**Duration:** %s", duration),
		fmt.Sprintf("**Last status:** %s", lastStatusLine(attempt)),
	}
	return strings.Join(lines, "\n"), nil
}

func collectWorkspaceSummary(workspacePath string, cfg *config.SymphonyConfig) (workspaceSummary, error) {
	if strings.TrimSpace(workspacePath) == "" {
		return workspaceSummary{}, fmt.Errorf("workspace path is empty")
	}

	branch, err := workspace.GitOutput(workspacePath, "branch", "--show-current")
	if err != nil {
		return workspaceSummary{}, err
	}

	lastCommitRaw, err := workspace.GitOutput(workspacePath, "log", "-1", "--pretty=format:%H%n%s")
	if err != nil {
		return workspaceSummary{}, err
	}
	lastCommitLines := strings.SplitN(lastCommitRaw, "\n", 2)

	modified, err := changedFiles(workspacePath)
	if err != nil {
		return workspaceSummary{}, err
	}

	remoteURL, err := workspace.GitOutput(workspacePath, "remote", "get-url", "origin")
	if err != nil {
		remoteURL = ""
	}

	summary := workspaceSummary{
		Branch:            strings.TrimSpace(branch),
		ModifiedFiles:     modified,
		LastCommitHash:    strings.TrimSpace(firstLine(lastCommitLines, 0)),
		LastCommitSubject: strings.TrimSpace(firstLine(lastCommitLines, 1)),
		RemoteURL:         strings.TrimSpace(remoteURL),
	}
	summary.PRURL = buildPRURL(cfg, summary)
	return summary, nil
}

func changedFiles(workspacePath string) ([]string, error) {
	statusOut, err := workspace.GitOutput(workspacePath, "status", "--short", "--untracked-files=all")
	if err != nil {
		return nil, err
	}

	files := parseStatusPaths(statusOut)
	if len(files) > 0 {
		return files, nil
	}

	showOut, err := workspace.GitOutput(workspacePath, "show", "--pretty=format:", "--name-only", "HEAD")
	if err != nil {
		return nil, nil
	}
	return uniqueNonEmpty(strings.Split(showOut, "\n")), nil
}

func parseStatusPaths(out string) []string {
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 3 {
			line = strings.TrimSpace(line[3:])
		}
		if idx := strings.LastIndex(line, " -> "); idx >= 0 {
			line = line[idx+4:]
		}
		if line != "" {
			files = append(files, line)
		}
	}
	return uniqueNonEmpty(files)
}

func buildPRURL(cfg *config.SymphonyConfig, summary workspaceSummary) string {
	branch := strings.TrimSpace(summary.Branch)
	if branch == "" {
		return ""
	}

	if tpl := cfg.TrackerPRURLTemplate(); tpl != "" {
		owner, repo := parseGitHubRemote(summary.RemoteURL)
		repoPath := strings.TrimPrefix(strings.TrimSuffix(summary.RemoteURL, ".git"), "https://github.com/")
		repoPath = strings.TrimPrefix(repoPath, "git@github.com:")
		replacer := strings.NewReplacer(
			"{branch}", url.PathEscape(branch),
			"{branch_raw}", branch,
			"{commit}", summary.LastCommitHash,
			"{owner}", owner,
			"{repo}", repo,
			"{repo_path}", repoPath,
			"{remote_url}", summary.RemoteURL,
		)
		return replacer.Replace(tpl)
	}

	owner, repo := parseGitHubRemote(summary.RemoteURL)
	if owner == "" || repo == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s/pull/new/%s", owner, repo, url.PathEscape(branch))
}

func parseGitHubRemote(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSuffix(raw, ".git")
	switch {
	case strings.HasPrefix(raw, "git@github.com:"):
		path := strings.TrimPrefix(raw, "git@github.com:")
		parts := strings.Split(path, "/")
		if len(parts) == 2 {
			return parts[0], parts[1]
		}
	case strings.HasPrefix(raw, "https://github.com/"):
		path := strings.TrimPrefix(raw, "https://github.com/")
		parts := strings.Split(path, "/")
		if len(parts) == 2 {
			return parts[0], parts[1]
		}
	}
	return "", ""
}

func lastStatusLine(attempt *RunAttempt) string {
	status := string(attempt.GetStatus())
	session := attempt.SessionSnapshot()
	if session.TurnCount > 0 {
		return fmt.Sprintf("%s (turn %d)", status, session.TurnCount)
	}
	return status
}

func formatLastCommit(hash, subject string) string {
	hash = strings.TrimSpace(hash)
	subject = escapeCode(strings.TrimSpace(subject))
	shortHash := hash
	if len(shortHash) > 7 {
		shortHash = shortHash[:7]
	}
	switch {
	case subject != "" && shortHash != "":
		return fmt.Sprintf("%s (%s)", subject, shortHash)
	case subject != "":
		return subject
	default:
		return shortHash
	}
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d < time.Second {
		d = time.Second
	}

	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	minutes := int(d / time.Minute)
	d -= time.Duration(minutes) * time.Minute
	seconds := int(d / time.Second)

	parts := make([]string, 0, 3)
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 || hours > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 || len(parts) == 0 || minutes > 0 || hours > 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}
	return strings.Join(parts, " ")
}

func formatInt(v int64) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	s := strconv.FormatInt(v, 10)
	if len(s) <= 3 {
		return sign + s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return sign + strings.Join(parts, ",")
}

func joinOrDefault(items []string, fallback string) string {
	if len(items) == 0 {
		return fallback
	}
	return strings.Join(items, ", ")
}

func uniqueNonEmpty(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func firstLine(lines []string, idx int) string {
	if idx < 0 || idx >= len(lines) {
		return ""
	}
	return lines[idx]
}

func escapeCode(s string) string {
	return strings.ReplaceAll(s, "`", "'")
}
