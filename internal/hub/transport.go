package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type session struct {
	conn     *websocket.Conn
	binding  Binding
	snapshot protocol.Snapshot
	mu       sync.Mutex
	pending  map[string]chan protocol.Frame
	done     chan struct{}
}

func (h *Hub) Close() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		sessions := make([]*session, 0, len(h.sessions))
		for _, s := range h.sessions {
			sessions = append(sessions, s)
		}
		h.mu.Unlock()
		var err error
		for _, s := range sessions {
			closeErr := s.conn.CloseNow()
			if websocket.CloseStatus(closeErr) != websocket.StatusNormalClosure {
				err = errors.Join(err, closeErr)
			}
		}
		h.closeErr = err
	})
	return h.closeErr
}

func (h *Hub) machine(b Binding) map[string]any {
	h.mu.Lock()
	s := h.sessions[b.ExecutorID]
	h.mu.Unlock()
	if s == nil {
		return map[string]any{"connected": false, "snapshot": publicSnapshot(protocol.Snapshot{Initialized: false, Projects: []protocol.Project{}, Tasks: []protocol.Task{}}, b)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"connected": true, "snapshot": publicSnapshot(s.snapshot, b)}
}
func (h *Hub) WebsocketHandler() http.Handler { return http.HandlerFunc(h.executorHTTP) }
func (h *Hub) executorHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/executor" {
		http.NotFound(w, r)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var b Binding
	found := false
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		for _, candidate := range h.cfg.Bindings {
			if subtle.ConstantTimeCompare([]byte(token), []byte(candidate.Token)) == 1 {
				b = candidate
				found = true
				break
			}
		}
	}
	if !found {
		http.Error(w, "executor authentication required", http.StatusUnauthorized)
		return
	}
	h.mu.Lock()
	_, duplicate := h.sessions[b.ExecutorID]
	closed := h.closed
	h.mu.Unlock()
	if closed {
		http.Error(w, "hub is closing", http.StatusServiceUnavailable)
		return
	}
	if duplicate {
		http.Error(w, "executor already connected", http.StatusConflict)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)
	ctx := r.Context()
	helloCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var hello protocol.Frame
	err = wsjson.Read(helloCtx, conn, &hello)
	cancel()
	if err != nil || hello.Version != protocol.Version || hello.Type != "hello" || hello.Snapshot == nil || hello.Snapshot.InstanceID == "" {
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid executor hello")
		return
	}
	for _, t := range hello.Snapshot.Tasks {
		if t.Scope != b.scope() {
			_ = conn.Close(websocket.StatusPolicyViolation, "task scope mismatch")
			return
		}
	}
	s := &session{conn: conn, binding: b, snapshot: *hello.Snapshot, pending: map[string]chan protocol.Frame{}, done: make(chan struct{})}
	h.mu.Lock()
	if h.closed || h.sessions[b.ExecutorID] != nil {
		h.mu.Unlock()
		_ = conn.Close(websocket.StatusPolicyViolation, "executor already connected")
		return
	}
	h.sessions[b.ExecutorID] = s
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.sessions[b.ExecutorID] == s {
			delete(h.sessions, b.ExecutorID)
		}
		h.mu.Unlock()
		close(s.done)
	}()
	if err = h.applySnapshot(ctx, b, *hello.Snapshot); err != nil {
		return
	}
	var last int64
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		err := tx.Get("cursor", owner(b.scope()), "events", &last)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return
	}
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "ack", AckSequence: last}); err != nil {
		return
	}
	go func() {
		if err := h.resumeStops(ctx, b); err != nil {
			_ = conn.Close(websocket.StatusInternalError, "pause reconciliation failed")
		}
	}()
	for {
		var frame protocol.Frame
		if err = wsjson.Read(ctx, conn, &frame); err != nil {
			return
		}
		if frame.Version != protocol.Version {
			_ = conn.Close(websocket.StatusPolicyViolation, "protocol version mismatch")
			return
		}
		switch frame.Type {
		case "snapshot":
			if frame.Snapshot == nil || frame.Snapshot.InstanceID != s.snapshot.InstanceID {
				return
			}
			for _, t := range frame.Snapshot.Tasks {
				if t.Scope != b.scope() {
					return
				}
			}
			s.mu.Lock()
			s.snapshot = *frame.Snapshot
			s.mu.Unlock()
			if err = h.applySnapshot(ctx, b, *frame.Snapshot); err != nil {
				return
			}
		case "result":
			if frame.Result == nil {
				return
			}
			if err = h.applyResult(ctx, b, *frame.Result); err != nil {
				return
			}
			s.mu.Lock()
			wait := s.pending[frame.ID]
			s.mu.Unlock()
			if wait != nil {
				select {
				case wait <- frame:
				default:
				}
			}
		case "event":
			if frame.Event == nil {
				return
			}
			if err = h.saveEvent(ctx, b, *frame.Event); err != nil {
				return
			}
			if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "ack", AckSequence: frame.Event.Sequence}); err != nil {
				return
			}
		default:
			_ = conn.Close(websocket.StatusPolicyViolation, "unexpected frame")
			return
		}
	}
}
func (h *Hub) dispatch(ctx context.Context, b Binding, op protocol.Operation) (protocol.Result, error) {
	h.mu.Lock()
	s := h.sessions[b.ExecutorID]
	h.mu.Unlock()
	if s == nil {
		return protocol.Result{}, failure("offline", "bound executor is offline")
	}
	requestID, err := randomID("rpc_")
	if err != nil {
		return protocol.Result{}, err
	}
	wait := make(chan protocol.Frame, 1)
	s.mu.Lock()
	s.pending[requestID] = wait
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, requestID); s.mu.Unlock() }()
	deadline, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := wsjson.Write(deadline, s.conn, protocol.Frame{Version: protocol.Version, Type: "operation", ID: requestID, Operation: &op}); err != nil {
		return protocol.Result{}, failure("unknown", "executor submission outcome is unknown")
	}
	select {
	case frame := <-wait:
		if frame.Result.OperationID != op.ID || frame.Result.TaskID != op.TaskID {
			return protocol.Result{}, failure("protocol_error", "executor result does not match operation")
		}
		return *frame.Result, nil
	case <-s.done:
		return protocol.Result{}, failure("unknown", "executor disconnected before receipt")
	case <-deadline.Done():
		return protocol.Result{}, failure("unknown", "executor receipt deadline elapsed")
	}
}
func (h *Hub) submit(ctx context.Context, b Binding, id string) (json.RawMessage, error) {
	var o operationRecord
	var send bool
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		o, err = loadOperation(tx, b, id)
		if err != nil {
			return err
		}
		if o.Stage != "prepared" {
			return nil
		}
		if err := checkAPIAttempt(tx, b, o.Source); err != nil {
			return err
		}
		t, err := loadTask(tx, b, o.TaskID)
		if err != nil {
			return err
		}
		if t.Version != o.TaskVersion {
			return failure("stale_version", "task changed after operation preparation")
		}
		if t.Paused && o.Action != "task.stop" && o.Action != "task.continue" {
			return failure("paused", "task is paused")
		}
		h.mu.Lock()
		online := h.sessions[b.ExecutorID] != nil
		h.mu.Unlock()
		if !online {
			return failure("offline", "bound executor is offline")
		}
		if t.Paused && o.Action == "task.continue" {
			if o.Source.Kind != "gateway" {
				return failure("paused", "only a gateway user may resume a paused task")
			}
			t.Version++
			t.Paused = false
			t.Execution = "starting"
			t.Waiting = ""
			t.UpdatedAt = time.Now().UTC()
			o.TaskVersion = t.Version
			if err = tx.Put("task", owner(b.scope()), t.ID, t); err != nil {
				return err
			}
		}
		o.Stage = "dispatched"
		o.UpdatedAt = time.Now().UTC()
		send = true
		return tx.Put("operation", owner(b.scope()), id, o)
	})
	if err != nil {
		return nil, err
	}
	if send {
		result, dispatchErr := h.dispatch(ctx, b, o.Operation)
		if dispatchErr != nil {
			result = protocol.Result{OperationID: o.ID, TaskID: o.TaskID, Stage: "unknown", Error: &protocol.Error{Code: "unknown", Message: dispatchErr.Error()}}
			if err = h.applyResult(context.WithoutCancel(ctx), b, result); err != nil {
				return nil, err
			}
		}
		_ = result
		err = h.db.Update(ctx, func(tx *store.Tx) error { var loadErr error; o, loadErr = loadOperation(tx, b, id); return loadErr })
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(publicOperation(o))
}
func (h *Hub) applyResult(ctx context.Context, b Binding, result protocol.Result) error {
	switch result.Stage {
	case "accepted", "failed", "unknown":
	default:
		return failure("invalid_result", "executor receipt has an unsupported stage")
	}
	return h.db.Update(ctx, func(tx *store.Tx) error {
		o, err := loadOperation(tx, b, result.OperationID)
		if err != nil {
			var p *protocol.Error
			if errors.As(err, &p) && p.Code == "not_found" {
				return nil
			}
			return err
		}
		if o.TaskID != result.TaskID {
			return failure("scope_mismatch", "result task mismatch")
		}
		if result.Stage == "unknown" && (o.Stage == "accepted" || o.Stage == "failed") {
			return nil
		}
		o.Stage = result.Stage
		o.Result = &result
		o.ReconciliationError = nil
		o.UpdatedAt = time.Now().UTC()
		if err = tx.Put("operation", owner(b.scope()), o.ID, o); err != nil {
			return err
		}
		task, err := loadTask(tx, b, o.TaskID)
		if err != nil {
			return err
		}
		if o.TaskVersion != task.Version {
			return nil
		}
		if result.ThreadID != "" {
			task.ThreadID = result.ThreadID
		}
		previousTurn := task.TurnID
		if result.TurnID != "" {
			task.TurnID = result.TurnID
		}
		var native protocol.Task
		if len(result.Data) > 0 && json.Unmarshal(result.Data, &native) == nil && native.ID != "" {
			if native.ID != task.ID {
				return failure("scope_mismatch", "native receipt task mismatch")
			}
			if native.Scope != b.scope() {
				return failure("scope_mismatch", "native task scope mismatch")
			}
			if native.Version != task.Version {
				return nil
			}
			terminal := task.Execution == "completed" || task.Execution == "failed" || task.Execution == "interrupted"
			if native.Execution != "" && (!terminal || previousTurn != native.TurnID || (native.Execution != "running" && native.Execution != "starting" && native.Execution != "unknown")) {
				task.Execution = native.Execution
			}
			task.Waiting = native.Waiting
			if len(native.Artifacts) > 0 {
				task.Artifacts = native.Artifacts
			}
		}
		if result.Stage == "unknown" {
			if task.Execution != "completed" && task.Execution != "failed" && task.Execution != "interrupted" {
				task.Execution = "unknown"
			}
		}
		task.UpdatedAt = time.Now().UTC()
		return tx.Put("task", owner(b.scope()), task.ID, task)
	})
}
func (h *Hub) saveEvent(ctx context.Context, b Binding, event protocol.Event) error {
	return h.db.Update(ctx, func(tx *store.Tx) error {
		if event.Sequence < 1 {
			return failure("invalid_event", "event sequence must be positive")
		}
		task, err := loadTask(tx, b, event.TaskID)
		if err != nil {
			return err
		}
		key := strconv.FormatInt(event.Sequence, 10)
		var old protocol.Event
		err = tx.Get("event", owner(b.scope()), key, &old)
		if err == nil {
			if !reflect.DeepEqual(old, event) {
				return failure("event_conflict", "event sequence was reused with changed content")
			}
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		var last int64
		if err = tx.Get("cursor", owner(b.scope()), "events", &last); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if event.Sequence != last+1 {
			return failure("event_gap", "event sequence must follow the committed cursor")
		}
		if err = tx.Put("event", owner(b.scope()), key, event); err != nil {
			return err
		}
		var params struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
			RequestID string        `json:"request_id"`
			Task      protocol.Task `json:"task"`
		}
		if err := json.Unmarshal(event.Data, &params); err != nil {
			return err
		}
		if event.Type == "artifact/available" || event.Type == "artifact/failed" {
			if err := h.saveArtifact(tx, b, &task, event); err != nil {
				return err
			}
		}
		if task.ThreadID == "" && event.ThreadID != "" {
			task.ThreadID = event.ThreadID
		}
		if event.TurnID != "" {
			task.TurnID = event.TurnID
		}
		switch event.Type {
		case "task/native_exited":
			if params.Task.ID == task.ID && params.Task.Version == task.Version {
				task.Execution = "unknown"
				task.Waiting = ""
			}
		case "turn/started":
			task.Execution = "running"
			task.TurnID = params.Turn.ID
		case "turn/completed":
			switch params.Turn.Status {
			case "completed", "failed", "interrupted":
				task.Execution = params.Turn.Status
				task.Waiting = ""
			default:
				task.Execution = "unknown"
			}
		default:
			if params.RequestID != "" {
				task.Waiting = params.RequestID
			}
		}
		task.UpdatedAt = time.Now().UTC()
		task.ProgressAt = task.UpdatedAt
		if event.Type == "turn/started" || event.Type == "turn/completed" || event.Type == "task/native_exited" {
			task.NextReconcileAt = time.Time{}
		}
		if err := tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
			return err
		}
		if err := h.createFollowup(tx, b, task, event); err != nil {
			return err
		}
		if err := queueRequestNotice(tx, b, task, event, params.RequestID); err != nil {
			return err
		}
		return tx.Put("cursor", owner(b.scope()), "events", event.Sequence)
	})
}
