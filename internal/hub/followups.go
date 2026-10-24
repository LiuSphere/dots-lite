package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
)

type followup struct {
	ID             string          `json:"id"`
	TaskID         string          `json:"task_id"`
	EventID        string          `json:"event_id"`
	EventType      string          `json:"event_type"`
	TurnID         string          `json:"turn_id,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	ParentID       string          `json:"parent_id,omitempty"`
	SupersededBy   string          `json:"superseded_by,omitempty"`
	TaskVersion    int64           `json:"task_version"`
	SessionID      string          `json:"session_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Prompt         string          `json:"prompt"`
	Request        json.RawMessage `json:"request"`
	RunID          string          `json:"run_id,omitempty"`
	Status         string          `json:"status"`
	NativeStatus   string          `json:"native_status,omitempty"`
	Attempts       int             `json:"attempts"`
	CreatedAt      time.Time       `json:"created_at"`
	NextCheckAt    time.Time       `json:"next_check_at"`
	OfferedAt      time.Time       `json:"offered_at"`
	Evidence       json.RawMessage `json:"evidence,omitempty"`
	NativeEvidence json.RawMessage `json:"native_evidence,omitempty"`
	Error          string          `json:"error,omitempty"`
}
type apiLink struct {
	FollowupID  string `json:"followup_id"`
	TaskID      string `json:"task_id"`
	TaskVersion int64  `json:"task_version"`
}

