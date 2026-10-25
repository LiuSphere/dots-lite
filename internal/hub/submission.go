package hub

import (
	"context"
	"dots-lite/internal/store"
	"encoding/json"
)

func (h *Hub) BlockUnknownSubmission(ctx context.Context, uid int, id string, body json.RawMessage) (json.RawMessage, error) {
	b, err := h.binding(uid)
	if err != nil {
		return nil, err
	}
	var report struct {
		Error  string `json:"error"`
		Status string `json:"status,omitempty"`
	}
	if err := decodeArgs(body, &report); err != nil {
		return nil, err
	}
	if report.Error == "" && report.Status != "attempting" {
		return nil, failure("invalid_arguments", "submission error is required")
	}
	var f followup
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.Get("followup", owner(b.scope()), id, &f); err != nil {
			return err
		}
		if f.RunID != "" {
			return failure("binding_conflict", "known native run must be polled, not marked as an unknown submission")
		}
		if report.Status == "attempting" {
			if f.Status == "blocked" || f.Status == "superseded" {
				return failure("stale_followup", "this followup no longer accepts submissions")
			}
			task, err := loadTask(tx, b, f.TaskID)
			if err != nil {
				return err
			}
			if task.Paused || task.Version != f.TaskVersion {
				return failure("paused", "task paused or changed before submission")
			}
			f.SubmissionAttempts++
			task.SubmissionAttempts++
			if err := tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
				return err
			}
			return tx.Put("followup", owner(b.scope()), id, f)
		}
		f.Status, f.Error = "blocked", report.Error
		task, err := loadTask(tx, b, f.TaskID)
		if err != nil {
			return err
		}
		if task.Version == f.TaskVersion {
			task.Waiting = "submission_unknown"
			if err := tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
				return err
			}
		}
		if err := queueAttention(tx, b, task, "submission|"+id, "跟进提交结果无法核实，已停止自动重发："+report.Error); err != nil {
			return err
		}
		return tx.Put("followup", owner(b.scope()), id, f)
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(f)
}
