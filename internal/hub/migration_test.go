package hub

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
)

func TestHubMigrationPreservesReceiptsAndAllOwnersMustBeCovered(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := Binding{UID: 1001, UserID: "legacy", ProfileID: "legacy", ExecutorID: "legacy", Token: "fixture", FolderIDs: []string{"root"}}
	cfg := Config{Bindings: []Binding{b}}
	if _, err := New(cfg, db); err != nil {
		t.Fatal(err)
	}
	task := taskRecord{Task: protocol.Task{ID: "legacy-task", Scope: b.scope(), FolderID: "root", ThreadID: "native-original", Version: 3, Waiting: "recovery_budget_exhausted"}, RecoveryUsed: 8, RecoveryAttempts: 24}
	op := operationRecord{Operation: protocol.Operation{ID: "original-op", TaskID: task.ID, Scope: b.scope(), TaskVersion: 3, ThreadID: task.ThreadID, Action: "task.continue", Input: "immutable original input"}, Stage: "unknown"}
	if err := db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
			return err
		}
		return tx.Put("operation", owner(b.scope()), op.ID, op)
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var before []byte
	if err := raw.QueryRow(`SELECT value FROM documents WHERE kind='operation'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM documents WHERE kind='meta' AND owner='dots-lite' AND id='protocol'`); err != nil {
		t.Fatal(err)
	}
	other := b
	other.UserID = "other"
	other.ExecutorID = "other"
	if _, err := New(Config{Bindings: []Binding{other}}, db); err == nil {
		t.Fatal("subset binding silently marked legacy state as protocol 4")
	}
	if err := MigrateV3(ctx, db, Config{Bindings: []Binding{other}}); err == nil {
		t.Fatal("global migration ignored another legacy owner")
	}
	if err := db.Backup(ctx, path+".backup"); err != nil {
		t.Fatal(err)
	}
	if err := MigrateV3(ctx, db, cfg); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(ctx, func(tx *store.Tx) error {
		actual, err := loadTask(tx, b, task.ID)
		if err != nil {
			return err
		}
		if actual.ThreadID != task.ThreadID || !actual.Paused || actual.Version != 4 || actual.RecoveryUsed != 8 || actual.RecoveryAttempts != 24 || actual.Waiting != "recovery_budget_exhausted" || actual.DirectoryMode != "allowlist" {
			t.Fatalf("migration lost identity, history or pause: %+v", actual)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var after []byte
	if err := raw.QueryRow(`SELECT value FROM documents WHERE kind='operation'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("migration rewrote an immutable unknown operation")
	}
}