func (h *Hub) createFollowup(tx *store.Tx, b Binding, task taskRecord, event protocol.Event) error {
	needed := event.Type == "error" || event.Type == "turn/failed" || event.Type == "task/native_exited" || event.Type == "task/reconcile" || event.Type == "item/commandExecution/requestApproval" || event.Type == "item/fileChange/requestApproval" || event.Type == "item/permissions/requestApproval" || event.Type == "tool/requestUserInput" || event.Type == "mcpServer/elicitation/request"
	if event.Type == "task/quiescent" {
		var quiet struct {
			GoalStatus   string `json:"goal_status"`
			NativeStatus string `json:"native_status"`
		}
		if err := json.Unmarshal(event.Data, &quiet); err != nil {
			return err
		}
		knownQuiet := false
		switch quiet.GoalStatus {
		case "none", "paused", "blocked", "usageLimited", "budgetLimited", "complete":
			knownQuiet = true
		}
		needed = knownQuiet && quiet.NativeStatus == "idle"
	}
	if !needed {
		return nil
	}
	if task.Paused && task.Execution != "interrupted" {
		return nil
	}
	var trigger struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(event.Data, &trigger); err != nil {
		return err
	}
	if event.Type == "task/quiescent" {
		trigger.Reason = "quiescent"
	}
	session, err := randomID("duet_api_")
	if err != nil {
		return err
	}
	key, err := randomID("duet_event_")
	if err != nil {
		return err
	}
	id := "event_" + strconv.FormatInt(event.Sequence, 10)
	evidence := string(event.Data)
	if len(evidence) > 16384 {
		evidence = evidence[:16384] + " [truncated; inspect task history for complete evidence]"
	}
	prompt := fmt.Sprintf("Follow up on authorized task %s version %d. Objective: %s\nEvent: %s\nEvidence: %s\nFirst call duet_query(query=session,task_id=%s) to read actual native runtime and duet_query(query=goal,task_id=%s) to read the native Goal. Then inspect existing operations and necessary files; a business snapshot alone is not verification. Do not replay unknown writes, resume a paused task, compete with an active native Goal, approve user decisions, or expand scope. Diagnose and fix clear in-scope failures, verify actual results, and acquire authorized artifacts. Record the actions and verification using duet_record(kind=followup,status=handled,evidence={actions:...,verification:...}); delivery and goal validation remain separate. If action is unsafe or requires user input, record the facts and waiting reason.", task.ID, task.Version, task.Objective, event.Type, evidence, task.ID, task.ID)
	body, err := json.Marshal(map[string]string{"input": prompt, "session_id": session})
	if err != nil {
		return err
	}
	f := followup{ID: id, TaskID: task.ID, EventID: strconv.FormatInt(event.Sequence, 10), EventType: event.Type, TurnID: task.TurnID, Reason: trigger.Reason, TaskVersion: task.Version, SessionID: session, IdempotencyKey: key, Prompt: prompt, Request: body, Status: "pending", CreatedAt: time.Now().UTC()}
	if err = tx.Put("followup", owner(b.scope()), id, f); err != nil {
		return err
	}
	return tx.Put("api_session", owner(b.scope()), session, apiLink{FollowupID: id, TaskID: task.ID, TaskVersion: task.Version})
}
func (h *Hub) NextFollowup(ctx context.Context, uid int) (json.RawMessage, error) {
	b, err := h.binding(uid)
	if err != nil {
		return nil, err
	}
	var next *followup
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		rows, err := tx.List("followup", owner(b.scope()))
		if err != nil {
			return err
		}
		items := []followup{}
		for _, raw := range rows {
			var f followup
			if err = json.Unmarshal(raw, &f); err != nil {
				return err
			}
			items = append(items, f)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
		now := time.Now().UTC()
		for _, f := range items {
			if f.Status == "handled" || f.Status == "blocked" || f.Status == "superseded" {
				continue
			}
			task, err := loadTask(tx, b, f.TaskID)
			if err != nil {
				return err
			}
			if task.Paused || task.Version != f.TaskVersion || task.Waiting == "user_decision" || task.Waiting == "recovery_budget_exhausted" {
				continue
			}
			if f.Status == "needs_verification" && terminalRun(f.NativeStatus) {
				retry, err := h.retryFollowup(tx, b, &task, &f, now)
				if err != nil {
					return err
				}
				if err = tx.Put("followup", owner(b.scope()), f.ID, f); err != nil {
					return err
				}
				if retry == nil {
					continue
				}
				f = *retry
			}
			if now.Before(f.NextCheckAt) {
				continue
			}
			if (!task.RecoveryStartedAt.IsZero() && now.Sub(task.RecoveryStartedAt) > time.Hour) || (f.RunID == "" && task.RecoveryAttempts >= 24) || (f.OfferedAt.IsZero() && task.RecoveryUsed >= h.cfg.FollowupBudget) {
				f.Status = "blocked"
				f.Error = "cumulative recovery budget exhausted"
				task.Waiting = "recovery_budget_exhausted"
				if err = queueRecoveryBudgetNotice(tx, b, task, f); err != nil {
					return err
				}
				if err = tx.Put("followup", owner(b.scope()), f.ID, f); err != nil {
					return err
				}
				if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
					return err
				}
				continue
			}
			if task.RecoveryStartedAt.IsZero() {
				task.RecoveryStartedAt = now
			}
			if f.OfferedAt.IsZero() {
				task.RecoveryUsed++
				f.OfferedAt = now
			}
			if f.RunID == "" {
				task.RecoveryAttempts++
			}
			f.Attempts++
			f.NextCheckAt = now.Add(15 * time.Second)
			if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
				return err
			}
			if err = tx.Put("followup", owner(b.scope()), f.ID, f); err != nil {
				return err
			}
			next = &f
			break
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(next)
}

