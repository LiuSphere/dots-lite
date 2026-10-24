// Package store owns this application's SQLite records, not Hermes or Codex data.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = sql.ErrNoRows

type DB struct {
	db *sql.DB
	mu sync.Mutex
}
type Tx struct {
	ctx context.Context
	tx  *sql.Tx
}

func Open(path string) (*DB, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
		if err = os.Chmod(path, 0600); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS documents(kind TEXT NOT NULL, owner TEXT NOT NULL, id TEXT NOT NULL, value BLOB NOT NULL, updated INTEGER NOT NULL, PRIMARY KEY(kind,owner,id));`)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &DB{db: db}, nil
}
func (d *DB) Close() error { return d.db.Close() }
func (d *DB) Update(ctx context.Context, fn func(*Tx) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	err = fn(&Tx{ctx: ctx, tx: t})
	if err == nil {
		return t.Commit()
	}
	rollback := t.Rollback()
	if errors.Is(rollback, sql.ErrTxDone) {
		rollback = nil
	}
	return errors.Join(err, rollback)
}
func (t *Tx) Get(kind, owner, id string, dst any) error {
	var data []byte
	if err := t.tx.QueryRowContext(t.ctx, "SELECT value FROM documents WHERE kind=? AND owner=? AND id=?", kind, owner, id).Scan(&data); err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}
func (t *Tx) Put(kind, owner, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(t.ctx, "INSERT INTO documents(kind,owner,id,value,updated) VALUES(?,?,?,?,?) ON CONFLICT(kind,owner,id) DO UPDATE SET value=excluded.value,updated=excluded.updated", kind, owner, id, data, time.Now().UnixNano())
	return err
}
func (t *Tx) List(kind, owner string) ([]json.RawMessage, error) {
	rows, err := t.tx.QueryContext(t.ctx, "SELECT value FROM documents WHERE kind=? AND owner=? ORDER BY updated DESC,id", kind, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(b))
	}
	return out, rows.Err()
}
