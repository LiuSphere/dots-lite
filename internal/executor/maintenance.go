package executor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"dots-lite/internal/protocol"
)

func (s *Service) persistMaintenanceLocked(ctx context.Context, phase string, cause error) error {
	next := s.maintenance
	next.Phase = phase
	next.Error = ""
	if cause != nil {
		next.Error = cause.Error()
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO executor_meta(key,value) VALUES('maintenance',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(mustJSON(next))); err != nil {
		return err
	}
	s.maintenance = next
	return nil
}

func (s *Service) MaintenanceStatus() protocol.Maintenance {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maintenance
}

func (s *Service) queueMaintenanceSnapshot(ctx context.Context) error {
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maintenance.RemoteSyncPending = true
	if err := s.persistMaintenanceLocked(ctx, s.maintenance.Phase, nil); err != nil {
		return err
	}
	snapshot.Maintenance = s.maintenance
	event := protocol.Event{InstanceID: snapshot.InstanceID, Type: "executor/maintenance", Data: mustJSON(snapshot), CreatedAt: time.Now().UTC()}
	_, err = s.db.ExecContext(ctx, `INSERT INTO executor_events(payload) VALUES(?)`, string(mustJSON(event)))
	return err
}

func (s *Service) checkWrite(op protocol.Operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op.Action == "task.stop" {
		return nil
	}
	if s.maintenance.Phase != "running" || s.maintenance.Epoch != op.MaintenanceEpoch {
		return &protocol.Error{Code: "maintenance", Message: "service is in maintenance or this operation belongs to an older epoch"}
	}
	return nil
}

func (s *Service) nativeWrite(ctx context.Context, client *Client, op protocol.Operation, method string, params any) (json.RawMessage, error) {
	return client.callChecked(ctx, method, params, func() error {
		if err := s.checkWrite(op); err != nil {
			return err
		}
		if op.TaskID != "" && (method == "thread/start" || method == "thread/resume" || method == "turn/start") {
			s.mu.Lock()
			defer s.mu.Unlock()
			task, err := s.loadTask(ctx, op.TaskID)
			if err != nil {
				return err
			}
			futureResume := method == "thread/resume" && op.Action == "task.continue" && (task.Paused || op.Foreground) && op.TaskVersion == task.Version+1
			if (!futureResume && task.Version != op.TaskVersion) || (task.Paused && method != "thread/resume") {
				return errors.New("task paused or version changed before native submission")
			}
			_, _, err = s.taskRootLocked(ctx, task)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func LockMaintenanceCommand(statePath string) (func() error, error) {
	return lockState(statePath + ".control")
}

// PauseForMaintenance closes admission first, preserves each task's pause, then
// drains old writes. Its success means shutdown may be requested, not PID exit.
func (s *Service) PauseForMaintenance(ctx context.Context) (err error) {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	s.mu.Lock()
	if s.maintenance.Phase == "running" {
		s.maintenance.Epoch++
	}
	err = s.persistMaintenanceLocked(ctx, "quiescing", nil)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `UPDATE executor_tasks SET payload=json_set(payload,'$.paused',json('true'),'$.version',json_extract(payload,'$.version')+CASE WHEN json_extract(payload,'$.paused')=0 THEN 1 ELSE 0 END)`)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.queueMaintenanceSnapshot(ctx); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			s.mu.Lock()
			saveErr := s.persistMaintenanceLocked(context.WithoutCancel(ctx), "failed", err)
			s.mu.Unlock()
			err = errors.Join(err, saveErr)
		}
	}()
	// Interrupt known execution before draining acknowledgements. Some native
	// requests only settle after a status read or interrupt, so waiting first
	// would deadlock the very pause needed to settle them.
	initial, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	missingClient := s.client == nil
	s.mu.Unlock()
	if missingClient {
		for _, task := range initial.Tasks {
			if task.ThreadID != "" {
				if err := s.startNative(ctx, true); err != nil {
					return fmt.Errorf("initialize native inspection for the pending pause: %w", err)
				}
				break
			}
		}
	}
	for _, task := range initial.Tasks {
		if task.ThreadID == "" {
			continue
		}
		s.mu.Lock()
		client := s.client
		s.mu.Unlock()
		_ = s.stopTask(ctx, protocol.Operation{ID: "maintenance-first-" + task.ID, TaskID: task.ID, Scope: task.Scope, FolderID: task.FolderID, TaskVersion: task.Version, Action: "task.stop"}, client)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	// TryLock keeps the deadline effective while a native acknowledgement is late.
	for !s.writeGate.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer s.writeGate.Unlock()
	s.mu.Lock()
	starting := s.starting
	s.mu.Unlock()
	if starting {
		return errors.New("native initialization is still in flight; retry pause after it settles")
	}
	state, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	for _, task := range state.Tasks {
		if task.ThreadID == "" {
			var count int
			if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM executor_operations WHERE json_extract(result,'$.task_id')=? AND json_extract(result,'$.stage') IN ('unknown','dispatched')`, task.ID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("task %s has an unresolved creation", task.ID)
			}
			continue
		}
		s.mu.Lock()
		client := s.client
		s.mu.Unlock()
		op := protocol.Operation{ID: "maintenance-" + task.ID, TaskID: task.ID, Scope: task.Scope, FolderID: task.FolderID, WorkingDirectory: task.WorkingDirectory, ThreadID: task.ThreadID, TaskVersion: task.Version, Action: "task.stop", MaintenanceEpoch: s.MaintenanceStatus().Epoch}
		result := s.stopTask(ctx, op, client)
		if result.Stage != "accepted" || result.Error != nil {
			return fmt.Errorf("task %s pause is unconfirmed: %v", task.ID, result.Error)
		}
	}
	s.mu.Lock()
	err = s.persistMaintenanceLocked(ctx, "stopping", nil)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.queueMaintenanceSnapshot(ctx); err != nil {
		return err
	}
	for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); {
		if !s.MaintenanceStatus().RemoteSyncPending {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

// CompleteOfflineMaintenance is called only while the instance lock is held
// after the controlled process has exited. It never starts a native process.
func CompleteOfflineMaintenance(ctx context.Context, statePath string, resume bool) (protocol.Maintenance, error) {
	var state protocol.Maintenance
	unlock, err := lockState(statePath)
	if err != nil {
		return state, err
	}
	defer unlock()
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		return state, err
	}
	defer db.Close()
	var raw string
	if err = db.QueryRowContext(ctx, `SELECT value FROM executor_meta WHERE key='maintenance'`).Scan(&raw); err != nil {
		return state, err
	}
	if err = json.Unmarshal([]byte(raw), &state); err != nil {
		return state, err
	}
	if resume {
		if state.Phase != "stopped" {
			return state, errors.New("finish the original pause and confirm complete exit before resuming")
		}
		state.Epoch++
		state.Phase = "resuming"
	} else {
		if state.Phase != "stopping" && state.Phase != "stopped" {
			return state, errors.New("executor did not confirm quiescence; maintenance cannot be marked stopped")
		}
		var clean string
		if err := db.QueryRowContext(ctx, `SELECT value FROM executor_meta WHERE key='shutdown_clean'`).Scan(&clean); err != nil || clean != fmt.Sprint(state.Epoch) {
			return state, errors.New("owned process cleanup was not confirmed for this maintenance epoch")
		}
		state.Phase = "stopped"
	}
	state.Error = ""
	_, err = db.ExecContext(ctx, `UPDATE executor_meta SET value=? WHERE key='maintenance'`, string(mustJSON(state)))
	return state, err
}

func ReadOfflineMaintenance(ctx context.Context, statePath string) (protocol.Maintenance, error) {
	var state protocol.Maintenance
	if _, err := os.Stat(statePath); err != nil {
		return state, err
	}
	unlock, err := lockState(statePath)
	if err != nil {
		return state, err
	}
	defer unlock()
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		return state, err
	}
	defer db.Close()
	var raw string
	err = db.QueryRowContext(ctx, `SELECT value FROM executor_meta WHERE key='maintenance'`).Scan(&raw)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal([]byte(raw), &state)
	return state, err
}
