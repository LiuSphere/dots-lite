package hub_test

import (
	"context"
	"encoding/json"
	"fmt"
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

type followupView struct {
	ID, TaskID, SessionID, IdempotencyKey, RunID, Status string
	Request, Evidence, NativeEvidence                    json.RawMessage
}

func readFollowup(t *testing.T, raw json.RawMessage) followupView {
	t.Helper()
	var value struct {
		ID             string          `json:"id"`
		TaskID         string          `json:"task_id"`
		SessionID      string          `json:"session_id"`
		Key            string          `json:"idempotency_key"`
		RunID          string          `json:"run_id"`
		Status         string          `json:"status"`
		Request        json.RawMessage `json:"request"`
		Evidence       json.RawMessage `json:"evidence"`
		NativeEvidence json.RawMessage `json:"native_evidence"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return followupView{value.ID, value.TaskID, value.SessionID, value.Key, value.RunID, value.Status, value.Request, value.Evidence, value.NativeEvidence}
}

func connectedHubFixture(t *testing.T, budget int) (context.Context, *hub.Hub, *websocket.Conn, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := testConfig()
	cfg.FollowupBudget = budget
	h, err := hub.New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	srv := httptest.NewServer(h.WebsocketHandler())
	t.Cleanup(srv.Close)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/executor", &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer token-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "hello", Snapshot: &protocol.Snapshot{InstanceID: "retry-instance", Initialized: true}}); err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	var ids struct {
		TaskID string `json:"task_id"`
	}
	if err = json.Unmarshal(call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"verify report"}`), &ids); err != nil {
		t.Fatal(err)
	}
	return ctx, h, conn, ids.TaskID
}

func followupFixture(t *testing.T, budget int) (context.Context, *hub.Hub, *websocket.Conn, string) {
	t.Helper()
	ctx, h, conn, taskID := connectedHubFixture(t, budget)
	event := protocol.Event{Sequence: 1, TaskID: taskID, InstanceID: "retry-instance", Type: "task/quiescent", Data: json.RawMessage(`{"goal_status":"none","native_status":"idle","task_version":1}`), CreatedAt: time.Now().UTC()}
	var ack protocol.Frame
	var err error
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &event}); err != nil {
		t.Fatal(err)
	}
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	return ctx, h, conn, taskID
}

func TestHandledUnavailableStateDoesNotSuppressLaterQuiescenceInSameTurn(t *testing.T) {
	ctx, h, conn, taskID := connectedHubFixture(t, 4)
	var ops []struct {
		ID string `json:"operation_id"`
	}
	if err := json.Unmarshal(call(t, h, 1001, "duet_query", `{"query":"operations","task_id":"`+taskID+`"}`), &ops); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	native := protocol.Task{ID: taskID, Scope: protocol.Scope{UserID: "a", ProfileID: "a", ExecutorID: "pc-a"}, Version: 1, ThreadID: "thread-a", TurnID: "turn-a", Execution: "running"}
	go func() {
		var frame protocol.Frame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			done <- err
			return
		}
		data, _ := json.Marshal(native)
		if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: frame.ID, Result: &protocol.Result{OperationID: frame.Operation.ID, TaskID: taskID, Stage: "accepted", Data: data, ThreadID: "thread-a", TurnID: "turn-a"}}); err != nil {
			done <- err
			return
		}
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			done <- err
			return
		}
		done <- wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: frame.ID, Result: &protocol.Result{OperationID: frame.Operation.ID, TaskID: taskID, Stage: "failed", Error: &protocol.Error{Code: "native_unready", Message: "native temporarily unavailable"}}})
	}()
	call(t, h, 1001, "duet_submit", `{"operation_id":"`+ops[0].ID+`"}`)
	if err := h.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	raw, err := h.NextFollowup(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	first := readFollowup(t, raw)
	evidence := json.RawMessage(`{"task_id":"` + taskID + `","kind":"followup","status":"handled","evidence":{"actions":["checked native availability"],"verification":{"observed":"native unavailable"}}}`)
	ref, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "duet_record", Arguments: evidence, Source: protocol.Source{Kind: "api", Platform: "api_server", ChatID: first.SessionID, SessionID: "physical", NativeTaskID: "first-run", ToolCallID: "handled-first"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Call(ctx, 1001, protocol.ToolRequest{Tool: "duet_record", Arguments: evidence, ContextRef: ref}); err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{Sequence: 1, TaskID: taskID, InstanceID: "retry-instance", ThreadID: "thread-a", TurnID: "turn-a", Type: "turn/completed", Data: json.RawMessage(`{"turn":{"id":"turn-a","status":"completed"}}`), CreatedAt: time.Now().UTC()}
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &event}); err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	go func() {
		for _, action := range []string{"task.read", "goal.get"} {
			var frame protocol.Frame
			if err := wsjson.Read(ctx, conn, &frame); err != nil {
				done <- err
				return
			}
			if frame.Operation.Action != action {
				done <- fmt.Errorf("unexpected action %s", frame.Operation.Action)
				return
			}
			native.Execution = "completed"
			var data json.RawMessage
			if action == "task.read" {
				data, _ = json.Marshal(map[string]any{"task": native, "native": map[string]any{"thread": map[string]any{"status": map[string]string{"type": "idle"}}}})
			} else {
				data = json.RawMessage(`{"goal":null}`)
			}
			if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: frame.ID, Result: &protocol.Result{OperationID: frame.Operation.ID, TaskID: taskID, Stage: "accepted", Data: data}}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	if err = h.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	next, err := h.NextFollowup(ctx, 1001)
	if err != nil || string(next) == "null" {
		t.Fatalf("handled availability check suppressed actual final verification: %s %v", next, err)
	}
	if readFollowup(t, next).ID == first.ID {
		t.Fatal("final verification reused handled diagnostic")
	}
}

