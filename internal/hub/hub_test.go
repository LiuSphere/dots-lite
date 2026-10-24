package hub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dots-lite/internal/hub"
	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestToolCallsRequireRegisteredMatchingSource(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h, err := hub.New(hub.Config{Bindings: []hub.Binding{{UID: 1001, UserID: "a", ProfileID: "a", ExecutorID: "pc-a", Token: "token-a", ProjectIDs: []string{"demo"}, Sources: []hub.AllowedSource{{Platform: "test", SenderID: "sender-a", ChatID: "chat-a"}}}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"query":"machine"}`)
	if _, err := h.Call(context.Background(), 1001, protocol.ToolRequest{Tool: "duet_query", Arguments: args, ContextRef: "invented"}); err == nil {
		t.Fatal("unregistered model context was accepted")
	}
	inv := protocol.Invocation{Tool: "duet_query", Arguments: args, Source: protocol.Source{Kind: "gateway", Platform: "test", SenderID: "sender-a", ChatID: "chat-a", SessionID: "session-a"}}
	ref, err := h.RegisterInvocation(context.Background(), 1001, inv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(context.Background(), 1002, protocol.ToolRequest{Tool: inv.Tool, Arguments: args, ContextRef: ref}); err == nil {
		t.Fatal("other UID used context reference")
	}
	if _, err := h.Call(context.Background(), 1001, protocol.ToolRequest{Tool: inv.Tool, Arguments: json.RawMessage(`{"query":"tasks"}`), ContextRef: ref}); err == nil {
		t.Fatal("changed arguments used context reference")
	}
	result, err := h.Call(context.Background(), 1001, protocol.ToolRequest{Tool: inv.Tool, Arguments: args, ContextRef: ref})
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Online bool `json:"online"`
	}
	if err := json.Unmarshal(result, &status); err != nil {
		t.Fatal(err)
	}
	if status.Online {
		t.Fatal("offline executor was reported online")
	}
}

func TestRootUIDCannotBeConfiguredAsProfile(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := testConfig()
	cfg.Bindings[0].UID = 0
	if _, err := hub.New(cfg, db); err == nil {
		t.Fatal("root UID was accepted as profile")
	}
}

func TestInvocationParametersPreserveLargeIntegersAndRejectTrailingJSON(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	source := protocol.Source{Kind: "gateway", Platform: "test", SenderID: "sender-a", ChatID: "chat-a", SessionID: "session-a"}
	if _, err = h.RegisterInvocation(context.Background(), 1001, protocol.Invocation{Tool: "duet_query", Arguments: json.RawMessage(`{"query":"machine"} {"query":"tasks"}`), Source: source}); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
	ref, err := h.RegisterInvocation(context.Background(), 1001, protocol.Invocation{Tool: "duet_query", Arguments: json.RawMessage(`{"query":"machine","offset":9007199254740992}`), Source: source})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Call(context.Background(), 1001, protocol.ToolRequest{Tool: "duet_query", Arguments: json.RawMessage(`{"query":"machine","offset":9007199254740993}`), ContextRef: ref})
	if err == nil {
		t.Fatal("changed integer bypassed exact invocation binding")
	}
}

func TestHTTPIdentityCannotBeSuppliedByHeader(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/invocations", strings.NewReader(`{"tool":"duet_query","arguments":{"query":"machine"},"source":{"kind":"gateway","platform":"test","sender_id":"sender-a","chat_id":"chat-a","session_id":"session-a"}}`))
	r.Header.Set("X-UID", "1001")
	w := httptest.NewRecorder()
	h.APIHandler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unverified peer status = %d", w.Code)
	}
}

func TestOfflineStopPersistsPauseAndInvalidatesPreparedWrites(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	prepared := call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"make report"}`)
	var ids struct {
		TaskID      string `json:"task_id"`
		OperationID string `json:"operation_id"`
	}
	_ = json.Unmarshal(prepared, &ids)
	staleRecord := registered(t, h, 1001, "duet_record", `{"task_id":"`+ids.TaskID+`","kind":"verification","status":"passed","evidence":{"checks":[{"observed":"old result"}]}}`)
	stopped := call(t, h, 1001, "duet_stop", `{"task_id":"`+ids.TaskID+`"}`)
	var stop struct {
		PauseRecorded bool `json:"pause_recorded"`
		StopConfirmed bool `json:"stop_confirmed"`
	}
	_ = json.Unmarshal(stopped, &stop)
	if !stop.PauseRecorded || stop.StopConfirmed {
		t.Fatalf("offline stop facts incorrect: %s", stopped)
	}
	taskJSON := call(t, h, 1001, "duet_query", `{"query":"task","task_id":"`+ids.TaskID+`"}`)
	var task protocol.Task
	_ = json.Unmarshal(taskJSON, &task)
	if !task.Paused || task.Version != 2 || task.Execution != "pause_requested" {
		t.Fatalf("pause not durable: %s", taskJSON)
	}
	_, err = h.Call(context.Background(), 1001, registered(t, h, 1001, "duet_submit", `{"operation_id":"`+ids.OperationID+`"}`))
	if err == nil {
		t.Fatal("old version write was accepted after stop")
	}
	if _, err = h.Call(context.Background(), 1001, staleRecord); err == nil {
		t.Fatal("gateway reference from before stop recorded stale verification")
	}
	call(t, h, 1001, "duet_record", `{"task_id":"`+ids.TaskID+`","kind":"verification","status":"pending","evidence":{"reason":"inspect paused task"}}`)
}

func TestEmptyResultEvidenceCannotMarkVerificationOrDeliveryComplete(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	prepared := call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"make report"}`)
	var ids struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(prepared, &ids)
	for _, body := range []string{`"kind":"verification","status":"passed","evidence":{"checks":[null]}`, `"kind":"delivery","status":"delivered","evidence":{"receipt":null}`} {
		raw := `{"task_id":"` + ids.TaskID + `",` + body + `}`
		if _, err = h.Call(context.Background(), 1001, registered(t, h, 1001, "duet_record", raw)); err == nil {
			t.Fatalf("empty result evidence accepted: %s", raw)
		}
	}
	valid := `{"task_id":"` + ids.TaskID + `","kind":"verification","status":"passed","evidence":{"checks":[{"command":"go test","exit_code":0,"output_ref":"observed-test-output"}]}}`
	if _, err = h.Call(context.Background(), 1001, registered(t, h, 1001, "duet_record", valid)); err != nil {
		t.Fatal(err)
	}
}

func TestStopConfirmationUsesVersionedNativePauseReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, execution string
		paused          bool
		version         int64
		confirmed       bool
	}{
		{"completed", "completed", true, 2, true},
		{"failed", "failed", true, 2, true},
		{"stopped", "stopped", true, 2, true},
		{"unpaused", "completed", false, 2, false},
		{"old_version", "completed", true, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			h, err := hub.New(testConfig(), db)
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(h.WebsocketHandler())
			defer srv.Close()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/executor", &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer token-a"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "hello", Snapshot: &protocol.Snapshot{InstanceID: "stop-instance", Initialized: true}}); err != nil {
				t.Fatal(err)
			}
			var ack protocol.Frame
			if err = wsjson.Read(ctx, conn, &ack); err != nil {
				t.Fatal(err)
			}
			var ids struct {
				TaskID string `json:"task_id"`
			}
			if err = json.Unmarshal(call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"finish then pause"}`), &ids); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				var frame protocol.Frame
				if err := wsjson.Read(ctx, conn, &frame); err != nil {
					done <- err
					return
				}
				native, _ := json.Marshal(protocol.Task{ID: ids.TaskID, Scope: protocol.Scope{UserID: "a", ProfileID: "a", ExecutorID: "pc-a"}, Version: tc.version, Paused: tc.paused, Execution: tc.execution})
				done <- wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: frame.ID, Result: &protocol.Result{OperationID: frame.Operation.ID, TaskID: ids.TaskID, Stage: "accepted", Data: native}})
			}()
			stopped := call(t, h, 1001, "duet_stop", `{"task_id":"`+ids.TaskID+`"}`)
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			var confirmation struct {
				Confirmed bool `json:"stop_confirmed"`
			}
			if err = json.Unmarshal(stopped, &confirmation); err != nil {
				t.Fatal(err)
			}
			if confirmation.Confirmed != tc.confirmed {
				t.Fatalf("native pause confirmation incorrect: %s", stopped)
			}
		})
	}
}

