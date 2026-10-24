package executor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"dots-lite/internal/protocol"
)

type pendingRequest struct {
	ID         string          `json:"request_id"`
	InstanceID string          `json:"instance_id"`
	TaskID     string          `json:"task_id"`
	ThreadID   string          `json:"thread_id"`
	TurnID     string          `json:"turn_id,omitempty"`
	Method     string          `json:"method"`
	Params     json.RawMessage `json:"params"`
	NativeID   json.RawMessage `json:"native_id"`
}

func (s *Service) pumpNative(client *Client) {
	defer close(s.pumpDone)
	faulted := false
	for message := range client.Events() {
		if faulted {
			continue
		}
		if err := s.handleNative(message); err != nil {
			s.mu.Lock()
			s.failNativeEventsLocked(err)
			s.mu.Unlock()
			faulted = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nativeErr = errors.Join(s.nativeErr, client.Err())
	if err := s.recordNativeExit(context.Background(), errors.Join(s.eventFault, client.Err())); err != nil {
		s.failNativeEventsLocked(err)
	}
}

func (s *Service) failNativeEventsLocked(err error) {
	s.eventFault = errors.Join(s.eventFault, err)
	s.nativeErr = errors.Join(s.nativeErr, err)
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Service) recordNativeExit(ctx context.Context, cause error) (err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM executor_tasks WHERE occupied=1 OR json_extract(payload,'$.waiting')<>''`)
	if err != nil {
		return err
	}
	var tasks []protocol.Task
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return err
		}
		var task protocol.Task
		if err := json.Unmarshal([]byte(data), &task); err != nil {
			rows.Close()
			return err
		}
		task.Execution, task.Waiting, task.UpdatedAt = "unknown", "", time.Now().UTC()
		tasks = append(tasks, task)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	if _, err = tx.ExecContext(ctx, `UPDATE executor_requests SET valid=0`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE executor_operations SET result=json_set(result,'$.stage','unknown') WHERE json_extract(result,'$.stage')='dispatched'`); err != nil {
		return err
	}
	for _, task := range tasks {
		if _, err = tx.ExecContext(ctx, `UPDATE executor_tasks SET payload=? WHERE task_id=?`, string(mustJSON(task)), task.ID); err != nil {
			return err
		}
		message := "native event stream ended"
		if cause != nil {
			message = cause.Error()
		}
		event := protocol.Event{TaskID: task.ID, InstanceID: s.instance, ThreadID: task.ThreadID, TurnID: task.TurnID, Type: "task/native_exited", Data: mustJSON(map[string]any{"task": task, "native_status": "unknown", "error": protocol.Error{Code: "native_exited", Message: message}}), CreatedAt: time.Now().UTC()}
		if _, err = tx.ExecContext(ctx, `INSERT INTO executor_events(payload) VALUES(?)`, string(mustJSON(event))); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) handleNative(message Message) (err error) {
	if strings.HasSuffix(message.Method, "/delta") {
		return nil
	}
	var params struct {
		ThreadID  string          `json:"threadId"`
		TurnID    string          `json:"turnId"`
		RequestID json.RawMessage `json:"requestId"`
		Turn      struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if message.Method == "serverRequest/resolved" {
		rows, err := tx.Query(`SELECT request_id,payload FROM executor_requests WHERE valid=1`)
		if err != nil {
			return err
		}
		var matches []string
		key, err := requestKey(params.RequestID)
		if err != nil {
			rows.Close()
			return err
		}
		for rows.Next() {
			var id, text string
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close()
				return err
			}
			var request pendingRequest
			if err := json.Unmarshal([]byte(text), &request); err != nil {
				rows.Close()
				return err
			}
			other, err := requestKey(request.NativeID)
			if err != nil {
				rows.Close()
				return err
			}
			if other == key && request.ThreadID == params.ThreadID {
				matches = append(matches, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, id := range matches {
			if _, err := tx.Exec(`UPDATE executor_requests SET valid=0 WHERE request_id=?`, id); err != nil {
				return err
			}
		}
	}
	if params.ThreadID == "" {
		return nil
	}
	var taskJSON string
	if err := tx.QueryRow(`SELECT payload FROM executor_tasks WHERE json_extract(payload,'$.thread_id')=?`, params.ThreadID).Scan(&taskJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var task protocol.Task
	if err := json.Unmarshal([]byte(taskJSON), &task); err != nil {
		return err
	}
	data := message.Params
	keyEvent := false
	if len(message.ID) > 0 {
		if task.TurnID == "" && params.TurnID != "" {
			task.TurnID = params.TurnID
		}
		request := pendingRequest{ID: randomID(), InstanceID: s.instance, TaskID: task.ID, ThreadID: params.ThreadID, TurnID: params.TurnID, Method: message.Method, Params: message.Params, NativeID: message.ID}
		if _, err := tx.Exec(`INSERT INTO executor_requests VALUES(?,?,1)`, request.ID, string(mustJSON(request))); err != nil {
			return err
		}
		task.Waiting = request.ID
		data = mustJSON(map[string]any{"request_id": request.ID, "instance_id": request.InstanceID, "task_id": task.ID, "method": message.Method, "params": message.Params})
		keyEvent = true
	}
	switch message.Method {
	case "turn/started":
		task.TurnID, task.Execution = params.Turn.ID, "running"
		keyEvent = true
	case "turn/completed":
		task.TurnID, task.Execution, task.Waiting = params.Turn.ID, params.Turn.Status, ""
		keyEvent = true
	case "serverRequest/resolved":
		task.Waiting = ""
		keyEvent = true
	case "thread/goal/updated", "thread/goal/cleared", "error":
		keyEvent = true
	case "item/completed":
		keyEvent = true
	}
	if err := saveTaskWith(context.Background(), tx, task); err != nil {
		return err
	}
	if keyEvent {
		event := protocol.Event{TaskID: task.ID, InstanceID: s.instance, ThreadID: task.ThreadID, TurnID: task.TurnID, Type: message.Method, Data: data, CreatedAt: time.Now().UTC()}
		if _, err := tx.Exec(`INSERT INTO executor_events(payload) VALUES(?)`, string(mustJSON(event))); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if message.Method == "turn/completed" || message.Method == "thread/goal/updated" || message.Method == "thread/goal/cleared" {
		s.startReconcileLocked(task, s.client)
	}
	return nil
}

// Completion events are hints. Read native state outside the event pump so that
// an active Goal can keep emitting events while a turn is being reconciled.
func (s *Service) startReconcileLocked(task protocol.Task, client *Client) {
	if client == nil {
		return
	}
	if s.reconciling[task.ID] {
		s.reconcilePending[task.ID] = task
		return
	}
	s.reconciling[task.ID] = true
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := s.reconcileCompletion(ctx, task, client)
		s.mu.Lock()
		delete(s.reconciling, task.ID)
		if err != nil {
			s.nativeErr = errors.Join(s.nativeErr, err)
		}
		if pending, ok := s.reconcilePending[task.ID]; ok {
			delete(s.reconcilePending, task.ID)
			s.startReconcileLocked(pending, client)
		}
		s.mu.Unlock()
	}()
}

func (s *Service) reconcileCompletion(ctx context.Context, expected protocol.Task, client *Client) error {
	if expected.Paused || expected.TurnID == "" || (expected.Execution != "completed" && expected.Execution != "interrupted" && expected.Execution != "failed") {
		return nil
	}
	for {
		goal, err := readNativeGoalStatus(ctx, client, expected.ThreadID)
		if err != nil {
			return err
		}
		if !quietGoal(goal) {
			return nil
		}
		status, err := readNativeStatus(ctx, client, expected.ThreadID)
		if err != nil {
			return err
		}
		if status != "idle" {
			return nil
		}
		s.mu.Lock()
		task, err := s.loadTask(ctx, expected.ID)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if task.Paused || task.Version != expected.Version || task.TurnID != expected.TurnID || task.Execution != expected.Execution || task.Waiting != "" {
			s.mu.Unlock()
			return nil
		}
		var occupied, uncertain int
		err = s.db.QueryRowContext(ctx, `SELECT occupied FROM executor_tasks WHERE task_id=?`, task.ID).Scan(&occupied)
		if err == nil {
			err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM executor_operations WHERE json_extract(result,'$.task_id')=? AND json_extract(result,'$.stage') IN ('unknown','dispatched') AND (action='' OR action IN ('task.create','task.continue','turn.steer','task.stop','goal.set','approval.respond','question.respond','files.upload'))`, task.ID).Scan(&uncertain)
		}
		if err != nil || occupied == 0 {
			s.mu.Unlock()
			return err
		}
		if uncertain > 0 {
			s.mu.Unlock()
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				continue
			}
		}
		// Keep the task lock through authorized file reads and commit. A new
		// operation cannot claim the root before these artifact bytes are captured.
		events := make([]protocol.Event, 0, len(task.Artifacts)+1)
		taskRoot, _, rootErr := s.taskRootLocked(ctx, task)
		for _, path := range task.Artifacts {
			result := protocol.Result{Stage: "failed"}
			if rootErr != nil {
				result = failResult(result, "working_directory_unavailable", rootErr.Error())
			} else {
				result = s.readFileAtRoot(ctx, protocol.Operation{TaskID: task.ID, FolderID: task.FolderID, Action: "files.download", Path: path}, taskRoot)
			}
			event := protocol.Event{TaskID: task.ID, InstanceID: s.instance, ThreadID: task.ThreadID, TurnID: task.TurnID, CreatedAt: time.Now().UTC()}
			if result.Stage == "accepted" {
				var artifact map[string]any
				if err := json.Unmarshal(result.Data, &artifact); err != nil {
					s.mu.Unlock()
					return err
				}
				artifact["task_version"] = task.Version
				event.Type, event.Data = "artifact/available", mustJSON(artifact)
			} else {
				event.Type, event.Data = "artifact/failed", mustJSON(map[string]any{"task_version": task.Version, "path": path, "error": result.Error})
			}
			events = append(events, event)
		}
		events = append(events, protocol.Event{TaskID: task.ID, InstanceID: s.instance, ThreadID: task.ThreadID, TurnID: task.TurnID, Type: "task/quiescent", Data: mustJSON(map[string]any{"goal_status": goal, "native_status": status, "task": task}), CreatedAt: time.Now().UTC()})
		err = s.commitQuiescent(ctx, task.ID, events)
		if err != nil {
			s.failNativeEventsLocked(err)
		}
		s.mu.Unlock()
		return err
	}
}

func (s *Service) commitQuiescent(ctx context.Context, taskID string, events []protocol.Event) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	for _, event := range events {
		if _, err = tx.ExecContext(ctx, `INSERT INTO executor_events(payload) VALUES(?)`, string(mustJSON(event))); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE executor_tasks SET occupied=0 WHERE task_id=?`, taskID); err != nil {
		return err
	}
	return tx.Commit()
}

func readNativeGoalStatus(ctx context.Context, client *Client, threadID string) (string, error) {
	data, err := client.Call(ctx, "thread/goal/get", map[string]string{"threadId": threadID})
	if err != nil {
		return "", err
	}
	var state struct {
		Goal json.RawMessage `json:"goal"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return "", err
	}
	if len(state.Goal) == 0 {
		return "", errors.New("native Goal response omitted goal")
	}
	if string(state.Goal) == "null" {
		return "none", nil
	}
	var goal struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(state.Goal, &goal); err != nil {
		return "", err
	}
	return goal.Status, nil
}

func quietGoal(status string) bool {
	switch status {
	case "none", "paused", "blocked", "usageLimited", "budgetLimited", "complete":
		return true
	}
	return false
}

func (s *Service) PendingEvents(ctx context.Context, after int64) ([]protocol.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,payload FROM executor_events WHERE sequence>? ORDER BY sequence LIMIT 100`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []protocol.Event{}
	for rows.Next() {
		var seq int64
		var text string
		if err := rows.Scan(&seq, &text); err != nil {
			return nil, err
		}
		var event protocol.Event
		if err := json.Unmarshal([]byte(text), &event); err != nil {
			return nil, err
		}
		event.Sequence = seq
		events = append(events, event)
	}
	return events, rows.Err()
}
func (s *Service) AckEvents(ctx context.Context, sequence int64) error {
	var highest int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='executor_events'),0)`).Scan(&highest); err != nil {
		return err
	}
	if sequence < 0 || sequence > highest {
		return errors.New("event acknowledgement exceeds emitted sequence")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM executor_events WHERE sequence<=?`, sequence)
	return err
}
func (s *Service) applyLatePause(ctx context.Context, op protocol.Operation, task protocol.Task, client *Client) protocol.Result {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	op.TaskVersion, op.Action = task.Version, "task.stop"
	r := s.stopTask(ctx, op, client)
	if r.Stage == "accepted" {
		r.Stage = "failed"
		return failResult(r, "paused", "pause was applied after the in-flight native write acknowledgement arrived")
	}
	return r
}

func (s *Service) stopTask(ctx context.Context, op protocol.Operation, client *Client) protocol.Result {
	r := protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "unknown"}
	s.mu.Lock()
	task, err := s.loadTask(ctx, op.TaskID)
	if err != nil {
		s.mu.Unlock()
		r.Stage = "failed"
		return failResult(r, "task_not_found", err.Error())
	}
	if op.FolderID != task.FolderID {
		s.mu.Unlock()
		r.Stage = "failed"
		return failResult(r, "forbidden", "task belongs to a different folder")
	}
	if op.TaskVersion < task.Version {
		s.mu.Unlock()
		r.Stage = "failed"
		return failResult(r, "stale_version", "stop version is older than task")
	}
	task.Paused = true
	if op.TaskVersion > task.Version {
		task.Version = op.TaskVersion
	}
	if task.Execution != "completed" && task.Execution != "interrupted" && task.Execution != "failed" && task.Execution != "stopped" {
		task.Execution = "stopping"
	}
	err = s.saveTask(ctx, task)
	s.mu.Unlock()
	if err != nil {
		return failResult(r, "storage", err.Error())
	}
	r.ThreadID, r.TurnID = task.ThreadID, task.TurnID
	if task.ThreadID == "" || client == nil {
		return failResult(r, "stop_unconfirmed", "pause intent is stored; native execution cannot yet be checked")
	}
	goal, err := client.Call(ctx, "thread/goal/get", map[string]string{"threadId": task.ThreadID})
	if err != nil {
		return failResult(r, "native", err.Error())
	}
	var state struct {
		Goal *struct {
			Status string `json:"status"`
		} `json:"goal"`
	}
	if err := json.Unmarshal(goal, &state); err != nil {
		return failResult(r, "native_response", err.Error())
	}
	if state.Goal != nil && state.Goal.Status != "paused" && state.Goal.Status != "complete" {
		if _, err := client.Call(ctx, "thread/goal/set", map[string]string{"threadId": task.ThreadID, "status": "paused", "origin": "automatic"}); err != nil {
			return failResult(r, "native", err.Error())
		}
	}
	status, err := readNativeStatus(ctx, client, task.ThreadID)
	if err != nil {
		return failResult(r, "native", err.Error())
	}
	var interruptErr error
	if status != "idle" {
		if status != "active" || task.TurnID == "" {
			return failResult(r, "stop_unconfirmed", "active native turn cannot yet be identified")
		}
		_, interruptErr = client.Call(ctx, "turn/interrupt", map[string]string{"threadId": task.ThreadID, "turnId": task.TurnID})
		var rpcErr *RPCError
		if interruptErr != nil && !errors.As(interruptErr, &rpcErr) {
			return failResult(r, "native", interruptErr.Error())
		}
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
			status, err = readNativeStatus(ctx, client, task.ThreadID)
			if err != nil {
				return failResult(r, "native", errors.Join(interruptErr, err).Error())
			}
			if status == "idle" {
				break
			}
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return failResult(r, "stop_unconfirmed", ctx.Err().Error())
			case <-timer.C:
			}
		}
		if status != "idle" {
			return failResult(r, "stop_unconfirmed", errors.Join(errors.New("native thread has not become idle"), interruptErr).Error())
		}
	}
	goalStatus, err := readNativeGoalStatus(ctx, client, task.ThreadID)
	if err != nil {
		return failResult(r, "native", err.Error())
	}
	if !quietGoal(goalStatus) {
		return failResult(r, "stop_unconfirmed", "native goal has not been proven inactive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, err := s.loadTask(ctx, task.ID)
	if err != nil {
		return failResult(r, "storage", err.Error())
	}
	if latest.Version != task.Version || !latest.Paused {
		return failResult(r, "stop_unconfirmed", "task changed while its native execution was being checked")
	}
	var pending int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM executor_operations WHERE operation_id<>? AND json_extract(result,'$.task_id')=? AND json_extract(result,'$.stage') IN ('unknown','dispatched') AND (action='' OR action IN ('task.create','task.continue','turn.steer','goal.set','approval.respond','question.respond','files.upload'))`, op.ID, task.ID).Scan(&pending); err != nil {
		return failResult(r, "storage", err.Error())
	}
	if pending > 0 {
		return failResult(r, "stop_unconfirmed", "pause intent is stored; an older native write has not settled")
	}
	latest.Paused, latest.Version = true, task.Version
	if latest.Execution == "running" || latest.Execution == "stopping" || latest.Execution == "starting" || latest.Execution == "unknown" {
		latest.Execution = "stopped"
	}
	if err := s.saveTask(ctx, latest); err != nil {
		return failResult(r, "storage", err.Error())
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE executor_tasks SET occupied=0 WHERE task_id=?`, task.ID); err != nil {
		return failResult(r, "storage", err.Error())
	}
	if interruptErr != nil {
		event := protocol.Event{TaskID: task.ID, InstanceID: s.instance, ThreadID: task.ThreadID, TurnID: task.TurnID, Type: "task/stop_verified", Data: mustJSON(map[string]any{"native_status": "idle", "interrupt_error": interruptErr.Error(), "task": latest}), CreatedAt: time.Now().UTC()}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO executor_events(payload) VALUES(?)`, string(mustJSON(event))); err != nil {
			return failResult(r, "storage", err.Error())
		}
	}
	r.Stage, r.Data = "accepted", mustJSON(latest)
	return r
}
func readNativeStatus(ctx context.Context, client *Client, threadID string) (string, error) {
	data, err := client.Call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
	if err != nil {
		return "", err
	}
	var read struct {
		Thread struct {
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(data, &read); err != nil {
		return "", err
	}
	if read.Thread.Status.Type == "" {
		return "", errors.New("native thread response omitted runtime status")
	}
	return read.Thread.Status.Type, nil
}
