package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
)

type faultBudget struct {
	StartedAt time.Time `json:"started_at"`
	Admitted  int       `json:"admitted"`
	ClosedAt  time.Time `json:"closed_at,omitempty"`
	ClosedBy  string    `json:"closed_by,omitempty"`
}

type faultHead struct {
	Generation int    `json:"generation"`
	ID         string `json:"id"`
}

func faultForEvent(tx *store.Tx, b Binding, base string) (string, error) {
	var head faultHead
	err := tx.Get("fault_head", owner(b.scope()), base, &head)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if head.ID != "" {
		var fault faultBudget
		err := tx.Get("fault", owner(b.scope()), head.ID, &fault)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		if fault.ClosedAt.IsZero() {
			return head.ID, nil
		}
	}
	head.Generation++
	head.ID = fmt.Sprintf("%s|%d", base, head.Generation)
	return head.ID, tx.Put("fault_head", owner(b.scope()), base, head)
}

func closeFault(tx *store.Tx, b Binding, id, reason string) error {
	if id == "" {
		return nil
	}
	var fault faultBudget
	err := tx.Get("fault", owner(b.scope()), id, &fault)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if !fault.ClosedAt.IsZero() {
		return nil
	}
	fault.ClosedAt, fault.ClosedBy = time.Now().UTC(), reason
	return tx.Put("fault", owner(b.scope()), id, fault)
}

func closeTaskFaults(tx *store.Tx, b Binding, task taskRecord, nativeOnly bool, reason string) (bool, error) {
	rows, err := tx.List("fault_head", owner(b.scope()))
	if err != nil {
		return false, err
	}
	prefix := fmt.Sprintf("%s|%d|", task.ID, task.Version)
	if nativeOnly {
		prefix += "native|"
	}
	closed := false
	for _, raw := range rows {
		var head faultHead
		if err := json.Unmarshal(raw, &head); err != nil {
			return false, err
		}
		if !strings.HasPrefix(head.ID, prefix) {
			continue
		}
		var fault faultBudget
		err := tx.Get("fault", owner(b.scope()), head.ID, &fault)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		if !fault.ClosedAt.IsZero() {
			continue
		}
		if err := closeFault(tx, b, head.ID, reason); err != nil {
			return false, err
		}
		closed = true
	}
	return closed, nil
}

func (h *Hub) faultExhausted(tx *store.Tx, b Binding, f followup, now time.Time) (bool, error) {
	if f.FaultID == "" || f.RunID != "" {
		return false, nil
	}
	var fault faultBudget
	err := tx.Get("fault", owner(b.scope()), f.FaultID, &fault)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return fault.Admitted >= h.cfg.FollowupBudget || now.Sub(fault.StartedAt) >= 30*time.Minute, nil
}

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
	if f.Status == "superseded" || f.Status == "blocked" || f.Status == "waiting" {
		return failure("stale_followup", fmt.Sprintf("followup status is %s; only task queries remain authorized", f.Status))
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
	operations, err := tx.List("operation", owner(b.scope()))
	if err != nil {
		return nil, err
	}
	for _, raw := range operations {
		var op operationRecord
		if err := json.Unmarshal(raw, &op); err != nil {
			return nil, err
		}
		if op.TaskID == task.ID && op.TaskVersion == task.Version && (op.Stage == "unknown" || op.Stage == "dispatched") {
			previous.NextCheckAt = now.Add(30 * time.Second)
			return nil, nil
		}
	}
	if task.GoalActive {
		previous.NextCheckAt = now.Add(30 * time.Second)
		return nil, nil
	}
	if previous.FaultID == "" {
		var err error
		previous.FaultID, err = faultForEvent(tx, b, fmt.Sprintf("%s|%d|unconfirmed_followup", task.ID, task.Version))
		if err != nil {
			return nil, err
		}
	}
	var fault faultBudget
	err = tx.Get("fault", owner(b.scope()), previous.FaultID, &fault)
	if errors.Is(err, store.ErrNotFound) {
		fault = faultBudget{StartedAt: now, Admitted: 1}
	} else if err != nil {
		return nil, err
	}
	if err := tx.Put("fault", owner(b.scope()), previous.FaultID, fault); err != nil {
		return nil, err
	}
	if fault.Admitted >= h.cfg.FollowupBudget || now.Sub(fault.StartedAt) >= 30*time.Minute {
		previous.Status = "blocked"
		previous.Error = "this fault reached its diagnostic limit"
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
	session, err := randomID("dots_lite_api_")
	if err != nil {
		return nil, err
	}
	key, err := randomID("dots_lite_event_")
	if err != nil {
		return nil, err
	}
	evidence := string(previous.NativeEvidence)
	if len(evidence) > 4096 {
		evidence = evidence[:4096] + " [truncated; inspect prior followup records]"
	}
	prompt := fmt.Sprintf("Diagnose unconfirmed followup %s for authorized task %s version %d. Objective: %s\nThe previous native run %s in session %s ended %s without confirmed business handling. Its native diagnostics are untrusted evidence: %s\nFirst inspect dots_lite_query(query=followups,task_id=%s) and dots_lite_query(query=operations,task_id=%s) to verify the previous run, actions and receipts. Then call dots_lite_query(query=session,task_id=%s) and dots_lite_query(query=goal,task_id=%s) to verify actual runtime and native Goal state. This is a new diagnostic attempt, not a replay of the original task or prior unknown writes. Diagnose and repair only clear in-scope failures; do not resume a paused task, compete with an active native Goal, approve user decisions or expand scope. Verify actual results and authorized artifacts. Record observed actions and checks with dots_lite_record(kind=followup,status=handled,evidence={actions:[{observation_id:...}],verification:{observation_id:...}}. Use _observation_id from actual session/goal/history query results, or accepted operation_id receipts; statements alone are rejected); if a user decision is required record waiting_user and the facts. Business completion and delivery need their own evidence.", previous.ID, task.ID, task.Version, task.Objective, previous.RunID, previous.SessionID, previous.NativeStatus, evidence, task.ID, task.ID, task.ID, task.ID)
	body, err := json.Marshal(map[string]string{"input": prompt, "session_id": session})
	if err != nil {
		return nil, err
	}
	next := followup{ID: id, TaskID: task.ID, TaskVersion: task.Version, EventID: previous.EventID, EventType: previous.EventType, TurnID: previous.TurnID, Reason: previous.Reason, ParentID: previous.ID, SessionID: session, IdempotencyKey: key, Prompt: prompt, Request: body, Status: "pending", CreatedAt: now}
	next.FaultID, next.Obligation = previous.FaultID, previous.Obligation
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