type runReport struct {
	RunID     string          `json:"run_id"`
	SessionID string          `json:"session_id,omitempty"`
	Status    string          `json:"status"`
	Evidence  json.RawMessage `json:"evidence,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func (h *Hub) SaveFollowupRun(ctx context.Context, uid int, id string, body json.RawMessage) (json.RawMessage, error) {
	b, err := h.binding(uid)
	if err != nil {
		return nil, err
	}
	var report runReport
	if err = decodeArgs(body, &report); err != nil {
		return nil, err
	}
	if report.RunID == "" || report.SessionID == "" {
		return nil, failure("invalid_arguments", "native run and session references are required")
	}
	var f followup
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.Get("followup", owner(b.scope()), id, &f); err != nil {
			return err
		}
		if f.SessionID != report.SessionID || (f.RunID != "" && f.RunID != report.RunID) {
			return failure("binding_conflict", "native run session does not match pre-registration")
		}
		if f.Status == "superseded" {
			return nil
		}
		var old apiLink
		err := tx.Get("native_run", owner(b.scope()), report.RunID, &old)
		if err == nil && old.FollowupID != f.ID {
			return failure("binding_conflict", "native run already belongs to another followup")
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err = tx.Put("native_run", owner(b.scope()), report.RunID, apiLink{FollowupID: f.ID, TaskID: f.TaskID, TaskVersion: f.TaskVersion}); err != nil {
			return err
		}
		f.RunID = report.RunID
		f.NativeStatus = report.Status
		if f.Status != "handled" && f.Status != "blocked" {
			f.Status = "processing"
		}
		return tx.Put("followup", owner(b.scope()), f.ID, f)
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(f)
}
func (h *Hub) ReportFollowup(ctx context.Context, uid int, id string, body json.RawMessage) (json.RawMessage, error) {
	b, err := h.binding(uid)
	if err != nil {
		return nil, err
	}
	var report runReport
	if err = decodeArgs(body, &report); err != nil {
		return nil, err
	}
	var f followup
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.Get("followup", owner(b.scope()), id, &f); err != nil {
			return err
		}
		if f.RunID == "" || f.RunID != report.RunID {
			return failure("binding_conflict", "followup report must reference its known native run")
		}
		if f.Status == "superseded" {
			return nil
		}
		f.NativeStatus = report.Status
		f.Error = report.Error
		f.NativeEvidence = report.Evidence
		if f.Status != "handled" && f.Status != "blocked" {
			switch report.Status {
			case "completed", "failed", "cancelled":
				f.Status = "needs_verification"
			default:
				f.Status = "processing"
			}
		}
		f.NextCheckAt = time.Now().UTC().Add(15 * time.Second)
		if f.Status == "needs_verification" && terminalRun(f.NativeStatus) {
			task, err := loadTask(tx, b, f.TaskID)
			if err != nil {
				return err
			}
			if _, err = h.retryFollowup(tx, b, &task, &f, time.Now().UTC()); err != nil {
				return err
			}
		}
		return tx.Put("followup", owner(b.scope()), f.ID, f)
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(f)
}
func (h *Hub) checkBoundTask(ctx context.Context, b Binding, inv *invocation, req protocol.ToolRequest) error {
	if inv.TaskID == "" {
		if inv.Source.Kind == "api" && req.Tool != "duet_query" {
			return failure("unbound_source", "unbound API context is diagnostic only")
		}
		return nil
	}
	var args struct {
		TaskID      string `json:"task_id"`
		OperationID string `json:"operation_id"`
		Action      string `json:"action"`
	}
	if err := json.Unmarshal(req.Arguments, &args); err != nil {
		return err
	}
	if args.TaskID != "" && args.TaskID != inv.TaskID {
		return failure("binding_conflict", "API followup cannot change its task")
	}
	if req.Tool == "duet_prepare" && (args.Action == "task.create" || args.TaskID == "") {
		return failure("binding_conflict", "API followup must use its registered task")
	}
	return h.db.Update(ctx, func(tx *store.Tx) error {
		task, err := loadTask(tx, b, inv.TaskID)
		if err != nil {
			return err
		}
		if args.OperationID != "" {
			o, err := loadOperation(tx, b, args.OperationID)
			if err != nil {
				return err
			}
			if o.TaskID != inv.TaskID {
				return failure("binding_conflict", "API followup cannot submit another task operation")
			}
		}
		if req.Tool != "duet_query" && (task.Version != inv.TaskVersion || (inv.Source.Kind == "api" && task.Paused)) {
			return failure("stale_version", "API followup task changed or paused")
		}
		if inv.Source.Kind == "api" && req.Tool != "duet_query" {
			return checkAPIAttempt(tx, b, inv.Source)
		}
		return nil
	})
}
