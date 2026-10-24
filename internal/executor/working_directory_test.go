package executor_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dots-lite/internal/executor"
	"dots-lite/internal/protocol"
)

func TestServiceUsesSelectedExistingWorkingDirectoryForNativeCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	workdir := filepath.Join(config.Projects[0].Root, "child")
	if err := os.Mkdir(workdir, 0700); err != nil {
		t.Fatal(err)
	}
	config.Native.Command = append(config.Native.Command, "expect-cwd", workdir)
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "child-create", TaskID: "child-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "work"}
	if err := json.Unmarshal([]byte(`{"working_directory":"child"}`), &op); err != nil {
		t.Fatal(err)
	}
	result := service.Execute(ctx, op)
	if result.Stage != "accepted" {
		t.Fatalf("selected child directory did not reach native create: %+v", result)
	}
	var task struct {
		WorkingDirectory string `json:"working_directory"`
	}
	if err := json.Unmarshal(result.Data, &task); err != nil || task.WorkingDirectory != "child" {
		t.Fatalf("selected directory not preserved in task: data=%s error=%v", result.Data, err)
	}
}

func TestServiceFilesAndAttachmentsStayInsideTaskWorkingDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	grant := config.Projects[0].Root
	child := filepath.Join(grant, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{filepath.Join(grant, "report.txt"): "grant", filepath.Join(child, "report.txt"): "child"} {
		if err := os.WriteFile(name, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config.Native.Command = append(config.Native.Command, "expect-inputs")
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	file := func(name string, data []byte) protocol.FileUpload {
		sum := sha256.Sum256(data)
		return protocol.FileUpload{Name: name, DataBase64: base64.StdEncoding.EncodeToString(data), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	}
	op := protocol.Operation{ID: "child-inputs", TaskID: "input-task", Scope: config.Scope, ProjectID: "test", WorkingDirectory: "child", TaskVersion: 1, Action: "task.create", Input: "inspect staged input files", InputFiles: []protocol.FileUpload{file("empty.bin", nil), file("binary.bin", []byte{0, 255, 128, 1})}}
	first := service.Execute(ctx, op)
	if first.Stage != "accepted" {
		t.Fatalf("native did not find child attachments: %+v", first)
	}
	read := protocol.Operation{ID: "child-read", TaskID: op.TaskID, Scope: config.Scope, ProjectID: "test", Action: "files.read", Path: "report.txt"}
	result := service.Execute(ctx, read)
	var data struct {
		Text string `json:"text"`
	}
	if result.Stage != "accepted" || json.Unmarshal(result.Data, &data) != nil || data.Text != "child" {
		t.Fatalf("task-relative read used the wrong directory: %+v data=%s", result, result.Data)
	}
	read.ID, read.Path = "child-outside", "../report.txt"
	if result := service.Execute(ctx, read); result.Error == nil || result.Error.Code != "path_denied" {
		t.Fatalf("task read escaped to grant root: %+v", result)
	}
	later := file("later.bin", []byte{4, 5, 6})
	upload := protocol.Operation{ID: "child-later-input", TaskID: op.TaskID, Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "files.upload", Upload: &later}
	if result := service.Execute(ctx, upload); result.Stage != "accepted" {
		t.Fatalf("later upload: %+v", result)
	}
	actual, err := os.ReadFile(filepath.Join(child, ".dots-duet", "attachments", op.TaskID, later.Name))
	if err != nil || string(actual) != string([]byte{4, 5, 6}) {
		t.Fatalf("later upload not in task cwd: bytes=%v error=%v", actual, err)
	}
	if _, err := os.Stat(filepath.Join(grant, ".dots-duet")); !os.IsNotExist(err) {
		t.Fatalf("attachments were also written in the grant root: %v", err)
	}
	if replay := service.Execute(ctx, op); replay.Stage != "accepted" || string(replay.Data) != string(first.Data) {
		t.Fatalf("working directory changed the original operation receipt: %+v", replay)
	}
}

func TestServiceAutomaticArtifactsUseTaskWorkingDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	grant := config.Projects[0].Root
	child := filepath.Join(grant, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{filepath.Join(grant, "report.txt"): "wrong grant report", filepath.Join(child, "report.txt"): "selected child report"} {
		if err := os.WriteFile(name, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "child-artifacts", TaskID: "child-task", Scope: config.Scope, ProjectID: "test", WorkingDirectory: "child", TaskVersion: 1, Action: "task.create", Input: "complete immediately", Artifacts: []string{"report.txt"}}
	if result := service.Execute(ctx, op); result.Stage != "accepted" {
		t.Fatalf("create: %+v", result)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		events, err := service.PendingEvents(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == "artifact/available" {
				var file struct {
					Data string `json:"data_base64"`
				}
				if json.Unmarshal(event.Data, &file) != nil || file.Data != base64.StdEncoding.EncodeToString([]byte("selected child report")) {
					t.Fatalf("automatic artifact used grant root: %s", event.Data)
				}
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no automatic task artifact event")
}

func TestServiceRestoresOriginalWorkingDirectoryWhenContinuingThread(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	child := filepath.Join(config.Projects[0].Root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	config.Native.Command = append(config.Native.Command, "expect-cwd", child)
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "child-start", TaskID: "child-task", Scope: config.Scope, ProjectID: "test", WorkingDirectory: "child", TaskVersion: 1, Action: "task.create", Input: "work"}
	if result := service.Execute(ctx, op); result.Stage != "accepted" {
		t.Fatalf("create: %+v", result)
	}
	op.ID, op.Action, op.TaskVersion, op.WorkingDirectory = "child-stop", "task.stop", 2, ""
	if result := service.Execute(ctx, op); result.Stage != "accepted" {
		t.Fatalf("stop: %+v", result)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	config.Native.Command[len(config.Native.Command)-2] = "expect-resume-cwd"
	service, err = executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op.ID, op.Action, op.TaskVersion, op.Input = "child-continue", "task.continue", 3, "resume child"
	result := service.Execute(ctx, op)
	if result.Stage != "accepted" || result.ThreadID != "thread-1" {
		t.Fatalf("restored child continuation: %+v", result)
	}
	var task protocol.Task
	if json.Unmarshal(result.Data, &task) != nil || task.WorkingDirectory != "child" {
		t.Fatalf("restored directory: %s", result.Data)
	}
}

func TestServiceRejectsWorkingDirectoryChangesForExistingTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	for _, directory := range []string{"child", "other"} {
		if err := os.Mkdir(filepath.Join(config.Projects[0].Root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "immutable-create", TaskID: "immutable-task", Scope: config.Scope, ProjectID: "test", WorkingDirectory: "child", TaskVersion: 1, Action: "task.create", Input: "work"}
	if result := service.Execute(ctx, op); result.Stage != "accepted" {
		t.Fatalf("create: %+v", result)
	}
	for _, action := range []string{"task.continue", "turn.steer", "goal.set", "files.read", "files.upload", "approval.respond", "question.respond", "task.stop"} {
		changed := protocol.Operation{ID: "retarget-" + action, TaskID: op.TaskID, Scope: config.Scope, ProjectID: "test", WorkingDirectory: "other", TaskVersion: 1, Action: action, Input: "retarget", Path: "report.txt", ExpectedTurnID: "turn-1", Goal: json.RawMessage(`{"status":"active","tokenBudget":10}`)}
		if result := service.Execute(ctx, changed); result.Error == nil || result.Error.Code != "invalid_working_directory" {
			t.Fatalf("%s did not reject task retargeting: %+v", action, result)
		}
	}
	state, err := service.Snapshot(ctx)
	if err != nil || len(state.Tasks) != 1 || state.Tasks[0].WorkingDirectory != "child" || state.Tasks[0].Paused {
		t.Fatalf("retarget changed task state: %+v error=%v", state, err)
	}
}

func TestServiceRejectsInvalidOrMissingWorkingDirectoriesBeforeTaskSideEffects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	for i, directory := range []string{"../outside", "child/../other", "/absolute", "C:/absolute", "C:relative", "child\\nested", "child//nested", "missing", "missing/nested"} {
		op := protocol.Operation{ID: "bad-cwd-" + directory, TaskID: "bad-task", Scope: config.Scope, ProjectID: "test", WorkingDirectory: directory, TaskVersion: 1, Action: "task.create", Input: "work"}
		result := service.Execute(ctx, op)
		if result.Stage != "failed" || result.Error == nil || (result.Error.Code != "invalid_working_directory" && result.Error.Code != "working_directory_unavailable") {
			t.Fatalf("invalid directory %d %q: %+v", i, directory, result)
		}
	}
	state, err := service.Snapshot(ctx)
	if err != nil || len(state.Tasks) != 0 {
		t.Fatalf("invalid directory created tasks: %+v error=%v", state, err)
	}
	for _, path := range []string{"missing", ".dots-duet"} {
		if _, err := os.Stat(filepath.Join(config.Projects[0].Root, path)); !os.IsNotExist(err) {
			t.Fatalf("invalid directory created %s: %v", path, err)
		}
	}
	op := protocol.Operation{ID: "valid-default-cwd", TaskID: "default-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "work"}
	result := service.Execute(ctx, op)
	var task protocol.Task
	if result.Stage != "accepted" || result.ThreadID != "thread-1" || json.Unmarshal(result.Data, &task) != nil || task.WorkingDirectory != "." {
		t.Fatalf("invalid choices ran native work or broke the default folder: %+v", result)
	}
}

func TestServiceAllowsSiblingTaskDirectoriesAndRejectsOverlappingLeases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	for _, directory := range []string{"a/nested", "b"} {
		if err := os.MkdirAll(filepath.Join(config.Projects[0].Root, filepath.FromSlash(directory)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"a", "b"} {
		op := protocol.Operation{ID: "start-" + directory, TaskID: "task-" + directory, Scope: config.Scope, ProjectID: "test", WorkingDirectory: directory, TaskVersion: 1, Action: "task.create", Input: "work"}
		if result := service.Execute(ctx, op); result.Stage != "accepted" {
			t.Fatalf("sibling %s: %+v", directory, result)
		}
	}
	for _, directory := range []string{"a", "a/nested", "."} {
		op := protocol.Operation{ID: "overlap-" + directory, TaskID: "overlap-task", Scope: config.Scope, ProjectID: "test", WorkingDirectory: directory, TaskVersion: 1, Action: "task.create", Input: "work"}
		if result := service.Execute(ctx, op); result.Error == nil || result.Error.Code != "conflict" {
			t.Fatalf("overlap %s: %+v", directory, result)
		}
	}
	t.Run("directory alias", func(t *testing.T) {
		if err := os.Symlink("a", filepath.Join(config.Projects[0].Root, "alias")); err != nil {
			t.Skipf("directory symlink unavailable: %v", err)
		}
		op := protocol.Operation{ID: "alias-conflict", TaskID: "alias-task", Scope: config.Scope, ProjectID: "test", WorkingDirectory: "alias", TaskVersion: 1, Action: "task.create", Input: "work"}
		if result := service.Execute(ctx, op); result.Error == nil || result.Error.Code != "conflict" {
			t.Fatalf("alias bypassed the lease: %+v", result)
		}
	})
}

func TestServiceDeniesWorkingDirectoryAndFileLinksOutsideTaskScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	grant, outside := config.Projects[0].Root, t.TempDir()
	child := filepath.Join(grant, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grant, "parent-only.txt"), []byte("parent private bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(grant, "outside")); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(grant, "parent-only.txt"), filepath.Join(child, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "outside-directory", TaskID: "outside-task", Scope: config.Scope, ProjectID: "test", WorkingDirectory: "outside", TaskVersion: 1, Action: "task.create", Input: "work"}
	if result := service.Execute(ctx, op); result.Error == nil || result.Error.Code != "working_directory_unavailable" {
		t.Fatalf("outside task root accepted: %+v", result)
	}
	op.ID, op.TaskID, op.WorkingDirectory = "child-with-link", "child-task", "child"
	if result := service.Execute(ctx, op); result.Stage != "accepted" || result.ThreadID != "thread-1" {
		t.Fatalf("invalid root ran native work: %+v", result)
	}
	read := protocol.Operation{ID: "child-link-read", TaskID: op.TaskID, Scope: config.Scope, ProjectID: "test", Action: "files.read", Path: "escape.txt"}
	if result := service.Execute(ctx, read); result.Error == nil || result.Error.Code != "path_denied" {
		t.Fatalf("file link escaped task scope: %+v data=%s", result, result.Data)
	}
}

func TestServiceRefusesToRebindRestoredTaskToChangedAuthorizedFolder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	child := filepath.Join(config.Projects[0].Root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "old-folder-create", TaskID: "preserved-task", Scope: config.Scope, ProjectID: "test", WorkingDirectory: "child", TaskVersion: 1, Action: "task.create", Input: "work"}
	if result := service.Execute(ctx, op); result.Stage != "accepted" {
		t.Fatalf("create: %+v", result)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	config.Projects[0].Root = t.TempDir()
	if err := os.Mkdir(filepath.Join(config.Projects[0].Root, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	service, err = executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"files.list", "task.continue"} {
		op.ID, op.Action, op.WorkingDirectory, op.Input = "changed-folder-"+action, action, "", "resume"
		if result := service.Execute(ctx, op); result.Error == nil || result.Error.Code != "working_directory_unavailable" {
			t.Fatalf("restored task rebound to a new folder for %s: %+v", action, result)
		}
	}
	state, err := service.Snapshot(ctx)
	if err != nil || len(state.Tasks) != 1 || state.Tasks[0].ID != "preserved-task" || state.Tasks[0].WorkingDirectory != "child" {
		t.Fatalf("old task was cleared or remapped: %+v err=%v", state, err)
	}
}

func TestServiceRejectsReplacementOfHeldTaskDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	child := filepath.Join(config.Projects[0].Root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "held-child-create", TaskID: "held-child-task", Scope: config.Scope, ProjectID: "test", WorkingDirectory: "child", TaskVersion: 1, Action: "task.create", Input: "work"}
	if result := service.Execute(ctx, op); result.Stage != "accepted" {
		t.Fatalf("create: %+v", result)
	}
	if err := os.Rename(child, child+"-old"); err != nil {
		t.Skipf("host denied replacement of a held directory: %v", err)
	}
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "replacement.txt"), []byte("replacement bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	read := protocol.Operation{ID: "held-child-read", TaskID: op.TaskID, Scope: config.Scope, ProjectID: "test", Action: "files.read", Path: "replacement.txt"}
	if result := service.Execute(ctx, read); result.Error == nil || result.Error.Code != "working_directory_unavailable" {
		t.Fatalf("held task silently adopted replacement: %+v data=%s", result, result.Data)
	}
}