func TestTerminalEventBeforeReceiptDoesNotRegressTaskToRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.WebsocketHandler())
	defer srv.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/executor", &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer token-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "hello", Snapshot: &protocol.Snapshot{InstanceID: "fast-instance", Initialized: true}}); err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	prepared := call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"fast task"}`)
	var ids struct {
		TaskID      string `json:"task_id"`
		OperationID string `json:"operation_id"`
	}
	_ = json.Unmarshal(prepared, &ids)
	done := make(chan error, 1)
	go func() {
		var operation protocol.Frame
		if err := wsjson.Read(ctx, conn, &operation); err != nil {
			done <- err
			return
		}
		event := protocol.Event{Sequence: 1, InstanceID: "fast-instance", TaskID: ids.TaskID, ThreadID: "fast-thread", TurnID: "fast-turn", Type: "turn/completed", Data: json.RawMessage(`{"threadId":"fast-thread","turn":{"id":"fast-turn","status":"completed"}}`), CreatedAt: time.Now()}
		if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &event}); err != nil {
			done <- err
			return
		}
		if err := wsjson.Read(ctx, conn, &ack); err != nil {
			done <- err
			return
		}
		data, _ := json.Marshal(protocol.Task{ID: ids.TaskID, Scope: protocol.Scope{UserID: "a", ProfileID: "a", ExecutorID: "pc-a"}, ThreadID: "fast-thread", TurnID: "fast-turn", Execution: "running", Version: 1})
		done <- wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: operation.ID, Result: &protocol.Result{OperationID: ids.OperationID, TaskID: ids.TaskID, Stage: "accepted", ThreadID: "fast-thread", TurnID: "fast-turn", Data: data}})
	}()
	call(t, h, 1001, "duet_submit", `{"operation_id":"`+ids.OperationID+`"}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	taskJSON := call(t, h, 1001, "duet_query", `{"query":"task","task_id":"`+ids.TaskID+`"}`)
	var task protocol.Task
	_ = json.Unmarshal(taskJSON, &task)
	if task.Execution != "completed" || task.ThreadID != "fast-thread" || task.TurnID != "fast-turn" {
		t.Fatalf("receipt regressed terminal event: %s", taskJSON)
	}
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: "late-timeout", Result: &protocol.Result{OperationID: ids.OperationID, TaskID: ids.TaskID, Stage: "unknown", Error: &protocol.Error{Code: "unknown", Message: "late timeout"}}}); err != nil {
		t.Fatal(err)
	}
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &protocol.Event{Sequence: 2, TaskID: ids.TaskID, InstanceID: "fast-instance", ThreadID: "fast-thread", TurnID: "fast-turn", Type: "serverRequest/resolved", Data: json.RawMessage(`{}`), CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	taskJSON = call(t, h, 1001, "duet_query", `{"query":"task","task_id":"`+ids.TaskID+`"}`)
	if err = json.Unmarshal(taskJSON, &task); err != nil {
		t.Fatal(err)
	}
	if task.Execution != "completed" {
		t.Fatalf("late uncertainty replaced observed completion: %s", taskJSON)
	}
}

func TestAutomaticArtifactTransferRequiresMatchingScopeAndReceipt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.WebsocketHandler())
	defer srv.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/executor", &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer token-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "hello", Snapshot: &protocol.Snapshot{InstanceID: "artifact-instance", Initialized: true}}); err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	prepared := call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"make report","artifacts":["out.txt"]}`)
	var ids struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(prepared, &ids)
	file := json.RawMessage(`{"path":"out.txt","task_version":1,"data_base64":"YWJj","sha256":"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad","size":3,"returned_bytes":3,"offset":0,"truncated":false}`)
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &protocol.Event{Sequence: 1, TaskID: ids.TaskID, InstanceID: "artifact-instance", Type: "artifact/available", Data: file, CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	other, err := h.NextArtifact(ctx, 1002)
	if err != nil {
		t.Fatal(err)
	}
	if string(other) != "null" {
		t.Fatal("other UID acquired artifact bytes")
	}
	payload, err := h.NextArtifact(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	var artifact struct {
		ID   string          `json:"id"`
		File json.RawMessage `json:"file"`
	}
	_ = json.Unmarshal(payload, &artifact)
	if artifact.ID == "" || !strings.Contains(string(artifact.File), "YWJj") {
		t.Fatalf("file data was not acquired: %s", payload)
	}
	if _, err = h.RecordArtifactAcquisition(ctx, 1001, artifact.ID, json.RawMessage(`{"acquired":true,"local_path":"/private/out.txt","sha256":"wrong","size":3,"returned_bytes":3,"offset":0}`)); err == nil {
		t.Fatal("wrong receipt hash was accepted")
	}
	receipt := json.RawMessage(`{"acquired":true,"local_path":"/private/out.txt","sha256":"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad","size":3,"returned_bytes":3,"offset":0}`)
	first, err := h.RecordArtifactAcquisition(ctx, 1001, artifact.ID, receipt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.RecordArtifactAcquisition(ctx, 1001, artifact.ID, receipt)
	if err != nil || string(first) != string(second) {
		t.Fatalf("acquisition receipt not idempotent: %s %s %v", first, second, err)
	}
	taskJSON := call(t, h, 1001, "duet_query", `{"query":"task","task_id":"`+ids.TaskID+`"}`)
	var task protocol.Task
	_ = json.Unmarshal(taskJSON, &task)
	if task.Delivery != "acquired" {
		t.Fatalf("acquisition not distinct from delivery: %s", taskJSON)
	}
	// A delayed artifact from before a stop must not replace the current outcome.
	go func() {
		var stop protocol.Frame
		if wsjson.Read(ctx, conn, &stop) == nil {
			_ = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: stop.ID, Result: &protocol.Result{OperationID: stop.Operation.ID, TaskID: ids.TaskID, Stage: "accepted"}})
		}
	}()
	call(t, h, 1001, "duet_stop", `{"task_id":"`+ids.TaskID+`"}`)
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &protocol.Event{Sequence: 2, TaskID: ids.TaskID, InstanceID: "artifact-instance", Type: "artifact/available", Data: file, CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	taskJSON = call(t, h, 1001, "duet_query", `{"query":"task","task_id":"`+ids.TaskID+`"}`)
	if err = json.Unmarshal(taskJSON, &task); err != nil {
		t.Fatal(err)
	}
	if task.Delivery != "acquired" {
		t.Fatalf("old artifact changed current delivery: %s", taskJSON)
	}
	continued := call(t, h, 1001, "duet_prepare", `{"action":"task.continue","task_id":"`+ids.TaskID+`","input":"resume authorized task"}`)
	var next struct {
		OperationID string `json:"operation_id"`
	}
	if err = json.Unmarshal(continued, &next); err != nil {
		t.Fatal(err)
	}
	versions := make(chan int64, 1)
	go func() {
		var continuation protocol.Frame
		if wsjson.Read(ctx, conn, &continuation) != nil {
			versions <- -1
			return
		}
		versions <- continuation.Operation.TaskVersion
		data, _ := json.Marshal(protocol.Task{ID: ids.TaskID, Scope: protocol.Scope{UserID: "a", ProfileID: "a", ExecutorID: "pc-a"}, Version: continuation.Operation.TaskVersion, Execution: "running", ThreadID: "thread-a", TurnID: "turn-b"})
		_ = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: continuation.ID, Result: &protocol.Result{OperationID: continuation.Operation.ID, TaskID: ids.TaskID, Stage: "accepted", ThreadID: "thread-a", TurnID: "turn-b", Data: data}})
	}()
	call(t, h, 1001, "duet_submit", `{"operation_id":"`+next.OperationID+`"}`)
	taskJSON = call(t, h, 1001, "duet_query", `{"query":"task","task_id":"`+ids.TaskID+`"}`)
	if err = json.Unmarshal(taskJSON, &task); err != nil {
		t.Fatal(err)
	}
	if version := <-versions; task.Paused || task.Version != 3 || version != 3 {
		t.Fatalf("explicit resume did not advance version and unpause: %s; sent version %d", taskJSON, version)
	}
}

