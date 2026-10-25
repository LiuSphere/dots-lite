package hub

import (
	"context"
	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
	"encoding/json"
	"errors"
	"fmt"
)

func ensureProtocol(ctx context.Context, db *store.DB, cfg Config) error {
	return db.Update(ctx, func(tx *store.Tx) error {
		var version int
		err := tx.Get("meta", "dots-lite", "protocol", &version)
		if err == nil {
			if version != protocol.Version {
				return fmt.Errorf("state protocol %d requires an offline migration", version)
			}
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		owners, err := tx.Owners("task")
		if err != nil {
			return err
		}
		if len(owners) > 0 {
			return errors.New("legacy hub state requires an explicit offline protocol 4 migration")
		}
		return tx.Put("meta", "dots-lite", "protocol", protocol.Version)
	})
}

// MigrateV3 is an offline operation. The caller stops all service writers and
// backs up the consistent database before invoking this transaction.
func MigrateV3(ctx context.Context, db *store.DB, cfg Config) error {
	return db.Update(ctx, func(tx *store.Tx) error {
		var version int
		err := tx.Get("meta", "dots-lite", "protocol", &version)
		if err == nil {
			return fmt.Errorf("state already has protocol %d; migration was not repeated", version)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		covered := map[string]bool{}
		for _, b := range cfg.Bindings {
			covered[owner(b.scope())] = true
		}
		owners, err := tx.Owners("task")
		if err != nil {
			return err
		}
		for _, o := range owners {
			if !covered[o] {
				return errors.New("restore all legacy task bindings in configuration before global protocol migration")
			}
		}
		for _, b := range cfg.Bindings {
			if b.DirectoryMode == "task" || len(b.FolderIDs) == 0 {
				return errors.New("legacy migration must retain the existing allowlist and folder_ids")
			}
			rows, err := tx.List("task", owner(b.scope()))
			if err != nil {
				return err
			}
			for _, raw := range rows {
				var task taskRecord
				if err := json.Unmarshal(raw, &task); err != nil {
					return err
				}
				if !task.Paused {
					task.Paused = true
					task.Version++
				}
				task.DirectoryMode = "allowlist"
				if task.Waiting != "recovery_budget_exhausted" && task.Waiting != "user_decision" {
					task.Waiting = "migration_review"
				}
				if err := tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
					return err
				}
				if task.ThreadID != "" {
					var bound string
					err := tx.Get("thread_binding", owner(b.scope()), task.ThreadID, &bound)
					if err == nil && bound != task.ID {
						return errors.New("legacy native thread is bound to multiple tasks; manual reconciliation is required")
					}
					if err != nil && !errors.Is(err, store.ErrNotFound) {
						return err
					}
					if err := tx.Put("thread_binding", owner(b.scope()), task.ThreadID, task.ID); err != nil {
						return err
					}
				}
			}
		}
		return tx.Put("meta", "dots-lite", "protocol", protocol.Version)
	})
}
