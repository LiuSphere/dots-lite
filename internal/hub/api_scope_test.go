package hub_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dots-duet-lite/internal/hub"
	"dots-duet-lite/internal/protocol"
	"dots-duet-lite/internal/store"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestAPIQueriesRequireTheirPreRegisteredTask(t *testing.T) {
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
	defer h.Close()
	source := protocol.Source{Kind: "api", Platform: "api_server", ChatID: "unregistered", SessionID: "physical-session", NativeTaskID: "native-run", ToolCallID: "tool-call"}
	for _, args := range []string{`{"query":"tasks"}`, `{"query":"download","task_id":"known-task","path":"private.txt"}`} {
		if _, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "duet_query", Arguments: json.RawMessage(args), Source: source}); err == nil {
			t.Fatalf("unregistered API session obtained a task query reference: %s", args)
		}
	}
	var first, second struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"first"}`), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(call(t, h, 1001, "duet_prepare", `{"action":"task.create","project_id":"demo","input":"second"}`), &second); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h.WebsocketHandler())
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/executor", &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer token-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "hello", Snapshot: &protocol.Snapshot{InstanceID: "api-scope-instance", Initialized: true}}); err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &protocol.Event{Sequence: 1, TaskID: first.TaskID, InstanceID: "api-scope-instance", Type: "error", Data: json.RawMessage(`{"message":"fixture failure requiring a bound followup"}`), CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	follow, err := h.NextFollowup(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	var binding struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(follow, &binding); err != nil || binding.SessionID == "" {
		t.Fatalf("missing followup binding: %s, %v", follow, err)
	}
	source.ChatID = binding.SessionID
	if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &protocol.Event{Sequence: 2, TaskID: second.TaskID, InstanceID: "api-scope-instance", Type: "error", Data: json.RawMessage(`{"message":"another task failure"}`), CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{
		`{"query":"tasks"}`, `{"query":"operations"}`, `{"query":"followups"}`, `{"query":"machine"}`,
		`{"query":"download","task_id":"` + second.TaskID + `","path":"private.txt"}`,
		`{"query":"followups","task_id":"` + second.TaskID + `"}`,
	} {
		if _, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "duet_query", Arguments: json.RawMessage(args), Source: source}); err == nil {
			t.Fatalf("prebound API session escaped its task: %s", args)
		}
	}
	for _, query := range []string{"task", "operations", "followups"} {
		args, _ := json.Marshal(map[string]string{"query": query, "task_id": first.TaskID})
		ref, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "duet_query", Arguments: args, Source: source})
		if err != nil {
			t.Fatal(err)
		}
		result, err := h.Call(ctx, 1001, protocol.ToolRequest{Tool: "duet_query", Arguments: args, ContextRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		if query == "followups" {
			var records []struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal(result, &records); err != nil || len(records) != 1 || records[0].TaskID != first.TaskID {
				t.Fatalf("task-scoped followup query leaked or lost records: %s, %v", result, err)
			}
		}
	}
}