func TestPreparedTaskSurvivesRestartAndStaysInsideItsScope(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.db")
	cfg := testConfig()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h, err := hub.New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	created := call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"write a report","name":"report"}`)
	var prepared struct {
		TaskID      string `json:"task_id"`
		OperationID string `json:"operation_id"`
		Stage       string `json:"stage"`
	}
	if err := json.Unmarshal(created, &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.TaskID == "" || prepared.OperationID == "" || prepared.Stage != "prepared" {
		t.Fatalf("not a prepared task: %s", created)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h, err = hub.New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	task := call(t, h, 1001, "duet_query", `{"query":"task","task_id":"`+prepared.TaskID+`"}`)
	var recovered protocol.Task
	if err := json.Unmarshal(task, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.Objective != "write a report" || recovered.Execution != "prepared" || recovered.Verification != "pending" || recovered.Delivery != "pending" {
		t.Fatalf("task changed after restart: %s", task)
	}
	op := call(t, h, 1001, "duet_query", `{"query":"operation","operation_id":"`+prepared.OperationID+`"}`)
	var operation struct {
		Stage string `json:"stage"`
	}
	_ = json.Unmarshal(op, &operation)
	if operation.Stage != "prepared" {
		t.Fatalf("operation lost: %s", op)
	}
	req := registered(t, h, 1002, "duet_query", `{"query":"task","task_id":"`+prepared.TaskID+`"}`)
	if _, err = h.Call(ctx, 1002, req); err == nil {
		t.Fatal("other scope read task")
	}
}

func TestEventReplayIsIdempotentButGapsAndChangedContentAreRejected(t *testing.T) {
	for _, variant := range []string{"changed", "gap"} {
		t.Run(variant, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			h, err := hub.New(testConfig(), db)
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(h.WebsocketHandler())
			defer srv.Close()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/executor", &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer token-a"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "hello", Snapshot: &protocol.Snapshot{InstanceID: "event-instance", Initialized: true}}); err != nil {
				t.Fatal(err)
			}
			var ack protocol.Frame
			if err = wsjson.Read(ctx, conn, &ack); err != nil {
				t.Fatal(err)
			}
			var ids struct {
				TaskID string `json:"task_id"`
			}
			if err = json.Unmarshal(call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"inspect events"}`), &ids); err != nil {
				t.Fatal(err)
			}
			event := protocol.Event{Sequence: 1, TaskID: ids.TaskID, InstanceID: "event-instance", Type: "turn/completed", Data: json.RawMessage(`{"turn":{"id":"turn-a","status":"completed"}}`), CreatedAt: time.Now()}
			for i := 0; i < 2; i++ {
				if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &event}); err != nil {
					t.Fatal(err)
				}
				if err = wsjson.Read(ctx, conn, &ack); err != nil || ack.AckSequence != 1 {
					t.Fatalf("durable replay: %+v %v", ack, err)
				}
			}
			if variant == "gap" {
				event.Sequence = 3
			} else {
				event.Type = "turn/started"
			}
			if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &event}); err != nil {
				t.Fatal(err)
			}
			if err = wsjson.Read(ctx, conn, &ack); err == nil {
				t.Fatalf("invalid event acknowledged: %+v", ack)
			}
			var task protocol.Task
			if err = json.Unmarshal(call(t, h, 1001, "duet_query", `{"query":"task","task_id":"`+ids.TaskID+`"}`), &task); err != nil {
				t.Fatal(err)
			}
			if task.Execution != "completed" {
				t.Fatalf("invalid event changed task: %+v", task)
			}
		})
	}
}

