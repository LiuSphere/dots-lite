package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
)

type followup struct {
	SubmissionAttempts int             `json:"submission_attempts"`
	Wait               *waitCondition  `json:"wait,omitempty"`
	ID                 string          `json:"id"`
	TaskID             string          `json:"task_id"`
	EventID            string          `json:"event_id"`
	EventType          string          `json:"event_type"`
	TurnID             string          `json:"turn_id,omitempty"`
	Reason             string          `json:"reason,omitempty"`
	ParentID           string          `json:"parent_id,omitempty"`
	SupersededBy       string          `json:"superseded_by,omitempty"`
	TaskVersion        int64           `json:"task_version"`
	SessionID          string          `json:"session_id"`
	IdempotencyKey     string          `json:"idempotency_key"`
	Prompt             string          `json:"prompt"`
	Request            json.RawMessage `json:"request"`
	RunID              string          `json:"run_id,omitempty"`
	Status             string          `json:"status"`
	NativeStatus       string          `json:"native_status,omitempty"`
	Attempts           int             `json:"attempts"`
	CreatedAt          time.Time       `json:"created_at"`
	NextCheckAt        time.Time       `json:"next_check_at"`
	OfferedAt          time.Time       `json:"offered_at"`
	Evidence           json.RawMessage `json:"evidence,omitempty"`
	NativeEvidence     json.RawMessage `json:"native_evidence,omitempty"`
	Error              string          `json:"error,omitempty"`
	Obligation         string          `json:"obligation,omitempty"`
	FaultID            string          `json:"fault_id,omitempty"`
}
type apiLink struct {
	FollowupID  string `json:"followup_id"`
	TaskID      string `json:"task_id"`
	TaskVersion int64  `json:"task_version"`
}

