package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dots-duet-lite/internal/executor"
	"dots-duet-lite/internal/hub"
	"dots-duet-lite/internal/protocol"
	"dots-duet-lite/internal/store"
)

func TestBridgeRunsRegisteredTaskWithoutRepeatingExecution(t *testing.T) {
	runRegisteredBridge(t, false, "")
}

func TestBridgeStagesPrivateInputsBeforeNativeTurnAndOmitsPublicBytes(t *testing.T) {
	runRegisteredBridge(t, true, "")
}

func TestBridgeRunsInChosenSubfolderAndKeepsInputsAndFilesThere(t *testing.T) {
	runRegisteredBridge(t, true, "team-a/demo")
}

func runRegisteredBridge(t *testing.T, withInputs bool, workingDirectory string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	state := t.TempDir()
	db, err := store.Open(filepath.Join(state, "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	scope := protocol.Scope{UserID: "fixture-user", ProfileID: "fixture-profile", ExecutorID: "fixture-computer"}
	h, err := hub.New(hub.Config{Bindings: []hub.Binding{{
		UID: 101, UserID: scope.UserID, ProfileID: scope.ProfileID, ExecutorID: scope.ExecutorID,
		Token: "fixture-executor-token", ProjectIDs: []string{"project"},
		Sources: []hub.AllowedSource{{Platform: "fixture", SenderID: "sender", ChatID: "chat"}},
	}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h.WebsocketHandler())
	t.Cleanup(server.Close)
	authorizedRoot := t.TempDir()
	projectRoot := authorizedRoot
	if workingDirectory != "" {
		projectRoot = filepath.Join(authorizedRoot, filepath.FromSlash(workingDirectory))
		if err := os.MkdirAll(projectRoot, 0700); err != nil {
			t.Fatal(err)
		}
	}
	nativeMode := "bridge"
	if withInputs {
		nativeMode = "bridge-inputs"
	}
	service, err := executor.NewService(executor.ServiceConfig{
		Scope: scope, StatePath: filepath.Join(state, "executor.sqlite"),
		Projects: []protocol.Project{{ID: "project", Root: authorizedRoot, ApprovalPolicy: "on-request", Sandbox: "workspace-write"}},
		Native: executor.Options{
			Command: []string{os.Args[0], "-test.run=^TestNativeProcess$", "--", nativeMode},
			Stderr:  io.Discard,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan error, 1)
	go func() {
		defer close(workerDone)
		workerDone <- service.Run(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/executor", "fixture-executor-token")
	}()
	t.Cleanup(func() {
		cancel()
		if err := service.Close(); err != nil {
			t.Logf("executor shutdown: %v", err)
		}
		select {
		case <-workerDone:
		case <-time.After(4 * time.Second):
			t.Error("executor worker did not stop")
		}
	})
	args := json.RawMessage(`{"query":"machine"}`)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		ref, err := h.RegisterInvocation(ctx, 101, protocol.Invocation{
			Tool: "duet_query", Arguments: args,
			Source: protocol.Source{Kind: "gateway", Platform: "fixture", SenderID: "sender", ChatID: "chat", SessionID: "fixture-session"},
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := h.Call(ctx, 101, protocol.ToolRequest{Tool: "duet_query", Arguments: args, ContextRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		var machine struct {
			Connected bool              `json:"connected"`
			Snapshot  protocol.Snapshot `json:"snapshot"`
		}
		if err := json.Unmarshal(result, &machine); err != nil {
			t.Fatal(err)
		}
		if machine.Connected && machine.Snapshot.Initialized {
			if len(machine.Snapshot.Projects) != 1 || machine.Snapshot.Projects[0].ID != "project" {
				t.Fatalf("registered project missing from readiness: %s", result)
			}
			break
		}
		select {
		case err := <-workerDone:
			t.Fatalf("executor stopped before ready: %v", err)
		case <-ctx.Done():
			t.Fatalf("machine never became ready: %s", result)
		case <-ticker.C:
		}
	}
	callTool := func(tool string, arguments any, attachments ...protocol.InvocationAttachment) json.RawMessage {
		t.Helper()
		args, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := h.RegisterInvocation(ctx, 101, protocol.Invocation{
			Tool: tool, Arguments: args,
			Source:          protocol.Source{Kind: "gateway", Platform: "fixture", SenderID: "sender", ChatID: "chat", SessionID: "fixture-session"},
			AttachmentFiles: attachments,
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := h.Call(ctx, 101, protocol.ToolRequest{Tool: tool, Arguments: args, ContextRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	prepareArgs := map[string]any{"action": "task.create", "project_id": "project", "input": "Create the fixture proof file", "name": "fixture task"}
	if workingDirectory != "" {
		folders := callTool("duet_query", map[string]string{"query": "folders"})
		var listed []protocol.Project
		if err := json.Unmarshal(folders, &listed); err != nil || len(listed) != 1 || listed[0].Root != authorizedRoot {
			t.Fatalf("authorized folders: %s, %v", folders, err)
		}
		directory := callTool("duet_query", map[string]string{"query": "directory", "folder_id": "project", "path": "team-a"})
		var listing struct {
			Entries []struct {
				Name        string `json:"name"`
				IsDirectory bool   `json:"is_directory"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(directory, &listing); err != nil || len(listing.Entries) != 1 || listing.Entries[0].Name != "demo" || !listing.Entries[0].IsDirectory {
			t.Fatalf("browse before a task: %s, %v", directory, err)
		}
		delete(prepareArgs, "project_id")
		prepareArgs["folder_id"] = "project"
		prepareArgs["working_directory"] = workingDirectory
		prepareArgs["artifacts"] = []string{"proof.txt"}
	}
	var attachments []protocol.InvocationAttachment
	if withInputs {
		prepareArgs["inputs"] = []map[string]string{{"name": "input.bin", "local_path": "/fixture-admission/input"}, {"name": "empty.txt", "local_path": "/fixture-admission/empty"}}
		for _, file := range []struct {
			Name, Path string
			Data       []byte
		}{{"input.bin", "/fixture-admission/input", []byte("\x00fixture attachment\r\n\xff")}, {"empty.txt", "/fixture-admission/empty", []byte{}}} {
			sum := sha256.Sum256(file.Data)
			attachments = append(attachments, protocol.InvocationAttachment{LocalPath: file.Path, File: protocol.FileUpload{Name: file.Name, DataBase64: base64.StdEncoding.EncodeToString(file.Data), SHA256: hex.EncodeToString(sum[:]), Size: int64(len(file.Data))}})
		}
	}
	prepared := callTool("duet_prepare", prepareArgs, attachments...)
	assertPublic := func(raw json.RawMessage) {
		t.Helper()
		if bytes.Contains(raw, []byte("data_base64")) || bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString([]byte("\x00fixture attachment\r\n\xff")))) {
			t.Fatalf("private attachment bytes appeared in public result: %s", raw)
		}
	}
	assertPublic(prepared)
	var registration struct {
		TaskID      string `json:"task_id"`
		OperationID string `json:"operation_id"`
		Stage       string `json:"stage"`
	}
	if err := json.Unmarshal(prepared, &registration); err != nil {
		t.Fatal(err)
	}
	if registration.Stage != "prepared" || registration.TaskID == "" || registration.OperationID == "" {
		t.Fatalf("missing recoverable registration: %s", prepared)
	}
	if _, err := os.Stat(filepath.Join(projectRoot, "proof.txt")); !os.IsNotExist(err) {
		t.Fatalf("prepare produced execution side effects: %v", err)
	}
	if withInputs {
		if _, err := os.Stat(filepath.Join(projectRoot, ".dots-duet", "attachments", registration.TaskID)); !os.IsNotExist(err) {
			t.Fatalf("prepare staged input files before submission: %v", err)
		}
		assertPublic(callTool("duet_query", map[string]string{"query": "operation", "operation_id": registration.OperationID}))
		assertPublic(callTool("duet_query", map[string]string{"query": "operations", "task_id": registration.TaskID}))
	}
	submit := map[string]string{"operation_id": registration.OperationID}
	submitted := callTool("duet_submit", submit)
	assertPublic(submitted)
	for {
		result := callTool("duet_query", map[string]string{"query": "operation", "operation_id": registration.OperationID})
		assertPublic(result)
		var operation struct {
			Stage  string           `json:"stage"`
			Result *protocol.Result `json:"result"`
		}
		if err := json.Unmarshal(result, &operation); err != nil {
			t.Fatal(err)
		}
		if operation.Stage == "accepted" {
			break
		}
		if operation.Stage == "failed" || operation.Stage == "unknown" {
			t.Fatalf("execution did not accept registered operation: %s", result)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("registered operation did not settle: %s", result)
		case <-ticker.C:
		}
	}
	repeated := callTool("duet_submit", submit)
	assertPublic(repeated)
	if !bytes.Equal(submitted, repeated) {
		t.Fatalf("duplicate operation changed its persisted receipt: %s / %s", submitted, repeated)
	}
	assertPublic(callTool("duet_query", map[string]string{"query": "operations", "task_id": registration.TaskID}))
	proof, err := os.ReadFile(filepath.Join(projectRoot, "proof.txt"))
	if err != nil || string(proof) != "executed\n" {
		t.Fatalf("duplicate submission repeated or lost execution: proof=%q, error=%v", proof, err)
	}
	if withInputs {
		for _, name := range []string{"input.bin", "empty.txt"} {
			actual, err := os.ReadFile(filepath.Join(projectRoot, ".dots-duet", "attachments", registration.TaskID, name))
			if err != nil {
				t.Fatal(err)
			}
			expected := []byte{}
			if name == "input.bin" {
				expected = []byte("\x00fixture attachment\r\n\xff")
			}
			if !bytes.Equal(actual, expected) {
				t.Fatalf("staged %s content changed after duplicate submit: %q", name, actual)
			}
		}
	}
	for {
		result := callTool("duet_query", map[string]string{"query": "task", "task_id": registration.TaskID})
		var task protocol.Task
		if err := json.Unmarshal(result, &task); err != nil {
			t.Fatal(err)
		}
		if task.Execution == "completed" {
			if workingDirectory != "" && task.WorkingDirectory != workingDirectory {
				t.Fatalf("task lost its selected directory: %s", result)
			}
			if task.Verification != "pending" || task.Delivery != "pending" {
				t.Fatalf("turn completion was confused with verification/delivery: %s", result)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("fast native completion was not retained: %s", result)
		case <-ticker.C:
		}
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].Execution != "completed" {
		t.Fatalf("executor snapshot overwrote fast completion: %+v", snapshot.Tasks)
	}
	if workingDirectory != "" {
		text := callTool("duet_query", map[string]string{"query": "text", "task_id": registration.TaskID, "path": "proof.txt"})
		var file struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(text, &file); err != nil || file.Text != "executed\n" {
			t.Fatalf("task-relative file query: %s, %v", text, err)
		}
		if _, err := os.Stat(filepath.Join(authorizedRoot, "proof.txt")); !os.IsNotExist(err) {
			t.Fatalf("task wrote outside its chosen working directory: %v", err)
		}
		if _, err := os.Stat(filepath.Join(authorizedRoot, ".dots-duet")); !os.IsNotExist(err) {
			t.Fatalf("inputs were staged at authorization root: %v", err)
		}
	}
}

// This is an external protocol fixture, not a substitute for real Codex tests.
func TestNativeProcess(t *testing.T) {
	if len(os.Args) < 2 {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if mode != "bridge" && mode != "bridge-inputs" {
		return
	}
	if err := nativePeer(mode == "bridge-inputs"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func nativePeer(expectInputs bool) error {
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	projectRoot := ""
	turnNumber := 0
	for {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := decoder.Decode(&message); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if message.Method == "initialized" {
			continue
		}
		var result any
		switch message.Method {
		case "initialize":
			result = map[string]any{"userAgent": "bridge-fixture/1"}
		case "account/read":
			result = map[string]any{"account": nil, "requiresOpenaiAuth": false}
		case "thread/goal/get":
			result = map[string]any{"goal": nil}
		case "thread/read":
			result = map[string]any{"thread": map[string]any{"id": "fixture-thread", "status": map[string]string{"type": "idle"}}}
		case "thread/start":
			var params struct {
				CWD string `json:"cwd"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				return err
			}
			projectRoot = params.CWD
			if expectInputs {
				if err := verifyStagedInputsBeforeTurn(projectRoot); err != nil {
					return err
				}
			}
			result = map[string]any{"thread": map[string]any{"id": "fixture-thread", "status": map[string]string{"type": "idle"}}}
		case "turn/start":
			if expectInputs {
				if err := verifyNativeInputFiles(projectRoot, message.Params); err != nil {
					return err
				}
			}
			turnNumber++
			file, err := os.OpenFile(filepath.Join(projectRoot, "proof.txt"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				return err
			}
			_, writeErr := io.WriteString(file, "executed\n")
			if err := file.Close(); err != nil {
				return err
			}
			if writeErr != nil {
				return writeErr
			}
			turn := map[string]any{"id": fmt.Sprintf("fixture-turn-%d", turnNumber), "status": "inProgress", "items": []any{}}
			if err := encoder.Encode(map[string]any{"id": message.ID, "result": map[string]any{"turn": turn}}); err != nil {
				return err
			}
			turn["status"] = "completed"
			if err := encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "fixture-thread", "turn": turn}}); err != nil {
				return err
			}
			continue
		default:
			return fmt.Errorf("unexpected native call: %s", message.Method)
		}
		if err := encoder.Encode(map[string]any{"id": message.ID, "result": result}); err != nil {
			return err
		}
	}
}

func verifyNativeInputFiles(projectRoot string, raw json.RawMessage) error {
	var params struct {
		Input []struct {
			Text string `json:"text"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return err
	}
	if len(params.Input) != 1 {
		return fmt.Errorf("native turn did not receive input file references")
	}
	text := params.Input[0].Text
	start := strings.LastIndex(text, "\n[")
	if start < 0 {
		return fmt.Errorf("native turn input omitted staged attachment metadata")
	}
	var files []struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(text[start+1:]), &files); err != nil {
		return err
	}
	if len(files) != 2 {
		return fmt.Errorf("native turn expected two staged input files, got %d", len(files))
	}
	seen := map[string]bool{}
	for _, file := range files {
		path := filepath.FromSlash(file.Path)
		if !filepath.IsLocal(path) {
			return fmt.Errorf("native input path left workspace: %s", file.Path)
		}
		name := filepath.Base(path)
		expected := []byte{}
		switch name {
		case "input.bin":
			expected = []byte("\x00fixture attachment\r\n\xff")
		case "empty.txt":
		default:
			return fmt.Errorf("unexpected input filename: %s", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate native input filename: %s", name)
		}
		seen[name] = true
		actual, err := os.ReadFile(filepath.Join(projectRoot, path))
		if err != nil {
			return fmt.Errorf("input %s was not readable before turn/start: %w", name, err)
		}
		sum := sha256.Sum256(actual)
		if !bytes.Equal(actual, expected) || file.Size != int64(len(expected)) || file.SHA256 != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("input %s bytes or metadata were wrong before turn/start", name)
		}
	}
	return nil
}

func verifyStagedInputsBeforeTurn(projectRoot string) error {
	directories, err := filepath.Glob(filepath.Join(projectRoot, ".dots-duet", "attachments", "*"))
	if err != nil {
		return err
	}
	if len(directories) != 1 {
		return fmt.Errorf("native thread/start expected one staged task directory before any turn, got %d", len(directories))
	}
	for _, name := range []string{"input.bin", "empty.txt"} {
		actual, err := os.ReadFile(filepath.Join(directories[0], name))
		if err != nil {
			return fmt.Errorf("attachment %s was absent before turn/start could be called: %w", name, err)
		}
		expected := []byte{}
		if name == "input.bin" {
			expected = []byte("\x00fixture attachment\r\n\xff")
		}
		if !bytes.Equal(actual, expected) {
			return fmt.Errorf("attachment %s had wrong bytes before turn/start could be called", name)
		}
	}
	return nil
}