func TestUploadUsesPreparedTaskScopeAndRejectsChangedContent(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	var ids struct {
		TaskID string `json:"task_id"`
	}
	if err = json.Unmarshal(call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"read attachment"}`), &ids); err != nil {
		t.Fatal(err)
	}
	valid := `{"action":"files.upload","task_id":"` + ids.TaskID + `","upload":{"name":"input.txt","data_base64":"YWJj","size":3,"sha256":"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}}`
	for _, invalid := range []string{
		strings.Replace(valid, `"input.txt"`, `"../input.txt"`, 1),
		strings.Replace(valid, `"input.txt"`, `"..\\input.txt"`, 1),
		strings.Replace(valid, `"size":3`, `"size":4`, 1),
		strings.Replace(valid, `"YWJj"`, `"YWJk"`, 1),
	} {
		if _, err = h.Call(context.Background(), 1001, registered(t, h, 1001, "duet_prepare", invalid)); err == nil {
			t.Fatalf("invalid upload prepared: %s", invalid)
		}
	}
	prepared := call(t, h, 1001, "duet_prepare", valid)
	var upload struct {
		OperationID string `json:"operation_id"`
	}
	if err = json.Unmarshal(prepared, &upload); err != nil {
		t.Fatal(err)
	}
	read := call(t, h, 1001, "duet_query", `{"query":"operation","operation_id":"`+upload.OperationID+`"}`)
	if strings.Contains(string(read), `"data_base64"`) || !strings.Contains(string(read), `"size":3`) || !strings.Contains(string(read), `"stage":"prepared"`) {
		t.Fatalf("public upload operation did not preserve metadata without bytes: %s", read)
	}
	if _, err = h.Call(context.Background(), 1002, registered(t, h, 1002, "duet_query", `{"query":"operation","operation_id":"`+upload.OperationID+`"}`)); err == nil {
		t.Fatal("other scope read prepared attachment")
	}
	call(t, h, 1001, "duet_stop", `{"task_id":"`+ids.TaskID+`"}`)
	if _, err = h.Call(context.Background(), 1001, registered(t, h, 1001, "duet_submit", `{"operation_id":"`+upload.OperationID+`"}`)); err == nil {
		t.Fatal("old upload submitted after stop")
	}
}

func TestReconcileFindsMissingTerminalEventWithoutReplayingWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.WebsocketHandler())
	defer srv.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/executor", &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer token-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "hello", Snapshot: &protocol.Snapshot{InstanceID: "reconcile-instance", Initialized: true}}); err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	var ids struct {
		TaskID      string `json:"task_id"`
		OperationID string `json:"operation_id"`
	}
	if err = json.Unmarshal(call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"finish report"}`), &ids); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for _, action := range []string{"task.create", "task.read", "goal.get"} {
			var frame protocol.Frame
			if err := wsjson.Read(ctx, conn, &frame); err != nil {
				done <- err
				return
			}
			if frame.Operation == nil || frame.Operation.Action != action {
				done <- fmt.Errorf("unexpected reconciliation action: %+v", frame.Operation)
				return
			}
			native := protocol.Task{ID: ids.TaskID, Scope: protocol.Scope{UserID: "a", ProfileID: "a", ExecutorID: "pc-a"}, Version: 1, ThreadID: "thread-a", TurnID: "turn-a", Execution: "running"}
			var data json.RawMessage
			switch action {
			case "task.create":
				data, _ = json.Marshal(native)
			case "task.read":
				native.Execution = "completed"
				data, _ = json.Marshal(map[string]any{"task": native, "native": map[string]any{"thread": map[string]any{"status": map[string]string{"type": "idle"}}}, "requests": []any{}})
			case "goal.get":
				data = json.RawMessage(`{"goal":null}`)
			}
			if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: frame.ID, Result: &protocol.Result{OperationID: frame.Operation.ID, TaskID: ids.TaskID, Stage: "accepted", ThreadID: "thread-a", TurnID: "turn-a", Data: data}}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	call(t, h, 1001, "duet_submit", `{"operation_id":"`+ids.OperationID+`"}`)
	if err = h.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var task protocol.Task
	if err = json.Unmarshal(call(t, h, 1001, "duet_query", `{"query":"task","task_id":"`+ids.TaskID+`"}`), &task); err != nil {
		t.Fatal(err)
	}
	if task.Execution != "completed" {
		t.Fatalf("missed terminal state was not recovered: %+v", task)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	next, err := h.NextFollowup(ctx, 1001)
	if err != nil || string(next) == "null" {
		t.Fatalf("missed event did not schedule verification: %s %v", next, err)
	}
	if err = h.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var follows []json.RawMessage
	if err = json.Unmarshal(call(t, h, 1001, "duet_query", `{"query":"followups"}`), &follows); err != nil {
		t.Fatal(err)
	}
	if len(follows) != 1 {
		t.Fatalf("same state created duplicate followup: %d", len(follows))
	}
}

func TestSubmitDispatchesOnceAndKeepsNativeExecutionSeparateFromVerification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.WebsocketHandler())
	defer srv.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/executor", &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer token-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "hello", Snapshot: &protocol.Snapshot{InstanceID: "instance-a", Initialized: true, Projects: []protocol.Project{{ID: "demo", Available: true}}}}); err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	prepared := call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"make report"}`)
	var ids struct {
		TaskID      string `json:"task_id"`
		OperationID string `json:"operation_id"`
	}
	_ = json.Unmarshal(prepared, &ids)
	peerDone := make(chan error, 1)
	go func() {
		var f protocol.Frame
		if err := wsjson.Read(ctx, conn, &f); err != nil {
			peerDone <- err
			return
		}
		if f.Type != "operation" || f.Operation.ID != ids.OperationID {
			peerDone <- fmt.Errorf("unexpected operation: %+v", f)
			return
		}
		task := protocol.Task{ID: ids.TaskID, Scope: protocol.Scope{UserID: "a", ProfileID: "a", ExecutorID: "pc-a"}, ProjectID: "demo", ThreadID: "thread-a", TurnID: "turn-a", Version: 1, Execution: "running"}
		data, _ := json.Marshal(task)
		peerDone <- wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: f.ID, Result: &protocol.Result{OperationID: ids.OperationID, TaskID: ids.TaskID, Stage: "accepted", ThreadID: "thread-a", TurnID: "turn-a", Data: data}})
	}()
	first := call(t, h, 1001, "duet_submit", `{"operation_id":"`+ids.OperationID+`"}`)
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
	second := call(t, h, 1001, "duet_submit", `{"operation_id":"`+ids.OperationID+`"}`)
	if string(first) != string(second) {
		t.Fatalf("same operation changed: %s vs %s", first, second)
	}
	taskJSON := call(t, h, 1001, "duet_query", `{"query":"task","task_id":"`+ids.TaskID+`"}`)
	var task protocol.Task
	_ = json.Unmarshal(taskJSON, &task)
	if task.ThreadID != "thread-a" || task.Execution != "running" || task.Verification != "pending" || task.Delivery != "pending" {
		t.Fatalf("states were conflated: %s", taskJSON)
	}
	if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &protocol.Event{Sequence: 1, TaskID: ids.TaskID, InstanceID: "instance-a", ThreadID: "thread-a", TurnID: "turn-a", Type: "turn/completed", Data: json.RawMessage(`{"threadId":"thread-a","turn":{"id":"turn-a","status":"completed"}}`), CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.AckSequence != 1 {
		t.Fatalf("event not acknowledged: %+v", ack)
	}
	taskJSON = call(t, h, 1001, "duet_query", `{"query":"task","task_id":"`+ids.TaskID+`"}`)
	_ = json.Unmarshal(taskJSON, &task)
	if task.Execution != "completed" || task.Verification != "pending" || task.Delivery != "pending" {
		t.Fatalf("turn completion is not independent: %s", taskJSON)
	}
	ordinary, err := h.NextFollowup(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	if string(ordinary) != "null" {
		t.Fatalf("ordinary turn completion started competing followup: %s", ordinary)
	}
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &protocol.Event{Sequence: 2, TaskID: ids.TaskID, InstanceID: "instance-a", ThreadID: "thread-a", TurnID: "turn-a", Type: "task/quiescent", Data: json.RawMessage(`{"goal_status":"none","native_status":"idle","task":{"task_id":"` + ids.TaskID + `","version":1}}`), CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	followJSON, err := h.NextFollowup(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	var follow struct {
		ID        string         `json:"id"`
		SessionID string         `json:"session_id"`
		TaskID    string         `json:"task_id"`
		Request   map[string]any `json:"request"`
	}
	if err = json.Unmarshal(followJSON, &follow); err != nil {
		t.Fatal(err)
	}
	if follow.TaskID != ids.TaskID || follow.SessionID == "" || follow.Request["session_id"] != follow.SessionID {
		t.Fatalf("followup was not prebound: %s", followJSON)
	}
	apiArgs := json.RawMessage(`{"query":"task","task_id":"` + ids.TaskID + `"}`)
	apiRef, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "duet_query", Arguments: apiArgs, Source: protocol.Source{Kind: "api", Platform: "api_server", ChatID: follow.SessionID, SessionID: "physical-session", NativeTaskID: "native-run", ToolCallID: "tool-call"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Call(ctx, 1001, protocol.ToolRequest{Tool: "duet_query", Arguments: apiArgs, ContextRef: apiRef}); err != nil {
		t.Fatal("prebound API could not query its task:", err)
	}
	emptyEvidence := json.RawMessage(`{"task_id":"` + ids.TaskID + `","kind":"followup","status":"handled","evidence":{"actions":[],"verification":{}}}`)
	emptyRef, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "duet_record", Arguments: emptyEvidence, Source: protocol.Source{Kind: "api", Platform: "api_server", ChatID: follow.SessionID, SessionID: "physical-session", NativeTaskID: "native-run", ToolCallID: "record-tool-call"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Call(ctx, 1001, protocol.ToolRequest{Tool: "duet_record", Arguments: emptyEvidence, ContextRef: emptyRef}); err == nil {
		t.Fatal("empty actions and verification marked followup handled")
	}
	for _, status := range []string{"handled", "waiting_user"} {
		evidence := json.RawMessage(`{"task_id":"` + ids.TaskID + `","kind":"followup","status":"` + status + `","evidence":{"actions":["inspected native history"],"verification":{"observed":"report exists"}}}`)
		ref, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "duet_record", Arguments: evidence, Source: protocol.Source{Kind: "api", Platform: "api_server", ChatID: follow.SessionID, SessionID: "physical-session", NativeTaskID: "native-run", ToolCallID: "record-" + status}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = h.Call(ctx, 1001, protocol.ToolRequest{Tool: "duet_record", Arguments: evidence, ContextRef: ref}); err != nil {
			t.Fatal(err)
		}
		run := json.RawMessage(`{"run_id":"run-a","session_id":"` + follow.SessionID + `","status":"completed"}`)
		if _, err = h.SaveFollowupRun(ctx, 1001, follow.ID, run); err != nil {
			t.Fatal(err)
		}
		reported, err := h.ReportFollowup(ctx, 1001, follow.ID, json.RawMessage(`{"run_id":"run-a","status":"completed","evidence":{"output":"native diagnostics"}}`))
		if err != nil {
			t.Fatal(err)
		}
		var saved struct {
			Status         string          `json:"status"`
			Evidence       json.RawMessage `json:"evidence"`
			NativeEvidence json.RawMessage `json:"native_evidence"`
		}
		if err = json.Unmarshal(reported, &saved); err != nil {
			t.Fatal(err)
		}
		want := "handled"
		if status == "waiting_user" {
			want = "blocked"
		}
		if saved.Status != want || !strings.Contains(string(saved.Evidence), "report exists") || !strings.Contains(string(saved.NativeEvidence), "native diagnostics") {
			t.Fatalf("late native report changed business evidence or lost diagnostics: %s", reported)
		}
	}
	readCtx, cancelRead := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelRead()
	var duplicate protocol.Frame
	if err := wsjson.Read(readCtx, conn, &duplicate); err == nil {
		t.Fatalf("operation was dispatched twice: %+v", duplicate)
	}
}

func testConfig() hub.Config {
	return hub.Config{Bindings: []hub.Binding{
		{UID: 1001, UserID: "a", ProfileID: "a", ExecutorID: "pc-a", Token: "token-a", ProjectIDs: []string{"demo"}, Sources: []hub.AllowedSource{{Platform: "test", SenderID: "sender-a", ChatID: "chat-a"}}},
		{UID: 1002, UserID: "b", ProfileID: "b", ExecutorID: "pc-b", Token: "token-b", ProjectIDs: []string{"demo"}, Sources: []hub.AllowedSource{{Platform: "test", SenderID: "sender-b", ChatID: "chat-b"}}},
	}}
}
func registered(t *testing.T, h *hub.Hub, uid int, tool, args string) protocol.ToolRequest {
	t.Helper()
	suffix := "a"
	if uid == 1002 {
		suffix = "b"
	}
	inv := protocol.Invocation{Tool: tool, Arguments: json.RawMessage(args), Source: protocol.Source{Kind: "gateway", Platform: "test", SenderID: "sender-" + suffix, ChatID: "chat-" + suffix, SessionID: "session-" + suffix}}
	ref, err := h.RegisterInvocation(context.Background(), uid, inv)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.ToolRequest{Tool: tool, Arguments: inv.Arguments, ContextRef: ref}
}
func call(t *testing.T, h *hub.Hub, uid int, tool, args string) json.RawMessage {
	t.Helper()
	out, err := h.Call(context.Background(), uid, registered(t, h, uid, tool, args))
	if err != nil {
		t.Fatal(err)
	}
	return out
}