func (h *Hub) createFollowup(tx *store.Tx, b Binding, task taskRecord, event protocol.Event) error {
	if event.Type == "error" {
		var retry struct {
			WillRetry bool `json:"willRetry"`
		}
		if json.Unmarshal(event.Data, &retry) == nil && retry.WillRetry {
			return nil
		}
	}
	var nativeGoal struct {
		GoalStatus string `json:"goal_status"`
		Goal       *struct {
			Status string `json:"status"`
		} `json:"goal"`
	}
	if err := json.Unmarshal(event.Data, &nativeGoal); err != nil {
		return err
	}
	if nativeGoal.Goal != nil {
		nativeGoal.GoalStatus = nativeGoal.Goal.Status
	}
	if (event.Type == "task/quiescent" || event.Type == "task/reconcile") && (nativeGoal.GoalStatus == "usageLimited" || nativeGoal.GoalStatus == "budgetLimited" || nativeGoal.GoalStatus == "paused") {
		return queueAttention(tx, b, task, "native-limit|"+task.TurnID+"|"+nativeGoal.GoalStatus, fmt.Sprintf("任务「%s」的原生 Goal 当前为 %s，后台不会自动解除暂停或重设资源限制。请确认后继续。", task.Name, nativeGoal.GoalStatus))
	}
	needed := event.Type == "error" || event.Type == "verification/failed" || event.Type == "artifact/failed" || event.Type == "turn/failed" || event.Type == "task/native_recovery_failed" || event.Type == "task/reconcile" || event.Type == "item/commandExecution/requestApproval" || event.Type == "item/fileChange/requestApproval" || event.Type == "item/permissions/requestApproval" || event.Type == "tool/requestUserInput" || event.Type == "mcpServer/elicitation/request"
	if event.Type == "task/quiescent" {
		if task.Verification == "passed" {
			return nil
		}
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
	if task.Paused || task.Waiting == "user_decision" || task.Waiting == "recovery_budget_exhausted" {
		return nil
	}
	var trigger struct {
		Reason    string `json:"reason"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(event.Data, &trigger); err != nil {
		return err
	}
	if event.Type == "task/quiescent" {
		trigger.Reason = "quiescent"
	}
	if trigger.Reason == "quiescent" && task.Verification == "passed" {
		return nil
	}
	// Approvals already have a deterministic user outbox. Network outages are
	// observed by reconciliation and transport, without creating model runs.
	switch event.Type {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
		return nil
	}
	if trigger.Reason == "state_unavailable" || trigger.Reason == "goal_unavailable" || trigger.Reason == "no_progress" {
		return nil
	}
	purpose := event.Type
	if trigger.Reason == "quiescent" {
		purpose = "verification"
		if task.Verification == "failed" {
			purpose = "verification/failed"
			trigger.Reason = "verification_failed"
		}
	}
	obligation := fmt.Sprintf("%s|%d|%s|%s|%s|%s|%s|%s", task.ID, task.Version, task.TurnID, purpose, trigger.Reason, trigger.RequestID, task.Verification, task.Delivery)
	var existing followup
	if err := tx.Get("obligation", owner(b.scope()), obligation, &existing); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	session, err := randomID("dots_lite_api_")
	if err != nil {
		return err
	}
	key, err := randomID("dots_lite_event_")
	if err != nil {
		return err
	}
	id := "event_" + strconv.FormatInt(event.Sequence, 10)
	evidence := string(event.Data)
	if len(evidence) > 16384 {
		evidence = evidence[:16384] + " [truncated; inspect task history for complete evidence]"
	}
	prompt := fmt.Sprintf("Follow up on authorized task %s version %d. Objective: %s\nEvent: %s\nEvidence: %s\nFirst call dots_lite_query(query=session,task_id=%s) to read actual native runtime and dots_lite_query(query=goal,task_id=%s) to read the native Goal. Then inspect existing operations and necessary files; a business snapshot alone is not verification. Do not replay unknown writes, resume a paused task, compete with an active native Goal, approve user decisions, or expand scope. Diagnose and fix clear in-scope failures, verify actual results, and acquire authorized artifacts. Record the actions and verification using dots_lite_record(kind=followup,status=handled,evidence={actions:[{observation_id:...}],verification:{observation_id:...}}. Use _observation_id from actual session/goal/history query results, or accepted operation_id receipts; statements alone are rejected); delivery and goal validation remain separate. If action is unsafe or requires user input, record the facts and waiting reason.", task.ID, task.Version, task.Objective, event.Type, evidence, task.ID, task.ID)
	body, err := json.Marshal(map[string]string{"input": prompt, "session_id": session})
	if err != nil {
		return err
	}
	f := followup{ID: id, TaskID: task.ID, EventID: strconv.FormatInt(event.Sequence, 10), EventType: event.Type, TurnID: task.TurnID, Reason: trigger.Reason, TaskVersion: task.Version, SessionID: session, IdempotencyKey: key, Prompt: prompt, Request: body, Status: "pending", CreatedAt: h.now().UTC()}
	f.Obligation = obligation
	if purpose != "verification" && !strings.HasPrefix(trigger.Reason, "wait_ready:") {
		kind := purpose
		if event.Type == "task/native_recovery_failed" {
			kind = "native"
		}
		var err error
		f.FaultID, err = faultForEvent(tx, b, fmt.Sprintf("%s|%d|%s|%s|%s", task.ID, task.Version, kind, trigger.Reason, trigger.RequestID))
		if err != nil {
			return err
		}
	}
	if err := tx.Put("obligation", owner(b.scope()), obligation, f); err != nil {
		return err
	}
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
	if _, phase := h.maintenanceState(b); phase != "running" {
		return json.RawMessage(`null`), nil
	}
	h.mu.Lock()
	online := h.sessions[b.ExecutorID] != nil
	h.mu.Unlock()
	if !online {
		return json.RawMessage(`null`), nil
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
		now := h.now().UTC()
		for _, f := range items {
			if f.Status == "waiting" && terminalRun(f.NativeStatus) {
				continue
			}
			if (f.Status == "handled" && terminalRun(f.NativeStatus)) || f.Status == "blocked" || f.Status == "superseded" {
				continue
			}
			task, err := loadTask(tx, b, f.TaskID)
			if err != nil {
				return err
			}
			if task.Paused || task.Version != f.TaskVersion || task.Waiting == "user_decision" || task.Waiting == "recovery_budget_exhausted" || task.Waiting == "submission_unknown" {
				continue
			}
			if f.OfferedAt.IsZero() && f.Reason == "quiescent" && f.TurnID != task.TurnID {
				f.Status = "superseded"
				if err := tx.Put("followup", owner(b.scope()), f.ID, f); err != nil {
					return err
				}
				continue
			}
			if f.OfferedAt.IsZero() && f.Reason == "quiescent" && (task.Execution == "running" || task.Execution == "starting" || task.GoalActive) {
				continue
			}
			busy := false
			for _, other := range items {
				if other.ID != f.ID && other.TaskID == f.TaskID && other.TaskVersion == f.TaskVersion && !other.OfferedAt.IsZero() && other.Status != "superseded" && other.Status != "blocked" && (!terminalRun(other.NativeStatus) || other.Status == "needs_verification") {
					busy = true
					break
				}
			}
			if busy {
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
			if f.FaultID != "" && f.OfferedAt.IsZero() {
				var fault faultBudget
				err := tx.Get("fault", owner(b.scope()), f.FaultID, &fault)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				if !fault.ClosedAt.IsZero() {
					f.Status = "superseded"
					if err := tx.Put("followup", owner(b.scope()), f.ID, f); err != nil {
						return err
					}
					continue
				}
			}
			// Only this unresolved submission/fault has a limit, never the age
			// or cumulative successful stages of the business task.
			exhausted, faultErr := h.faultExhausted(tx, b, f, now)
			if faultErr != nil {
				return faultErr
			}
			if exhausted || (f.RunID == "" && f.Attempts >= 24) {
				f.Status = "blocked"
				f.Error = "this fault or unresolved submission reached its diagnostic limit"
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
		if f.Status == "superseded" || (terminalRun(f.NativeStatus) && !terminalRun(report.Status)) {
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
		if f.RunID == "" && f.FaultID != "" {
			var fault faultBudget
			err := tx.Get("fault", owner(b.scope()), f.FaultID, &fault)
			if errors.Is(err, store.ErrNotFound) {
				fault.StartedAt = h.now().UTC()
			} else if err != nil {
				return err
			}
			fault.Admitted++
			if err := tx.Put("fault", owner(b.scope()), f.FaultID, fault); err != nil {
				return err
			}
		}

		if f.RunID == "" {
			task, err := loadTask(tx, b, f.TaskID)
			if err != nil {
				return err
			}
			task.RunsAccepted++
			if err := tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
				return err
			}
		}
		f.RunID = report.RunID

		if !terminalRun(f.NativeStatus) && terminalRun(report.Status) {
			task, err := loadTask(tx, b, f.TaskID)
			if err != nil {
				return err
			}
			task.RunsCompleted++
			if err := tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
				return err
			}
		}
		f.NativeStatus = report.Status
		if f.Status != "handled" && f.Status != "blocked" && f.Status != "waiting" {
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
		if f.Status == "superseded" || (terminalRun(f.NativeStatus) && !terminalRun(report.Status)) {
			return nil
		}

		if !terminalRun(f.NativeStatus) && terminalRun(report.Status) {
			task, err := loadTask(tx, b, f.TaskID)
			if err != nil {
				return err
			}
			task.RunsCompleted++
			if err := tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
				return err
			}
		}
		f.NativeStatus = report.Status
		f.Error = report.Error
		f.NativeEvidence = report.Evidence
		if f.Status != "handled" && f.Status != "blocked" && f.Status != "waiting" {
			switch report.Status {
			case "completed", "failed", "cancelled":
				f.Status = "needs_verification"
			default:
				f.Status = "processing"
			}
		}
		f.NextCheckAt = h.now().UTC().Add(15 * time.Second)
		if f.Status == "needs_verification" && terminalRun(f.NativeStatus) {
			task, err := loadTask(tx, b, f.TaskID)
			if err != nil {
				return err
			}
			if _, err = h.retryFollowup(tx, b, &task, &f, h.now().UTC()); err != nil {
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
		if inv.Source.Kind == "api" && req.Tool != "dots_lite_query" {
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
	if req.Tool == "dots_lite_prepare" && (args.Action == "task.create" || args.TaskID == "") {
		return failure("binding_conflict", "API followup must use its registered task")
	}
	if inv.Source.Kind == "api" && req.Tool == "dots_lite_prepare" && args.Action == "goal.set" {
		var request struct {
			Goal struct {
				Status string `json:"status"`
			} `json:"goal"`
		}
		if json.Unmarshal(req.Arguments, &request) != nil || request.Goal.Status != "paused" {
			return failure("resource_limit", "background followups cannot reset native Goal objectives or budgets")
		}
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
		if req.Tool != "dots_lite_query" && (task.Version != inv.TaskVersion || (inv.Source.Kind == "api" && task.Paused)) {
			return failure("stale_version", "API followup task changed or paused")
		}
		if inv.Source.Kind == "api" && req.Tool != "dots_lite_query" {
			return checkAPIAttempt(tx, b, inv.Source)
		}
		return nil
	})
}
