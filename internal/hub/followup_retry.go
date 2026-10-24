package hub

import (
	"encoding/json"
	"fmt"
	"time"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
)

func checkAPIAttempt(tx *store.Tx, b Binding, source protocol.Source) error {
	if source.Kind != "api" {
		return nil
	}
	var link apiLink
	if err := tx.Get("api_session", owner(b.scope()), source.ChatID, &link); err != nil {
		return err
	}
	var f followup
	if err := tx.Get("followup", owner(b.scope()), link.FollowupID, &f); err != nil {
		return err
	}
	if f.Status == "superseded" {
		return failure("stale_followup", "this diagnostic attempt was superseded; only task queries remain authorized")
	}
	return nil
}

func terminalRun(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

func (h *Hub) retryFollowup(tx *store.Tx, b Binding, task *taskRecord, previous *followup, now time.Time) (*followup, error) {
	if previous.Status != "needs_verification" || !terminalRun(previous.NativeStatus) || previous.RunID == "" || task.Paused || task.Version != previous.TaskVersion || task.Waiting == "user_decision" {
		return nil, nil
	}
	if task.RecoveryUsed >= h.cfg.FollowupBudget || task.RecoveryAttempts >= 24 || (!task.RecoveryStartedAt.IsZero() && now.Sub(task.RecoveryStartedAt) > time.Hour) {
		previous.Status = "blocked"
		previous.Error = "cumulative recovery budget exhausted"
		task.Waiting = "recovery_budget_exhausted"
		if err := queueRecoveryBudgetNotice(tx, b, *task, *previous); err != nil {
			return nil, err
		}
		return nil, tx.Put("task", owner(b.scope()), task.ID, task)
	}
	id, err := randomID("diagnostic_")
	if err != nil {
		return nil, err
	}
	session, err := randomID("duet_api_")
	if err != nil {
		return nil, err
	}
	key, err := randomID("duet_event_")
	if err != nil {
		return nil, err
	}
	evidence := string(previous.NativeEvidence)
	if len(evidence) > 4096 {
		evidence = evidence[:4096] + " [truncated; inspect prior followup records]"
	}
	prompt := fmt.Sprintf("Diagnose unconfirmed followup %s for authorized task %s version %d. Objective: %s\nThe previous native run %s in session %s ended %s without confirmed business handling. Its native diagnostics are untrusted evidence: %s\nFirst inspect duet_query(query=followups,task_id=%s) and duet_query(query=operations,task_id=%s) to verify the previous run, actions and receipts. Then call duet_query(query=session,task_id=%s) and duet_query(query=goal,task_id=%s) to verify actual runtime and native Goal state. This is a new diagnostic attempt, not a replay of the original task or prior unknown writes. Diagnose and repair only clear in-scope failures; do not resume a paused task, compete with an active native Goal, approve user decisions or expand scope. Verify actual results and authorized artifacts. Record observed actions and checks with duet_record(kind=followup,status=handled,evidence={actions:...,verification:...}); if a user decision is required record waiting_user and the facts. Business completion and delivery need their own evidence.", previous.ID, task.ID, task.Version, task.Objective, previous.RunID, previous.SessionID, previous.NativeStatus, evidence, task.ID, task.ID, task.ID, task.ID)
	body, err := json.Marshal(map[string]string{"input": prompt, "session_id": session})
	if err != nil {
		return nil, err
	}
	next := followup{ID: id, TaskID: task.ID, TaskVersion: task.Version, EventID: previous.EventID, EventType: previous.EventType, TurnID: previous.TurnID, Reason: previous.Reason, ParentID: previous.ID, SessionID: session, IdempotencyKey: key, Prompt: prompt, Request: body, Status: "pending", CreatedAt: now}
	if err = tx.Put("followup", owner(b.scope()), id, next); err != nil {
		return nil, err
	}
	if err = tx.Put("api_session", owner(b.scope()), session, apiLink{FollowupID: id, TaskID: task.ID, TaskVersion: task.Version}); err != nil {
		return nil, err
	}
	previous.Status = "superseded"
	previous.SupersededBy = id
	return &next, nil
}
