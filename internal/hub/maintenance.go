package hub

import (
	"context"
	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
)

func (h *Hub) saveMaintenanceEvent(ctx context.Context, b Binding, event protocol.Event) error {
	var snapshot protocol.Snapshot
	if err := json.Unmarshal(event.Data, &snapshot); err != nil {
		return err
	}
	for _, task := range snapshot.Tasks {
		if task.Scope != b.scope() {
			return failure("scope_mismatch", "maintenance task scope differs")
		}
	}
	applied := false
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		if event.Sequence < 1 {
			return failure("invalid_event", "maintenance sequence must be positive")
		}
		key := strconv.FormatInt(event.Sequence, 10)
		var old protocol.Event
		err := tx.Get("event", owner(b.scope()), key, &old)
		if err == nil {
			if !reflect.DeepEqual(old, event) {
				return failure("event_conflict", "maintenance event sequence changed")
			}
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		var last int64
		err = tx.Get("cursor", owner(b.scope()), "events", &last)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if event.Sequence != last+1 {
			return failure("event_gap", "maintenance event is out of order")
		}
		var current protocol.Maintenance
		err = tx.Get("maintenance", owner(b.scope()), "executor", &current)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// A reconnect hello may already contain a newer maintenance epoch.
		// Commit and acknowledge the old fact without reverting its barrier.
		if snapshot.Maintenance.Epoch >= current.Epoch {
			if err := applySnapshotTx(tx, b, snapshot); err != nil {
				return err
			}
			applied = true
		}
		if err := tx.Put("event", owner(b.scope()), key, event); err != nil {
			return err
		}
		return tx.Put("cursor", owner(b.scope()), "events", event.Sequence)
	})
	if err == nil && applied {
		h.mu.Lock()
		h.maintenance[b.ExecutorID] = snapshot.Maintenance
		h.mu.Unlock()
	}
	return err
}
