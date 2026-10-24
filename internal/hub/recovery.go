package hub

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"dots-duet-lite/internal/protocol"
	"dots-duet-lite/internal/store"
)

func (h *Hub) recoverRecords(ctx context.Context) error {
	for _, b := range h.cfg.Bindings {
		if err := h.db.Update(ctx, func(tx *store.Tx) error {
			deliveries, err := tx.List("delivery", owner(b.scope()))
			if err != nil {
				return err
			}
			for _, raw := range deliveries {
				var item delivery
				if err = json.Unmarshal(raw, &item); err != nil {
					return err
				}
				if item.Status != "dispatching" {
					continue
				}
				item.Status = "unknown"
				item.Error = "hub restarted before native send receipt; automatic replay is disabled"
				if err = tx.Put("delivery", owner(b.scope()), item.ID, item); err != nil {
					return err
				}
				if err = tx.Put("delivery_attempt", owner(b.scope()), item.AttemptID, item); err != nil {
					return err
				}
				if item.Purpose == "result" {
					task, err := loadTask(tx, b, item.TaskID)
					if err != nil {
						return err
					}
					if task.Version == item.TaskVersion {
						task.Delivery = "unknown"
						if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
							return err
						}
					}
				}
			}
			ops, err := tx.List("operation", owner(b.scope()))
			if err != nil {
				return err
			}
			for _, raw := range ops {
				var op operationRecord
				if err = json.Unmarshal(raw, &op); err != nil {
					return err
				}
				if op.Stage == "dispatched" {
					op.Stage = "unknown"
					op.Result = &protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "unknown", Error: &protocol.Error{Code: "unknown", Message: "hub restarted before executor receipt"}}
					if err = tx.Put("operation", owner(b.scope()), op.ID, op); err != nil {
						return err
					}
				}
			}
			tasks, err := tx.List("task", owner(b.scope()))
			if err != nil {
				return err
			}
			for _, raw := range tasks {
				var task taskRecord
				if err = json.Unmarshal(raw, &task); err != nil {
					return err
				}
				if task.Execution == "running" {
					task.Execution = "unknown"
					if task.Paused {
						task.Execution = "pause_requested"
					}
					if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
func (h *Hub) reconcileOperation(ctx context.Context, b Binding, id string) error {
	var op operationRecord
	err := h.db.Update(ctx, func(tx *store.Tx) error { var err error; op, err = loadOperation(tx, b, id); return err })
	if err != nil {
		return err
	}
	if op.Stage != "unknown" && op.Stage != "dispatched" {
		return nil
	}
	h.mu.Lock()
	online := h.sessions[b.ExecutorID] != nil
	h.mu.Unlock()
	if !online {
		return nil
	}
	queryID, err := randomID("read_")
	if err != nil {
		return err
	}
	result, err := h.dispatch(ctx, b, protocol.Operation{ID: queryID, TaskID: op.TaskID, Scope: b.scope(), TaskVersion: op.TaskVersion, ProjectID: op.ProjectID, Action: "operation.read", RequestID: op.ID})
	if err != nil {
		return h.saveReconciliationError(ctx, b, op.ID, &protocol.Error{Code: "reconciliation_failed", Message: err.Error()})
	}
	if result.Error != nil {
		return h.saveReconciliationError(ctx, b, op.ID, result.Error)
	}
	var receipt protocol.Result
	if err = json.Unmarshal(result.Data, &receipt); err != nil {
		return err
	}
	if receipt.OperationID != op.ID || receipt.TaskID != op.TaskID {
		return failure("binding_conflict", "native receipt does not match original operation")
	}
	return h.applyResult(ctx, b, receipt)
}
func (h *Hub) saveReconciliationError(ctx context.Context, b Binding, id string, diagnostic *protocol.Error) error {
	return h.db.Update(ctx, func(tx *store.Tx) error {
		op, err := loadOperation(tx, b, id)
		if err != nil {
			return err
		}
		op.ReconciliationError = diagnostic
		op.UpdatedAt = time.Now().UTC()
		return tx.Put("operation", owner(b.scope()), id, op)
	})
}
func (h *Hub) applySnapshot(ctx context.Context, b Binding, snapshot protocol.Snapshot) error {
	return h.db.Update(ctx, func(tx *store.Tx) error {
		for _, native := range snapshot.Tasks {
			task, err := loadTask(tx, b, native.ID)
			if err != nil {
				var failure *protocol.Error
				if errors.As(err, &failure) && failure.Code == "not_found" {
					continue
				}
				return err
			}
			if native.Scope != b.scope() {
				return failure("scope_mismatch", "snapshot task scope mismatch")
			}
			if native.Version != task.Version {
				continue
			}
			if native.ThreadID != "" {
				task.ThreadID = native.ThreadID
			}
			if native.TurnID != "" {
				task.TurnID = native.TurnID
			}
			if native.Version == task.Version {
				task.Execution = native.Execution
				task.Waiting = native.Waiting
			}
			task.UpdatedAt = time.Now().UTC()
			if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
				return err
			}
		}
		return nil
	})
}
