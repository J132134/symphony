package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"symphony/internal/config"
	"symphony/internal/types"
)

func (o *Orchestrator) reconcile(ctx context.Context) {
	o.state.mu.Lock()
	empty := len(o.state.Running) == 0
	o.state.mu.Unlock()
	if empty {
		return
	}

	o.mu.Lock()
	cfg := o.cfg
	tr := o.tracker
	o.mu.Unlock()

	if cfg == nil || tr == nil {
		return
	}

	stallMs := cfg.StallTimeoutMs()
	now := time.Now().UTC()

	type runEntry struct {
		id      string
		attempt *RunAttempt
	}
	o.state.mu.Lock()
	entries := make([]runEntry, 0, len(o.state.Running))
	for id, attempt := range o.state.Running {
		entries = append(entries, runEntry{id, attempt})
	}
	o.state.mu.Unlock()

	var stalledIDs []string
	runningIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		runningIDs = append(runningIDs, e.id)
		last := e.attempt.GetLastEventAt()
		if last == nil {
			last = &e.attempt.StartedAt
		}
		if int(now.Sub(*last).Milliseconds()) > stallMs {
			slog.Warn("orchestrator.stall_detected", "project", o.name,
				"issue", e.attempt.Identifier, "elapsed_ms", now.Sub(*last).Milliseconds())
			e.attempt.SetStatus(StatusStalled)
			stalledIDs = append(stalledIDs, e.id)
		}
	}

	for _, id := range stalledIDs {
		o.cancelWorker(id, CancelReasonStall)
	}

	if len(runningIDs) == 0 {
		return
	}

	current, err := tr.FetchIssueStatesByIDs(ctx, runningIDs)
	if err != nil {
		o.state.RecordTrackerFailure(time.Now().UTC(), err)
		slog.Error("orchestrator.reconcile_fetch_failed", "error", err)
		return
	}
	o.state.RecordTrackerSuccess(time.Now().UTC())

	issueMap := make(map[string]*types.Issue, len(current))
	for _, iss := range current {
		issueMap[iss.ID] = iss
	}

	termNorm := cfg.TermNorm()

	for _, id := range runningIDs {
		o.state.mu.Lock()
		attempt, stillRunning := o.state.Running[id]
		o.state.mu.Unlock()
		if !stillRunning {
			continue
		}

		cur, found := issueMap[id]
		if !found {
			slog.Warn("orchestrator.reconcile_missing", "issue_id", id)
			o.cancelWorker(id, CancelReasonReconcile)
			continue
		}

		if termNorm[config.NormalizeState(cur.State)] {
			slog.Info("orchestrator.reconcile_terminal", "issue", cur.Identifier, "state", cur.State)
			if attempt != nil {
				attempt.MarkCleanupOnExit()
			}
			o.cancelWorker(id, CancelReasonTerminal)
			continue
		}

		if isPauseState(cfg, cur.State) {
			slog.Info("orchestrator.reconcile_paused_for_state", "issue", cur.Identifier, "state", cur.State)
			o.cancelWorker(id, CancelReasonReconcile)
		}
	}
}

func (o *Orchestrator) cancelWorker(issueID string, reason WorkerCancelReason) {
	o.mu.Lock()
	handle, ok := o.workersByIssue[issueID]
	o.mu.Unlock()
	if ok {
		handle.attempt.SetCancelReason(reason)
		handle.cancel()
	}
}

func (o *Orchestrator) isStillActive(ctx context.Context, cfg *config.SymphonyConfig, issueID string) (bool, error) {
	o.mu.Lock()
	tr := o.tracker
	o.mu.Unlock()
	if tr == nil {
		return true, nil
	}
	issues, err := tr.FetchIssueStatesByIDs(ctx, []string{issueID})
	if err != nil {
		return true, err
	}
	if len(issues) == 0 {
		return false, nil
	}
	norm := config.NormalizeState(issues[0].State)
	return cfg.ActiveNorm()[norm] && !cfg.PauseNorm()[norm], nil
}
