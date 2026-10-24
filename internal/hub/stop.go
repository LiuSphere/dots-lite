package hub

import (
	"context"
	"encoding/json"
	"time"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
)

func (h *Hub) stop(ctx context.Context, b Binding, source protocol.Source, taskID string) (json.RawMessage, error) {
	id, err := randomID("op_")
	if err != nil {
		return nil, err
	}
	var op operationRecord
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := checkAPIAttempt(tx, b, source); err != nil {
			return err
		}
		task, err := loadTask(tx, b, taskID)
		if err != nil {
			return err
		}
		task.Version++
		task.Paused = true
		task.Execution = "pause_requested"
		task.UpdatedAt = time.Now().UTC()
		op = operationRecord{Operation: protocol.Operation{ID: id, TaskID: task.ID, Scope: b.scope(), TaskVersion: task.Version, FolderID: task.FolderID, WorkingDirectory: task.WorkingDirectory, Action: "task.stop", ThreadID: task.ThreadID, ExpectedTurnID: task.TurnID}, Source: source, Stage: "prepared", CreatedAt: task.UpdatedAt, UpdatedAt: task.UpdatedAt}
		if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
			return err
		}
		return tx.Put("operation", owner(b.scope()), id, op)
	})
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	online := h.sessions[b.ExecutorID] != nil
	h.mu.Unlock()
	if online {
		if _, err = h.submit(ctx, b, id); err != nil {
			return nil, err
		}
	}
	var task taskRecord
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		task, err = loadTask(tx, b, taskID)
		if err != nil {
			return err
		}
		op, err = loadOperation(tx, b, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	confirmed := false
	if op.Action == "task.stop" && op.Stage == "accepted" && op.Result != nil && op.Result.Error == nil && task.Paused && task.Version == op.TaskVersion {
		var native protocol.Task
		if json.Unmarshal(op.Result.Data, &native) == nil && native.ID == task.ID && native.Scope == b.scope() && native.Version == task.Version && native.Paused {
			switch native.Execution {
			case "paused", "interrupted", "completed", "failed", "stopped":
				confirmed = true
			}
		}
	}
	return json.Marshal(map[string]any{"task_id": taskID, "operation_id": id, "stage": op.Stage, "pause_recorded": true, "stop_confirmed": confirmed})
}
func (h *Hub) resumeStops(ctx context.Context, b Binding) error {
	ids := []string{}
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		rows, err := tx.List("operation", owner(b.scope()))
		if err != nil {
			return err
		}
		for _, raw := range rows {
			var op operationRecord
			if err = json.Unmarshal(raw, &op); err != nil {
				return err
			}
			if op.Action == "task.stop" && op.Stage == "prepared" {
				task, err := loadTask(tx, b, op.TaskID)
				if err != nil {
					return err
				}
				if task.Paused && task.Version == op.TaskVersion {
					ids = append(ids, op.ID)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = h.submit(ctx, b, id); err != nil {
			return err
		}
	}
	return nil
}