func TestTerminalFollowupWithoutBusinessConfirmationCreatesBoundedDiagnosticAttempt(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			ctx, h, _, taskID := followupFixture(t, 2)
			raw, err := h.NextFollowup(ctx, 1001)
			if err != nil {
				t.Fatal(err)
			}
			first := readFollowup(t, raw)
			oldArgs := json.RawMessage(`{"task_id":"` + taskID + `","kind":"verification","status":"passed","evidence":{"checks":[{"observed":"late first run"}]}}`)
			oldRef, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "duet_record", Arguments: oldArgs, Source: protocol.Source{Kind: "api", Platform: "api_server", ChatID: first.SessionID, SessionID: "physical-old", NativeTaskID: "native-first", ToolCallID: "late-record"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.SaveFollowupRun(ctx, 1001, first.ID, json.RawMessage(`{"run_id":"run-first","session_id":"`+first.SessionID+`","status":"`+status+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			reported, err := h.ReportFollowup(ctx, 1001, first.ID, json.RawMessage(`{"run_id":"run-first","status":"`+status+`","evidence":{"output":"no business checks"}}`))
			if err != nil {
				t.Fatal(err)
			}
			old := readFollowup(t, reported)
			nextRaw, err := h.NextFollowup(ctx, 1001)
			if err != nil {
				t.Fatal(err)
			}
			if string(nextRaw) == "null" {
				t.Fatalf("terminal unconfirmed run was not recovered: %s", reported)
			}
			next := readFollowup(t, nextRaw)
			if old.Status != "superseded" || next.ID == first.ID || next.SessionID == first.SessionID || next.IdempotencyKey == first.IdempotencyKey || next.RunID != "" || string(old.Request) != string(first.Request) {
				t.Fatalf("diagnostic attempt did not preserve and replace immutable association: %s / %s", reported, nextRaw)
			}
			if _, err = h.Call(ctx, 1001, protocol.ToolRequest{Tool: "duet_record", Arguments: oldArgs, ContextRef: oldRef}); err == nil {
				t.Fatal("superseded session still changed business results")
			}
			query := json.RawMessage(`{"query":"task","task_id":"` + taskID + `"}`)
			ref, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "duet_query", Arguments: query, Source: protocol.Source{Kind: "api", Platform: "api_server", ChatID: first.SessionID, SessionID: "physical-old", NativeTaskID: "native-first", ToolCallID: "read-old"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.Call(ctx, 1001, protocol.ToolRequest{Tool: "duet_query", Arguments: query, ContextRef: ref}); err != nil {
				t.Fatal(err)
			}
			late, err := h.ReportFollowup(ctx, 1001, first.ID, json.RawMessage(`{"run_id":"run-first","status":"failed","evidence":{"output":"late changed output"}}`))
			if err != nil {
				t.Fatal(err)
			}
			unchanged := readFollowup(t, late)
			if unchanged.RunID != old.RunID || string(unchanged.Request) != string(old.Request) || string(unchanged.NativeEvidence) != string(old.NativeEvidence) {
				t.Fatal("late callback rewrote superseded run evidence")
			}
			_, err = h.SaveFollowupRun(ctx, 1001, next.ID, json.RawMessage(`{"run_id":"run-second","session_id":"`+next.SessionID+`","status":"failed"}`))
			if err != nil {
				t.Fatal(err)
			}
			last, err := h.ReportFollowup(ctx, 1001, next.ID, json.RawMessage(`{"run_id":"run-second","status":"failed","evidence":{"output":"diagnostic also failed"}}`))
			if err != nil {
				t.Fatal(err)
			}
			if readFollowup(t, last).Status != "blocked" {
				t.Fatalf("cumulative budget did not block: %s", last)
			}
			end, err := h.NextFollowup(ctx, 1001)
			if err != nil || string(end) != "null" {
				t.Fatalf("exhausted task was retried: %s %v", end, err)
			}
		})
	}
}

func TestConfirmedFollowupsDoNotCreateDiagnosticAttempts(t *testing.T) {
	for _, status := range []string{"handled", "waiting_user"} {
		t.Run(status, func(t *testing.T) {
			ctx, h, _, taskID := followupFixture(t, 2)
			raw, err := h.NextFollowup(ctx, 1001)
			if err != nil {
				t.Fatal(err)
			}
			f := readFollowup(t, raw)
			evidence := json.RawMessage(`{"task_id":"` + taskID + `","kind":"followup","status":"` + status + `","evidence":{"actions":["inspected native requests"],"verification":{"observed":"request needs user"}}}`)
			ref, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "duet_record", Arguments: evidence, Source: protocol.Source{Kind: "api", Platform: "api_server", ChatID: f.SessionID, SessionID: "physical", NativeTaskID: "native-run", ToolCallID: "record-status"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.Call(ctx, 1001, protocol.ToolRequest{Tool: "duet_record", Arguments: evidence, ContextRef: ref}); err != nil {
				t.Fatal(err)
			}
			if _, err = h.SaveFollowupRun(ctx, 1001, f.ID, json.RawMessage(`{"run_id":"run-terminal","session_id":"`+f.SessionID+`","status":"completed"}`)); err != nil {
				t.Fatal(err)
			}
			if _, err = h.ReportFollowup(ctx, 1001, f.ID, json.RawMessage(`{"run_id":"run-terminal","status":"completed","evidence":{"output":"native completion"}}`)); err != nil {
				t.Fatal(err)
			}
			next, err := h.NextFollowup(ctx, 1001)
			if err != nil || string(next) != "null" {
				t.Fatalf("confirmed or blocked followup created retry: %s %v", next, err)
			}
		})
	}
}
