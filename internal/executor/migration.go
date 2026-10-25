package executor

import (
	"context"
	"database/sql"
	"dots-lite/internal/protocol"
	"encoding/json"
	"errors"
	"os"
	"time"
)

// MigrateV3 never starts native execution. Existing directories and receipts are
// retained; native permissions without evidence remain pending human review.
func MigrateV3(ctx context.Context, config ServiceConfig) (string, error) {
	if config.DirectoryMode == "task" || len(config.Folders) == 0 {
		return "", errors.New("legacy migration must retain folders and allowlist mode")
	}
	unlock, err := lockState(config.StatePath)
	if err != nil {
		return "", err
	}
	defer unlock()
	db, err := sql.Open("sqlite", config.StatePath)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var version string
	err = db.QueryRowContext(ctx, `SELECT value FROM executor_meta WHERE key='state_protocol'`).Scan(&version)
	if err == nil {
		return "", errors.New("executor state already has a protocol marker; migration was not repeated")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	backup := config.StatePath + ".v3-backup-" + time.Now().UTC().Format("20060102T150405.000000000")
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
		return "", err
	}
	if err := os.Chmod(backup, 0600); err != nil {
		return backup, err
	}
	var stored string
	expected := string(mustJSON(config.Scope))
	if err := db.QueryRowContext(ctx, `SELECT value FROM executor_meta WHERE key='scope'`).Scan(&stored); err != nil || stored != expected {
		return backup, errors.New("migration database scope does not match configuration")
	}
	rows, err := db.QueryContext(ctx, `SELECT payload,folder_root FROM executor_tasks`)
	if err != nil {
		return backup, err
	}
	type item struct {
		task     protocol.Task
		root     string
		identity string
	}
	items := []item{}
	for rows.Next() {
		var raw string
		var it item
		if err := rows.Scan(&raw, &it.root); err != nil {
			rows.Close()
			return backup, err
		}
		if err := json.Unmarshal([]byte(raw), &it.task); err != nil {
			rows.Close()
			return backup, err
		}
		allowed := false
		for _, folder := range config.Folders {
			if folder.ID == it.task.FolderID && pathWithin(folder.Root, it.root) {
				allowed = true
				break
			}
		}
		if !allowed {
			rows.Close()
			return backup, errors.New("legacy task directory does not match its original configured folder")
		}
		root, err := os.OpenRoot(it.root)
		if err != nil {
			rows.Close()
			return backup, err
		}
		identity, identityErr := directoryIdentity(root)
		if err := errors.Join(identityErr, root.Close()); err != nil {
			rows.Close()
			return backup, err
		}
		it.identity = identity
		if !it.task.Paused {
			it.task.Paused = true
			it.task.Version++
		}
		it.task.DirectoryMode = "allowlist"
		it.task.Waiting = "migration_review"
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return backup, err
	}
	rows.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return backup, err
	}
	defer tx.Rollback()
	for _, it := range items {
		if _, err := tx.ExecContext(ctx, `UPDATE executor_tasks SET payload=? WHERE task_id=?`, string(mustJSON(it.task)), it.task.ID); err != nil {
			return backup, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO executor_meta(key,value) VALUES(?,?)`, "task_root_identity:"+it.task.ID, it.identity); err != nil {
			return backup, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO executor_meta(key,value) VALUES('state_protocol','4'),('maintenance',?)`, string(mustJSON(protocol.Maintenance{Epoch: 1, Phase: "stopped"}))); err != nil {
		return backup, err
	}
	return backup, tx.Commit()
}
