package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
)

// Reconcile checks persisted outstanding work without replaying mutations.
// The service calls this every 30 seconds; each task persists its next check.
func (h *Hub) Reconcile(ctx context.Context) error {
	for _, b := range h.cfg.Bindings {
		var operations []operationRecord
		var tasks []taskRecord
		err := h.db.Update(ctx, func(tx *store.Tx) error {
			if err := expireDeliveryAttempts(tx, b); err != nil {
				return err
			}
			rows, err := tx.List("operation", owner(b.scope()))
			if err != nil {
				return err
			}
			for _, raw := range rows {
				var op operationRecord
				if err = json.Unmarshal(raw, &op); err != nil {
					return err
				}
				if op.Stage == "unknown" || (op.Stage == "dispatched" && time.Since(op.UpdatedAt) >= 8*time.Second) {
					operations = append(operations, op)
				}
			}
			rows, err = tx.List("task", owner(b.scope()))
			if err != nil {
				return err
			}
			for _, raw := range rows {
				var task taskRecord
				if err = json.Unmarshal(raw, &task); err != nil {
					return err
				}
				if task.Execution == "prepared" || task.Paused || h.now().Before(task.NextReconcileAt) {
					continue
				}
				if task.Execution == "completed" && task.Verification == "passed" && (len(task.Artifacts) == 0 || task.Delivery == "delivered") {
					continue
				}
				task.NextReconcileAt = h.now().UTC().Add(30 * time.Second)
				if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
					return err
				}
				tasks = append(tasks, task)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, op := range operations {
			if err = h.reconcileOperation(ctx, b, op.ID); err != nil {
				return err
			}
		}
		for _, task := range tasks {
			if err = h.reconcileTask(ctx, b, task); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Hub) readTaskState(ctx context.Context, b Binding, task taskRecord, action string) (json.RawMessage, error) {
	id, err := randomID("read_")
	if err != nil {
		return nil, err
	}
	result, err := h.dispatch(ctx, b, protocol.Operation{ID: id, TaskID: task.ID, Scope: b.scope(), TaskVersion: task.Version, FolderID: task.FolderID, WorkingDirectory: task.WorkingDirectory, ThreadID: task.ThreadID, Action: action})
	if err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, result.Error
	}
	if result.Stage != "accepted" || len(result.Data) == 0 {
		return nil, failure("invalid_result", "reconciliation read did not return a state")
	}
	return result.Data, nil
}

func (h *Hub) reconcileTask(ctx context.Context, b Binding, observed taskRecord) error {
	var native struct {
		Task   protocol.Task `json:"task"`
		Native struct {
			Thread struct {
				Status struct {
					Type string `json:"type"`
				} `json:"status"`
			} `json:"thread"`
		} `json:"native"`
	}
	var goal struct {
		Goal *struct {
			Status string `json:"status"`
		} `json:"goal"`
	}
	reason, diagnostic := "", ""
	data, readErr := h.readTaskState(ctx, b, observed, "task.read")
	if readErr == nil {
		readErr = json.Unmarshal(data, &native)
	}
	if readErr == nil && (native.Task.ID != observed.ID || native.Task.Scope != b.scope() || native.Task.Version != observed.Version || native.Task.Execution == "" || (observed.ThreadID != "" && native.Task.ThreadID != observed.ThreadID)) {
		readErr = failure("binding_conflict", "reconciliation state does not match the registered task")
	}
	nativeValid := readErr == nil
	if readErr != nil {
		reason, diagnostic = "state_unavailable", readErr.Error()
	} else {
		data, readErr = h.readTaskState(ctx, b, observed, "goal.get")
		if readErr == nil {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				readErr = err
			} else if fields["goal"] == nil {
				readErr = failure("invalid_result", "native Goal status is absent")
			}
		}
		if readErr == nil {
			readErr = json.Unmarshal(data, &goal)
		}
		if readErr != nil {
			reason, diagnostic = "goal_unavailable", readErr.Error()
		} else {
			quietGoal := goal.Goal == nil
			if goal.Goal != nil {
				switch goal.Goal.Status {
				case "paused", "blocked", "usageLimited", "budgetLimited", "complete":
					quietGoal = true
				}
			}
			if native.Native.Thread.Status.Type == "idle" && quietGoal {
				reason = "quiescent"
			}
			progress := observed.ProgressAt
			if progress.IsZero() {
				progress = observed.CreatedAt
			}
			if reason == "" && goal.Goal == nil && native.Native.Thread.Status.Type != "active" && h.now().Sub(progress) >= 5*time.Minute {
				reason = "no_progress"
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.db.Update(ctx, func(tx *store.Tx) error {
		task, err := loadTask(tx, b, observed.ID)
		if err != nil {
			return err
		}
		if task.Version != observed.Version || task.Paused || task.TurnID != observed.TurnID || task.Waiting == "user_decision" || task.Waiting == "submission_unknown" || task.Waiting == "migration_review" {
			return nil
		}
		priorWaiting := task.Waiting
		if nativeValid {
			task.Execution, task.Waiting = native.Task.Execution, native.Task.Waiting
			if native.Task.TurnID != "" {
				task.TurnID = native.Task.TurnID
			}
		}
		if priorWaiting == "recovery_budget_exhausted" {
			task.Waiting = priorWaiting
		}
		if nativeValid && readErr == nil {
			task.GoalActive = goal.Goal != nil && goal.Goal.Status == "active"
			if observed.Execution == "unknown" && (native.Native.Thread.Status.Type == "idle" || native.Native.Thread.Status.Type == "active") {
				closed, err := closeTaskFaults(tx, b, task, true, "native runtime and Goal reads confirmed recovery")
				if err != nil {
					return err
				}
				if closed && task.Waiting == "recovery_budget_exhausted" {
					task.Waiting = ""
				}
			}
			goalStatus := "none"
			if goal.Goal != nil {
				goalStatus = goal.Goal.Status
			}
			if err := h.wakeWaiting(tx, b, task, native.Native.Thread.Status.Type, goalStatus, h.now().UTC()); err != nil {
				return err
			}
		}
		if reason != "" {
			fingerprint := fmt.Sprintf("%d|%s|%s|%s|%s", task.Version, task.TurnID, task.Execution, reason, diagnostic)
			if fingerprint != task.ReconcileFingerprint {
				rows, err := tx.List("followup", owner(b.scope()))
				if err != nil {
					return err
				}
				pending := false
				active := false
				for _, raw := range rows {
					var f followup
					if err = json.Unmarshal(raw, &f); err != nil {
						return err
					}
					if f.TaskID == task.ID && f.TaskVersion == task.Version && ((f.Status != "handled" && f.Status != "superseded") || (f.Status == "handled" && reason == "quiescent" && f.TurnID == task.TurnID && (f.EventType == "task/quiescent" || (f.EventType == "task/reconcile" && f.Reason == "quiescent")))) {
						pending = true
						active = f.Status != "handled"
						break
					}
				}
				if !pending {
					var sequence int64
					if err = tx.Get("cursor", owner(b.scope()), "reconcile", &sequence); err != nil && !errors.Is(err, store.ErrNotFound) {
						return err
					}
					sequence--
					payload, err := json.Marshal(map[string]any{"reason": reason, "diagnostic": diagnostic, "task": task.Task, "native_status": native.Native.Thread.Status.Type, "goal": goal.Goal})
					if err != nil {
						return err
					}
					event := protocol.Event{Sequence: sequence, TaskID: task.ID, ThreadID: task.ThreadID, TurnID: task.TurnID, Type: "task/reconcile", Data: payload, CreatedAt: h.now().UTC()}
					if err = h.createFollowup(tx, b, task, event); err != nil {
						return err
					}
					if err = tx.Put("reconcile_event", owner(b.scope()), fmt.Sprint(sequence), event); err != nil {
						return err
					}
					if err = tx.Put("cursor", owner(b.scope()), "reconcile", sequence); err != nil {
						return err
					}
				}
				if !active {
					task.ReconcileFingerprint = fingerprint
				}
			}
		}
		return tx.Put("task", owner(b.scope()), task.ID, task)
	})
}
