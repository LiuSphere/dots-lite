package hub_test

import (
	"context"
	"encoding/json"
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

func TestFolderCreateKeepsWorkingDirectoryAndOriginalInvocation(t *testing.T) {
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
	args := `{"action":"task.create","folder_id":"demo","working_directory":"reports/monthly","input":"write report"}`
	prepared := call(t, h, 1001, "dots_lite_prepare", args)
	var ids struct {
		TaskID      string `json:"task_id"`
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(prepared, &ids); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`{"query":"task","task_id":"` + ids.TaskID + `"}`,
		`{"query":"operation","operation_id":"` + ids.OperationID + `"}`,
	} {
		var got map[string]any
		if err := json.Unmarshal(call(t, h, 1001, "dots_lite_query", query), &got); err != nil {
			t.Fatal(err)
		}
		if got["folder_id"] != "demo" || got["working_directory"] != "reports/monthly" {
			t.Fatalf("folder or directory was lost: %v", got)
		}
		if _, present := got["project_id"]; present {
			t.Fatal("obsolete project_id was exposed")
		}
	}
}

func TestFolderQueriesListOnlyAuthorizedFolders(t *testing.T) {
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
	connectFolderExecutor(t, h)
	for _, query := range []string{"folders"} {
		var folders []protocol.Folder
		if err := json.Unmarshal(call(t, h, 1001, "dots_lite_query", `{"query":"`+query+`"}`), &folders); err != nil {
			t.Fatal(err)
		}
		if len(folders) != 1 || folders[0].ID != "demo" {
			t.Fatalf("unexpected authorized folders: %+v", folders)
		}
	}
	var machine struct {
		Snapshot struct {
			Folders []protocol.Folder `json:"folders"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(call(t, h, 1001, "dots_lite_query", `{"query":"machine"}`), &machine); err != nil {
		t.Fatal(err)
	}
	if len(machine.Snapshot.Folders) != 1 || machine.Snapshot.Folders[0].ID != "demo" {
		t.Fatalf("machine did not expose authorized folders: %+v", machine)
	}
}

func connectFolderExecutor(t *testing.T, h *hub.Hub) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	server := httptest.NewServer(h.WebsocketHandler())
	t.Cleanup(server.Close)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/executor", &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer token-a"}}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow(); cancel() })
	if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "hello", Snapshot: &protocol.Snapshot{InstanceID: "folder-interface", Initialized: true, Folders: []protocol.Folder{{ID: "demo", Root: "C:/authorized", Available: true}, {ID: "hidden", Root: "C:/private", Available: true}}}}); err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	return conn, ctx
}

func TestFolderDirectoryBrowseRequiresExplicitAuthorizedFolder(t *testing.T) {
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
	conn, ctx := connectFolderExecutor(t, h)
	for _, args := range []string{
		`{"query":"directory"}`,
		`{"query":"directory","project_id":"demo"}`,
		`{"query":"directory","folder_id":"hidden"}`,
		`{"query":"text","folder_id":"demo","path":"private.txt"}`,
		`{"query":"download","folder_id":"demo","path":"private.txt"}`,
	} {
		if _, err := h.Call(ctx, 1001, registered(t, h, 1001, "dots_lite_query", args)); err == nil {
			t.Fatalf("unbound or unauthorized file query was accepted: %s", args)
		}
	}
	want := json.RawMessage(`[{"name":"nested","directory":true}]`)
	seen := make(chan protocol.Operation, 1)
	go func() {
		var frame protocol.Frame
		if wsjson.Read(ctx, conn, &frame) != nil || frame.Operation == nil {
			return
		}
		seen <- *frame.Operation
		_ = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: frame.ID, Result: &protocol.Result{OperationID: frame.Operation.ID, Stage: "accepted", Data: want}})
	}()
	got := call(t, h, 1001, "dots_lite_query", `{"query":"directory","folder_id":"demo","path":"."}`)
	if string(got) != string(want) {
		t.Fatalf("directory result = %s", got)
	}
	select {
	case op := <-seen:
		if op.TaskID != "" || op.FolderID != "demo" || op.Action != "files.list" || op.Path != "." {
			t.Fatalf("unexpected browse operation: %+v", op)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestFolderStopKeepsDirectoryAndInvalidatesPreparedVersion(t *testing.T) {
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
	var created struct {
		TaskID      string `json:"task_id"`
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(call(t, h, 1001, "dots_lite_prepare", `{"action":"task.create","folder_id":"demo","working_directory":"nested","input":"work"}`), &created); err != nil {
		t.Fatal(err)
	}
	var stopped struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(call(t, h, 1001, "dots_lite_stop", `{"task_id":"`+created.TaskID+`"}`), &stopped); err != nil {
		t.Fatal(err)
	}
	var op struct {
		WorkingDirectory string `json:"working_directory"`
		TaskVersion      int64  `json:"task_version"`
	}
	if err := json.Unmarshal(call(t, h, 1001, "dots_lite_query", `{"query":"operation","operation_id":"`+stopped.OperationID+`"}`), &op); err != nil {
		t.Fatal(err)
	}
	if op.WorkingDirectory != "nested" || op.TaskVersion != 2 {
		t.Fatalf("stop lost its directory or version: %+v", op)
	}
	if _, err := h.Call(context.Background(), 1001, registered(t, h, 1001, "dots_lite_submit", `{"operation_id":"`+created.OperationID+`"}`)); err == nil {
		t.Fatal("prepared create survived the pause version change")
	}
}

func TestFolderDirectoryAndInvocationStayFixedAcrossRetriesAndRestart(t *testing.T) {
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
	request := registered(t, h, 1001, "dots_lite_prepare", `{"action":"task.create","folder_id":"demo","working_directory":"nested","input":"work"}`)
	first, err := h.Call(context.Background(), 1001, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.Call(context.Background(), 1001, request)
	if err != nil || string(first) != string(again) {
		t.Fatalf("prepare retry created another operation: %s, %v", again, err)
	}
	changed := request
	changed.Arguments = json.RawMessage(`{"action":"task.create","project_id":"demo","working_directory":"nested","input":"work"}`)
	if _, err := h.Call(context.Background(), 1001, changed); err == nil {
		t.Fatal("alias rewriting bypassed the original invocation arguments")
	}
	var created struct {
		TaskID      string `json:"task_id"`
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(first, &created); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"other", ".", "..", "nested/..", "/outside", `C:\outside`, `nested\child`, "nested//child", "nested/./child", "nested/", "volume:child"} {
		args, _ := json.Marshal(map[string]string{"action": "task.continue", "task_id": created.TaskID, "working_directory": directory, "input": "continue"})
		if _, err := h.Call(context.Background(), 1001, registered(t, h, 1001, "dots_lite_prepare", string(args))); err == nil {
			t.Fatalf("task directory changed or escaped: %q", directory)
		}
	}
	for _, fields := range []string{``, `,"working_directory":"nested"`, `,"folder_id":"demo","working_directory":"nested"`} {
		call(t, h, 1001, "dots_lite_prepare", `{"action":"task.continue","task_id":"`+created.TaskID+`","input":"continue"`+fields+`}`)
	}
	if _, err := h.Call(context.Background(), 1001, registered(t, h, 1001, "dots_lite_prepare", `{"action":"task.create","project_id":"demo","folder_id":"other","input":"work"}`)); err == nil {
		t.Fatal("obsolete project_id argument was accepted")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h, err = hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, query := range []string{
		`{"query":"task","task_id":"` + created.TaskID + `"}`,
		`{"query":"operation","operation_id":"` + created.OperationID + `"}`,
	} {
		var got map[string]any
		if err := json.Unmarshal(call(t, h, 1001, "dots_lite_query", query), &got); err != nil {
			t.Fatal(err)
		}
		if got["working_directory"] != "nested" {
			t.Fatalf("restart changed the task or operation directory: %v", got)
		}
	}
	var rootTask struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(call(t, h, 1001, "dots_lite_prepare", `{"action":"task.create","folder_id":"demo","input":"root task"}`), &rootTask); err != nil {
		t.Fatal(err)
	}
	var root struct {
		WorkingDirectory string `json:"working_directory"`
	}
	if err := json.Unmarshal(call(t, h, 1001, "dots_lite_query", `{"query":"task","task_id":"`+rootTask.TaskID+`"}`), &root); err != nil || root.WorkingDirectory != "." {
		t.Fatalf("omitted directory did not select the folder root: %+v, %v", root, err)
	}
}

func TestFolderCreateRejectsOutsideOrUncleanDirectory(t *testing.T) {
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
	for _, directory := range []string{"..", "child/../other", "/outside", `C:/outside`, `child\other`, "child//other", "child/./other", "child/", "volume:child"} {
		args, _ := json.Marshal(map[string]string{"action": "task.create", "folder_id": "demo", "working_directory": directory, "input": "work"})
		if _, err := h.Call(context.Background(), 1001, registered(t, h, 1001, "dots_lite_prepare", string(args))); err == nil {
			t.Fatalf("outside or unclean creation directory was accepted: %q", directory)
		}
	}
	for _, directory := range []string{".", "child", "child/other"} {
		args, _ := json.Marshal(map[string]string{"action": "task.create", "folder_id": "demo", "working_directory": directory, "input": "work"})
		call(t, h, 1001, "dots_lite_prepare", string(args))
	}
}

func TestFolderBindingAuthorizesCreateAcrossRestart(t *testing.T) {
	for _, fields := range []string{`"folder_ids":["demo"]`} {
		t.Run(fields, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var cfg hub.Config
			if err := json.Unmarshal([]byte(`{"bindings":[{"uid":1001,"user_id":"a","profile_id":"a","executor_id":"pc-a","token":"token-a",`+fields+`,"sources":[{"platform":"test","sender_id":"sender-a","chat_id":"chat-a"}]}]}`), &cfg); err != nil {
				t.Fatal(err)
			}
			h, err := hub.New(cfg, db)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			call(t, h, 1001, "dots_lite_prepare", `{"action":"task.create","folder_id":"demo","input":"work"}`)
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := hub.New(cfg, db)
			if err != nil {
				t.Fatalf("reusing the same authorized-folder configuration failed: %v", err)
			}
			defer restarted.Close()
			call(t, restarted, 1001, "dots_lite_prepare", `{"action":"task.create","folder_id":"demo","input":"work after restart"}`)
		})
	}
}
