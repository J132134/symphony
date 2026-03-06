//go:build darwin

package menubar

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"

	"symphony/internal/status"
	"symphony/internal/version"
)

var spinnerFrames = []string{"◐", "◓", "◑", "◒"}

type app struct {
	client       *Client
	pollInterval time.Duration

	mu        sync.Mutex
	summary   status.Summary
	lastError error
	spinIndex int

	stateItem   *systray.MenuItem
	versionItem *systray.MenuItem
	countItem   *systray.MenuItem
	issuesItem  *systray.MenuItem
	refreshItem *systray.MenuItem
	openItem    *systray.MenuItem
	quitItem    *systray.MenuItem
}

func Run(opts Options) error {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}

	app := &app{
		client:       NewClient(opts.BaseURL),
		pollInterval: opts.PollInterval,
		summary: status.Summary{
			Status:          "network_lost",
			Version:         version.Current().Version,
			GitHash:         version.Current().GitHash,
			RunningIssueIDs: []string{},
		},
	}

	systray.Run(app.onReady, func() {})
	return nil
}

func (a *app) onReady() {
	systray.SetTitle("○")
	systray.SetTooltip("Symphony menubar is starting")

	a.stateItem = systray.AddMenuItem("Status: starting", "")
	a.stateItem.Disable()
	a.versionItem = systray.AddMenuItem("Version: unknown", "")
	a.versionItem.Disable()
	a.countItem = systray.AddMenuItem("Subprocesses: 0", "")
	a.countItem.Disable()
	a.issuesItem = systray.AddMenuItem("Issues: none", "")
	a.issuesItem.Disable()
	systray.AddSeparator()
	a.refreshItem = systray.AddMenuItem("Refresh now", "")
	a.openItem = systray.AddMenuItem("Open dashboard", "")
	a.quitItem = systray.AddMenuItem("Quit", "")

	go a.refreshLoop()
	go a.animateLoop()
	go a.handleMenuClicks()
}

func (a *app) refreshLoop() {
	a.refreshOnce()

	ticker := time.NewTicker(a.pollInterval)
	defer ticker.Stop()

	for range ticker.C {
		a.refreshOnce()
	}
}

func (a *app) refreshOnce() {
	summary, err := a.client.Summary()

	a.mu.Lock()
	if err != nil {
		a.lastError = err
	} else {
		a.lastError = nil
		a.summary = summary
	}
	a.mu.Unlock()

	a.render()
}

func (a *app) animateLoop() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		a.mu.Lock()
		if a.summary.Status == "running" && a.lastError == nil {
			a.spinIndex = (a.spinIndex + 1) % len(spinnerFrames)
		}
		a.mu.Unlock()
		a.render()
	}
}

func (a *app) handleMenuClicks() {
	for {
		select {
		case <-a.refreshItem.ClickedCh:
			_ = a.client.Refresh()
			a.refreshOnce()
		case <-a.openItem.ClickedCh:
			_ = exec.Command("open", a.client.DashboardURL()).Start()
		case <-a.quitItem.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func (a *app) render() {
	a.mu.Lock()
	summary := a.summary
	lastErr := a.lastError
	spinIndex := a.spinIndex
	a.mu.Unlock()

	icon, stateLabel := menuBarStatus(summary.Status, lastErr, spinIndex)
	versionLabel := summary.Version
	if short := version.ShortHash(summary.GitHash); short != "" {
		versionLabel = short
		if summary.Dirty {
			versionLabel += " dirty"
		}
	}
	issues := "none"
	if len(summary.RunningIssueIDs) > 0 {
		issues = strings.Join(summary.RunningIssueIDs, ", ")
	}

	systray.SetTitle(icon)
	systray.SetTooltip(buildTooltip(summary, stateLabel, versionLabel, issues, lastErr))

	a.stateItem.SetTitle("Status: " + stateLabel)
	a.versionItem.SetTitle("Version: " + versionLabel)
	a.countItem.SetTitle(fmt.Sprintf("Subprocesses: %d", summary.SubprocessCount))
	a.issuesItem.SetTitle("Issues: " + issues)
}

func buildTooltip(summary status.Summary, stateLabel, versionLabel, issues string, lastErr error) string {
	tooltipLines := []string{
		fmt.Sprintf("Status: %s", stateLabel),
		fmt.Sprintf("Version: %s", versionLabel),
		fmt.Sprintf("Subprocesses: %d", summary.SubprocessCount),
		fmt.Sprintf("Issues: %s", issues),
	}
	if summary.RetryCount > 0 {
		tooltipLines = append(tooltipLines, fmt.Sprintf("Retries: %d", summary.RetryCount))
		if summary.FailureRetryCount > 0 {
			tooltipLines = append(tooltipLines, fmt.Sprintf("Failure Retries: %d", summary.FailureRetryCount))
		}
		if summary.CapacityWaitCount > 0 {
			tooltipLines = append(tooltipLines, fmt.Sprintf("Capacity Waits: %d", summary.CapacityWaitCount))
		}
	}
	if summary.UpdatedAt != "" {
		tooltipLines = append(tooltipLines, fmt.Sprintf("Updated: %s", summary.UpdatedAt))
	}
	if lastErr != nil {
		tooltipLines = append(tooltipLines, fmt.Sprintf("Error: %s", lastErr.Error()))
	}
	return strings.Join(tooltipLines, "\n")
}

func menuBarStatus(status string, lastErr error, spinIndex int) (icon string, label string) {
	if lastErr != nil {
		return "⏸", "Daemon unreachable"
	}

	switch status {
	case "network_lost":
		return "⏸", "Network lost"
	case "error":
		return "⚠", "Error"
	case "running":
		return spinnerFrames[spinIndex%len(spinnerFrames)], "Running"
	default:
		return "○", "Idle"
	}
}
