package executor_test

import (
	"context"
	"database/sql"
	"dots-lite/internal/executor"
	"dots-lite/internal/protocol"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeSessionsService(t *testing.T, mode string) (*executor.Service, executor.ServiceConfig, string) {
	t.Helper()
	root := t.TempDir()
	cfg := serviceConfig(t)
	cfg.DirectoryMode = "task"
	cfg.ThreadSourceKinds = []string{"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown"}
	cfg.Folders = nil
	cfg.Native.Command = []string{os.Args[0], "-test.run=^TestNativeSessionsPeer$", root, mode}
	s, err := executor.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.StartNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, cfg, root
}

func TestNativeSessionListCoversProvidersSourcesPagesAndArchiveWithoutResuming(t *testing.T) {
	s, cfg, root := nativeSessionsService(t, "idle")
	cursor := ""
	ids := []string{}
	for i := 0; i < 3; i++ {
		r := s.Execute(context.Background(), protocol.Operation{ID: fmt.Sprintf("list-%d", i), Scope: cfg.Scope, Action: "threads.list", Cursor: cursor})
		if r.Stage != "accepted" {
			t.Fatalf("list: %+v", r)
		}
		var page struct {
			Data []struct {
				ID           string `json:"thread_id"`
				Availability string `json:"availability"`
				Reason       string `json:"reason"`
			} `json:"data"`
			Cursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(r.Data, &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Data {
			if item.Availability != "needs_check" || item.Reason == "" {
				t.Fatalf("unverified writer marked available: %s", r.Data)
			}
			ids = append(ids, item.ID)
		}
		cursor = page.Cursor
	}
	if strings.Join(ids, ",") != "native-1,native-2,native-archived" || cursor != "" {
		t.Fatalf("incomplete native inventory: %v %q", ids, cursor)
	}
	log, err := os.ReadFile(filepath.Join(root, "rpc.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "thread/resume") || strings.Contains(string(log), "turn/start") {
		t.Fatal("read-only inventory changed native execution")
	}
}

func TestAttachPreservesNativeIdentityPolicyAndDirectoryAndStartsNoTurn(t *testing.T) {
	s, cfg, root := nativeSessionsService(t, "idle")
	op := protocol.Operation{ID: "attach", TaskID: "bound", Scope: cfg.Scope, Action: "task.attach", ThreadID: "native-1", WorkingDirectory: root, TaskVersion: 1, Input: "continue the report"}
	r := s.Execute(context.Background(), op)
	if r.Stage != "accepted" {
		t.Fatalf("attach: %+v", r)
	}
	var task protocol.Task
	if json.Unmarshal(r.Data, &task) != nil || !task.Paused || task.ThreadID != "native-1" || task.WorkingDirectory != root || !strings.Contains(string(task.NativePolicy), "readOnly") {
		t.Fatalf("attachment changed native identity/policy: %s", r.Data)
	}
	if _, err := os.Stat(filepath.Join(root, "turns.txt")); !os.IsNotExist(err) {
		t.Fatal("attachment started a turn")
	}
	op.ID, op.Action, op.TaskVersion, op.Input = "continue", "task.continue", 2, "continue after reviewing permissions"
	r = s.Execute(context.Background(), op)
	if r.Stage != "accepted" || r.ThreadID != "native-1" {
		t.Fatalf("continuation: %+v", r)
	}
	bytes, err := os.ReadFile(filepath.Join(root, "turns.txt"))
	if err != nil || string(bytes) != "turn\n" {
		t.Fatalf("continuation: %q %v", bytes, err)
	}
	if again := s.Execute(context.Background(), op); again.TurnID != r.TurnID {
		t.Fatalf("same operation repeated native turn: %+v", again)
	}
}

func TestUnsafeNativeAttachmentReportsReasonAndNeverStartsTurn(t *testing.T) {
	for _, mode := range []string{"active", "writer-conflict", "provider-mismatch", "missing-policy"} {
		t.Run(mode, func(t *testing.T) {
			s, cfg, root := nativeSessionsService(t, mode)
			r := s.Execute(context.Background(), protocol.Operation{ID: "attach", TaskID: "bound", Scope: cfg.Scope, Action: "task.attach", ThreadID: "native-1", WorkingDirectory: root, TaskVersion: 1, Input: "report"})
			if r.Error == nil || r.Stage == "accepted" {
				t.Fatalf("unsafe attach accepted: %+v", r)
			}
			read := s.Execute(context.Background(), protocol.Operation{ID: "read", Scope: cfg.Scope, Action: "thread.read", ThreadID: "native-1"})
			if read.Error != nil || !strings.Contains(string(read.Data), "reason") || !strings.Contains(string(read.Data), r.Error.Message[:min(15, len(r.Error.Message))]) {
				t.Fatalf("failure not visible: %s %+v", read.Data, r)
			}
			if _, err := os.Stat(filepath.Join(root, "turns.txt")); !os.IsNotExist(err) {
				t.Fatal("unsafe attach started a model turn")
			}
		})
	}
}

func TestMaintenanceLoadsUnloadedTaskUnderNativeWriterProtectionWithoutStartingTurn(t *testing.T) {
	for _, mode := range []string{"idle", "writer-conflict"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			s, cfg, root := nativeSessionsService(t, "idle")
			op := protocol.Operation{ID: "attach-before-restart", TaskID: "bound", Scope: cfg.Scope, Action: "task.attach", ThreadID: "native-1", WorkingDirectory: root, TaskVersion: 1}
			if result := s.Execute(ctx, op); result.Stage != "accepted" {
				t.Fatalf("initial attachment: %+v", result)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			cfg.Native.Command[len(cfg.Native.Command)-1] = mode
			restarted, err := executor.NewService(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if err := restarted.StartNative(ctx); err != nil {
				t.Fatal(err)
			}
			err = restarted.PauseForMaintenance(ctx)
			if mode == "idle" && err != nil {
				t.Fatalf("unloaded idle task blocked maintenance: %v", err)
			}
			if mode == "writer-conflict" && (err == nil || !strings.Contains(err.Error(), "active writer")) {
				t.Fatalf("another writer was treated as idle: %v", err)
			}
			if mode == "writer-conflict" {
				if err := restarted.Close(); err != nil {
					t.Fatal(err)
				}
				cfg.Native.Command[len(cfg.Native.Command)-1] = "idle"
				recovered, err := executor.NewService(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer recovered.Close()
				if err := recovered.StartNative(ctx); err == nil {
					t.Fatal("failed maintenance automatically restarted native work")
				}
				if err := recovered.PauseForMaintenance(ctx); err != nil {
					t.Fatalf("explicit retry could not inspect and finish the original pause: %v", err)
				}
				restarted = recovered
			}
			if _, err := os.Stat(filepath.Join(root, "turns.txt")); !os.IsNotExist(err) {
				t.Fatal("maintenance started a model turn")
			}
			snapshot, err := restarted.Snapshot(ctx)
			if err != nil || !snapshot.Tasks[0].Paused {
				t.Fatalf("pause was lost: %+v %v", snapshot, err)
			}
		})
	}
}

func TestTaskModeDoesNotNeedFolderRegistrationAndPersistsDirectoryIdentity(t *testing.T) {
	cfg := serviceConfig(t)
	cfg.DirectoryMode = "task"
	cfg.Folders = nil
	root := t.TempDir()
	s, err := executor.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartNative(context.Background()); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "create", TaskID: "task", Scope: cfg.Scope, Action: "task.create", WorkingDirectory: root, TaskVersion: 1, Input: "complete immediately"}
	if r := s.Execute(context.Background(), op); r.Stage != "accepted" {
		t.Fatalf("default task directory: %+v", r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(root+"-old", root)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(root)
	s, err = executor.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := s.Execute(context.Background(), protocol.Operation{ID: "files", Scope: cfg.Scope, TaskID: "task", Action: "files.list"})
	if r.Error == nil || r.Error.Code != "working_directory_unavailable" {
		t.Fatalf("replaced directory accepted after restart: %+v", r)
	}
}

func TestMaintenancePausesGoalsAndTasksAndRetainsPauseAcrossOfflineResume(t *testing.T) {
	cfg := serviceConfig(t)
	s, err := executor.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "create", TaskID: "task", Scope: cfg.Scope, FolderID: "test", TaskVersion: 1, Action: "task.create", Input: "work"}
	if r := s.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	goal := op
	goal.ID, goal.Action, goal.Goal = "goal", "goal.set", json.RawMessage(`{"status":"active","tokenBudget":500}`)
	if r := s.Execute(ctx, goal); r.Stage != "accepted" {
		t.Fatalf("goal: %+v", r)
	}
	if err := s.PauseForMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Maintenance.Phase != "stopping" || !state.Tasks[0].Paused || state.Tasks[0].Version != 2 {
		t.Fatalf("maintenance: %+v", state)
	}
	op.ID = "stale-create"
	if r := s.Execute(ctx, op); r.Error == nil || r.Error.Code != "maintenance" {
		t.Fatalf("late write admitted: %+v", r)
	}
	if err := s.StartNative(ctx); err == nil {
		t.Fatal("maintenance restarted native execution")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if state, err := executor.CompleteOfflineMaintenance(ctx, cfg.StatePath, false); err != nil || state.Phase != "stopped" {
		t.Fatalf("offline stop: %+v %v", state, err)
	}
	if state, err := executor.CompleteOfflineMaintenance(ctx, cfg.StatePath, true); err != nil || state.Epoch != 2 || state.Phase != "resuming" {
		t.Fatalf("offline resume: %+v %v", state, err)
	}
	s, err = executor.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, err = s.Snapshot(ctx)
	if err != nil || !state.Tasks[0].Paused || state.Maintenance.Phase != "resuming" {
		t.Fatalf("resume changed task pause: %+v %v", state, err)
	}
}

func TestLegacyMigrationKeepsNativeIDsReceiptsAllowlistAndRequiresExplicitResume(t *testing.T) {
	ctx := context.Background()
	cfg := serviceConfig(t)
	s, err := executor.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "original-op", TaskID: "original-task", Scope: cfg.Scope, FolderID: "test", TaskVersion: 1, Action: "task.create", Input: "work"}
	if r := s.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var digest, receipt string
	if err := db.QueryRow(`SELECT digest,result FROM executor_operations WHERE operation_id='original-op'`).Scan(&digest, &receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM executor_meta WHERE key='state_protocol' OR key LIKE 'task_root_identity:%'; UPDATE executor_tasks SET payload=json_remove(payload,'$.native_policy','$.directory_mode')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if unsafe, err := executor.NewService(cfg); err == nil {
		_ = unsafe.Close()
		t.Fatal("legacy state silently entered protocol 4")
	}
	backup, err := executor.MigrateV3(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal(err)
	}
	s, err = executor.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Maintenance.Phase != "stopped" || len(state.Tasks) != 1 || state.Tasks[0].ThreadID != "thread-1" || !state.Tasks[0].Paused || state.Tasks[0].DirectoryMode != "allowlist" {
		t.Fatalf("migration changed task identity or permissions: %+v", state)
	}
	db, err = sql.Open("sqlite", cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var afterDigest, afterReceipt string
	if err := db.QueryRow(`SELECT digest,result FROM executor_operations WHERE operation_id='original-op'`).Scan(&afterDigest, &afterReceipt); err != nil {
		t.Fatal(err)
	}
	if digest != afterDigest || receipt != afterReceipt {
		t.Fatal("migration changed an immutable native operation receipt")
	}
}

func TestNativeSessionsPeer(t *testing.T) {
	if len(os.Args) != 4 || os.Args[1] != "-test.run=^TestNativeSessionsPeer$" {
		return
	}
	root, mode := os.Args[2], os.Args[3]
	dec, enc := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	loaded := false
	active := false
	thread := func(id string) map[string]any {
		status := "notLoaded"
		if loaded {
			status = "idle"
		}
		if active || mode == "active" {
			status = "active"
		}
		return map[string]any{"id": id, "cwd": root, "name": "native session", "preview": "previous history", "source": "vscode", "modelProvider": "relay", "model": "fixture-model", "canAcceptDirectInput": true, "updatedAt": 123, "status": map[string]string{"type": status}}
	}
	for {
		var frame struct {
			ID     json.RawMessage            `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&frame); err != nil {
			if err == io.EOF {
				os.Exit(0)
			}
			os.Exit(3)
		}
		log, err := os.OpenFile(filepath.Join(root, "rpc.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(3)
		}
		_, _ = fmt.Fprintln(log, frame.Method)
		_ = log.Close()
		if frame.Method == "initialized" {
			continue
		}
		var result any
		switch frame.Method {
		case "initialize":
			result = map[string]string{"userAgent": "native-sessions-fixture"}
		case "thread/list":
			var providers, sources []string
			_ = json.Unmarshal(frame.Params["modelProviders"], &providers)
			_ = json.Unmarshal(frame.Params["sourceKinds"], &sources)
			if frame.Params["modelProviders"] == nil || len(providers) != 0 || len(sources) != 10 {
				os.Exit(4)
			}
			var archived bool
			var cursor string
			_ = json.Unmarshal(frame.Params["archived"], &archived)
			_ = json.Unmarshal(frame.Params["cursor"], &cursor)
			id, next := "native-1", "page2"
			if cursor == "page2" {
				id, next = "native-2", ""
			}
			if archived {
				id, next = "native-archived", ""
			}
			result = map[string]any{"data": []any{thread(id)}, "nextCursor": next}
		case "thread/read":
			var id string
			_ = json.Unmarshal(frame.Params["threadId"], &id)
			result = map[string]any{"thread": thread(id)}
		case "thread/goal/get":
			result = map[string]any{"goal": nil}
		case "thread/unarchive":
			result = map[string]any{"thread": thread("native-1")}
		case "thread/resume":
			if frame.Params["cwd"] != nil || frame.Params["sandbox"] != nil || frame.Params["approvalPolicy"] != nil {
				os.Exit(5)
			}
			if mode == "writer-conflict" {
				_ = enc.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32600, "message": "already has an active writer"}})
				continue
			}
			loaded = true
			provider := "relay"
			if mode == "provider-mismatch" {
				provider = "openai"
			}
			result = map[string]any{"thread": thread("native-1"), "cwd": root, "approvalPolicy": "on-request", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "modelProvider": provider, "model": "fixture-model"}
			if mode == "missing-policy" {
				delete(result.(map[string]any), "sandbox")
			}
		case "turn/start":
			active = true
			if err := os.WriteFile(filepath.Join(root, "turns.txt"), []byte("turn\n"), 0600); err != nil {
				os.Exit(3)
			}
			result = map[string]any{"turn": map[string]string{"id": "continued-turn", "status": "inProgress"}}
		default:
			os.Exit(6)
		}
		if err := enc.Encode(map[string]any{"id": frame.ID, "result": result}); err != nil {
			os.Exit(3)
		}
	}
}

func TestNativeGoalPauseInvalidatesAnAlreadyPreparedForegroundContinuation(t *testing.T) {
	cfg := serviceConfig(t)
	s, err := executor.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "start", TaskID: "task", Scope: cfg.Scope, FolderID: "test", TaskVersion: 1, Action: "task.create", Input: "pause goal externally", Foreground: true}
	_ = s.Execute(ctx, op)
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		state, err := s.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Tasks) == 1 && state.Tasks[0].Paused && state.Tasks[0].Version == 2 {
			op.ID, op.Action, op.Input = "old-continue", "task.continue", "continue"
			if r := s.Execute(ctx, op); r.Error == nil || r.Error.Code != "stale_version" {
				t.Fatalf("queued foreground request bypassed native pause: %+v", r)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("native Goal pause did not persist a new business version")
}

func TestInstalledNativeInventoryReadOnly(t *testing.T) {
	if os.Getenv("DOTS_LITE_NATIVE_READONLY") != "1" {
		t.Skip("explicit readonly native inventory opt-in")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cfg := executor.ServiceConfig{Scope: protocol.Scope{UserID: "readonly-audit", ProfileID: "readonly-audit", ExecutorID: "readonly-audit"}, StatePath: filepath.Join(t.TempDir(), "inventory.db"), Native: executor.Options{Command: []string{codex, "app-server", "--listen", "stdio://"}, Stderr: io.Discard}}
	s, err := executor.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.LoadThreadSources(ctx, codex); err != nil {
		t.Fatal(err)
	}
	if err := s.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	cursor := ""
	pages, total := 0, 0
	for {
		pages++
		r := s.Execute(ctx, protocol.Operation{ID: fmt.Sprintf("readonly-page-%d", pages), Scope: cfg.Scope, Action: "threads.list", Cursor: cursor})
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		var page struct {
			Data   []json.RawMessage `json:"data"`
			Cursor string            `json:"next_cursor"`
		}
		if err := json.Unmarshal(r.Data, &page); err != nil {
			t.Fatal(err)
		}
		total += len(page.Data)
		cursor = page.Cursor
		if cursor == "" {
			break
		}
	}
	t.Logf("native readonly inventory: %d persistent sessions, %d pages; no resume or turn was submitted", total, pages)
}
