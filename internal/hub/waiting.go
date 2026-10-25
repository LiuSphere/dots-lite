package hub

import (
	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
	"encoding/json"
	"time"
)

type waitCondition struct {
	Kind         string    `json:"kind"`
	CheckAt      time.Time `json:"check_at"`
	NativeStatus string    `json:"native_status,omitempty"`
	GoalStatus   string    `json:"goal_status,omitempty"`
}

func (h *Hub) wakeWaiting(tx *store.Tx, b Binding, task taskRecord, nativeStatus, goalStatus string, now time.Time) error {
	rows, err := tx.List("followup", owner(b.scope()))
	if err != nil {
		return err
	}
	for _, raw := range rows {
		var f followup
		if err := json.Unmarshal(raw, &f); err != nil {
			return err
		}
		if f.TaskID != task.ID || f.TaskVersion != task.Version || f.Status != "waiting" || f.Wait == nil || !terminalRun(f.NativeStatus) || now.Before(f.Wait.CheckAt) {
			continue
		}
		if f.Wait.Kind == "native_state" && ((f.Wait.NativeStatus != "" && f.Wait.NativeStatus != nativeStatus) || (f.Wait.GoalStatus != "" && f.Wait.GoalStatus != goalStatus)) {
			continue
		}
		// The predicate is checked by the program. A timestamp-only progress log
		// cannot create this obligation; it follows an explicit recorded wait.
		f.Status = "handled"
		if err := tx.Put("followup", owner(b.scope()), f.ID, f); err != nil {
			return err
		}
		var sequence int64
		err := tx.Get("cursor", owner(b.scope()), "reconcile", &sequence)
		if err != nil && err != store.ErrNotFound {
			return err
		}
		sequence--
		event := protocol.Event{Sequence: sequence, TaskID: task.ID, TurnID: task.TurnID, Type: "task/reconcile", Data: json.RawMessage(`{"reason":"wait_ready:` + f.ID + `"}`), CreatedAt: now}
		if err := h.createFollowup(tx, b, task, event); err != nil {
			return err
		}
		if err := tx.Put("cursor", owner(b.scope()), "reconcile", sequence); err != nil {
			return err
		}
	}
	return nil
}
