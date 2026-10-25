package executor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"dots-lite/internal/executor"
	"dots-lite/internal/protocol"
)

func TestInstalledNativeAttachmentWithLocalFixedProvider(t *testing.T) {
	if os.Getenv("DOTS_LITE_NATIVE_FIXTURE") != "1" {
		t.Skip("explicit native fixture opt-in; no external inference")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	var requests atomic.Int64
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		w.Header().Set("Content-Type", "text/event-stream")
		item := map[string]any{"type": "message", "id": "msg_fixture", "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": "local-fixture-ok", "annotations": []any{}}}}
		for _, event := range []map[string]any{{"type": "response.created", "response": map[string]any{"id": "resp_fixture", "status": "in_progress", "output": []any{}}}, {"type": "response.output_item.added", "output_index": 0, "item": item}, {"type": "response.output_item.done", "output_index": 0, "item": item}, {"type": "response.completed", "response": map[string]any{"id": "resp_fixture", "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}}} {
			raw, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		}
	}))
	defer fixture.Close()
	options := executor.Options{Command: []string{codex, "app-server", "--listen", "stdio://", "-c", `model_provider="dots_fixture"`, "-c", `model_providers.dots_fixture.name="Local fixed fixture"`, "-c", `model_providers.dots_fixture.base_url="` + fixture.URL + `/v1"`, "-c", `model_providers.dots_fixture.wire_api="responses"`, "-c", `model_providers.dots_fixture.requires_openai_auth=false`, "-c", `model_providers.dots_fixture.supports_websockets=false`}, ExperimentalAPI: true, Stderr: io.Discard}
	client, err := executor.Start(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	done := make(chan executor.Message, 1)
	go func() {
		for event := range client.Events() {
			if event.Method == "turn/completed" {
				done <- event
			}
		}
	}()
	root := t.TempDir()
	start, err := client.Call(ctx, "thread/start", map[string]any{"cwd": root, "ephemeral": false, "approvalPolicy": "on-request", "sandbox": "read-only"})
	if err != nil {
		t.Fatal(err)
	}
	var before struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Approval json.RawMessage `json:"approvalPolicy"`
		Sandbox  json.RawMessage `json:"sandbox"`
	}
	if err := json.Unmarshal(start, &before); err != nil || before.Thread.ID == "" {
		t.Fatalf("thread start: %v", err)
	}
	var service *executor.Service
	defer func() {
		if service != nil {
			if err := service.Close(); err != nil {
				t.Error(err)
			}
			service = nil
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanup, err := executor.Start(cleanupCtx, options)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := cleanup.Call(cleanupCtx, "thread/archive", map[string]string{"threadId": before.Thread.ID}); err != nil {
			t.Error(err)
		}
		if err := cleanup.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := client.Call(ctx, "turn/start", map[string]any{"threadId": before.Thread.ID, "input": []map[string]string{{"type": "text", "text": "Return local-fixture-ok only; do not call tools."}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := executor.ServiceConfig{Scope: protocol.Scope{UserID: "local-fixture", ProfileID: "local-fixture", ExecutorID: "local-fixture"}, StatePath: filepath.Join(t.TempDir(), "executor.db"), DirectoryMode: "task", Native: options}
	service, err = executor.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "attach", TaskID: "fixture", Scope: cfg.Scope, TaskVersion: 1, Action: "task.attach", ThreadID: before.Thread.ID, WorkingDirectory: root, Input: "continue this fixture"}
	attached := service.Execute(ctx, op)
	if attached.Error != nil {
		t.Fatalf("native attachment: %+v", attached)
	}
	var task protocol.Task
	if err := json.Unmarshal(attached.Data, &task); err != nil {
		t.Fatal(err)
	}
	var actual map[string]json.RawMessage
	if err := json.Unmarshal(task.NativePolicy, &actual); err != nil {
		t.Fatal(err)
	}
	var oldPolicy, newPolicy any
	_ = json.Unmarshal(before.Sandbox, &oldPolicy)
	_ = json.Unmarshal(actual["sandbox"], &newPolicy)
	if task.ThreadID != before.Thread.ID || task.WorkingDirectory != root || !reflect.DeepEqual(oldPolicy, newPolicy) || string(actual["approvalPolicy"]) != string(before.Approval) {
		t.Fatal("native attachment changed identity, cwd or permissions")
	}
	op.ID, op.Action, op.TaskVersion, op.Foreground, op.Input = "continue", "task.continue", 2, true, "Return local-fixture-ok again; do not call tools."
	continued := service.Execute(ctx, op)
	if continued.Error != nil || continued.ThreadID != before.Thread.ID {
		t.Fatalf("native continuation: %+v", continued)
	}
	for {
		state, err := service.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if state.Tasks[0].Execution == "completed" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	history := service.Execute(ctx, protocol.Operation{ID: "history", TaskID: "fixture", Scope: cfg.Scope, Action: "task.history", ThreadID: before.Thread.ID})
	var turns struct {
		Data []json.RawMessage `json:"data"`
	}
	if history.Error != nil || json.Unmarshal(history.Data, &turns) != nil || len(turns.Data) < 2 {
		t.Fatalf("original history was not retained: %s %v", history.Data, history.Error)
	}
	t.Logf("real Codex attach/continue/history passed with %d localhost fixture HTTP requests; no external inference", requests.Load())
}
