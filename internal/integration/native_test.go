package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dots-duet-lite/internal/executor"
	"dots-duet-lite/internal/protocol"
)

// This opt-in check uses the installed Codex account and a real model turn.
// Ordinary go test never starts a model or writes to an existing project.
func TestRealCodexWritesAndDownloadsArtifact(t *testing.T) {
	if os.Getenv("DOTS_DUET_REAL_CODEX") != "1" {
		t.Skip("set DOTS_DUET_REAL_CODEX=1 to authorize a real Codex model test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	project := t.TempDir()
	scope := protocol.Scope{UserID: "acceptance-user", ProfileID: "acceptance-profile", ExecutorID: "acceptance-computer"}
	config := executor.ServiceConfig{
		Scope: scope, StatePath: filepath.Join(t.TempDir(), "executor.sqlite"),
		Projects: []protocol.Project{{ID: "acceptance", Root: project, ApprovalPolicy: "on-request", Sandbox: "workspace-write"}},
		Native:   executor.Options{Stderr: io.Discard},
	}
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	taskIssued := false
	t.Cleanup(func() {
		if service == nil {
			return
		}
		if taskIssued {
			stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			result := service.Execute(stopCtx, protocol.Operation{ID: "native-stop", TaskID: "native-task", Scope: scope, TaskVersion: 2, ProjectID: "acceptance", Action: "task.stop"})
			stop()
			if result.Error != nil {
				t.Errorf("native stop result: %s: %s", result.Stage, result.Error)
			}
		}
		if err := service.Close(); err != nil {
			t.Errorf("close real app-server: %v", err)
		}
	})
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	taskIssued = true
	result := service.Execute(ctx, protocol.Operation{
		ID: "native-create", TaskID: "native-task", Scope: scope, TaskVersion: 1,
		ProjectID: "acceptance", Action: "task.create", Name: "isolated acceptance test",
		Input:     "Work only in the current directory. Create acceptance.txt with exactly these UTF-8 bytes, without BOM: dots-duet-native-ok followed by one LF newline. Use one local write command, then read the file to verify it. Do not inspect other directories, use network, install software, or change anything else. Return a brief final report and stop.",
		Artifacts: []string{"acceptance.txt"},
	})
	if result.Error != nil || result.Stage != "accepted" {
		t.Fatalf("real native turn was not accepted: %+v", result)
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot, err := service.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Tasks) != 1 {
			t.Fatalf("unexpected real task count: %d", len(snapshot.Tasks))
		}
		task := snapshot.Tasks[0]
		if task.Waiting != "" {
			t.Fatalf("real native task requires a user decision; no approval was fabricated: %s", task.Waiting)
		}
		if task.Execution == "completed" {
			break
		}
		if task.Execution == "failed" || task.Execution == "interrupted" || task.Execution == "unknown" {
			t.Fatalf("real turn did not complete: %s", task.Execution)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	data, err := os.ReadFile(filepath.Join(project, "acceptance.txt"))
	if err != nil || string(data) != "dots-duet-native-ok\n" {
		t.Fatalf("actual artifact did not match: %q, %v", data, err)
	}
	download := service.Execute(ctx, protocol.Operation{
		ID: "native-download", TaskID: "native-task", Scope: scope, TaskVersion: 1,
		ProjectID: "acceptance", Action: "files.download", Path: "acceptance.txt",
	})
	if download.Error != nil {
		t.Fatal(download.Error)
	}
	var file struct {
		DataBase64 string `json:"data_base64"`
		SHA256     string `json:"sha256"`
		Truncated  bool   `json:"truncated"`
	}
	if err := json.Unmarshal(download.Data, &file); err != nil {
		t.Fatal(err)
	}
	copy, err := base64.StdEncoding.DecodeString(file.DataBase64)
	sum := sha256.Sum256(copy)
	if err != nil || string(copy) != string(data) || file.Truncated || file.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("real artifact transfer did not match: decoded=%q truncated=%t error=%v", copy, file.Truncated, err)
	}
	t.Logf("real model completed; downloaded %d bytes; sha256=%s", len(copy), file.SHA256)
	if err := service.Close(); err != nil {
		t.Fatal("close before native restart:", err)
	}
	service = nil
	restarted, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	service = restarted
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	continued := service.Execute(ctx, protocol.Operation{
		ID: "native-continue-after-restart", TaskID: "native-task", Scope: scope, TaskVersion: 1,
		ProjectID: "acceptance", Action: "task.continue",
		Input: "Read only acceptance.txt in the current workspace. Verify its exact text is dots-duet-native-ok followed by one LF. Do not modify files, use network, or inspect other directories. Report the verification and stop.",
	})
	if continued.Error != nil || continued.Stage != "accepted" || continued.ThreadID != result.ThreadID || continued.TurnID == result.TurnID {
		t.Fatalf("explicit continuation did not resume the original persisted thread: %+v", continued)
	}
	for {
		snapshot, err := service.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		task := snapshot.Tasks[0]
		if task.Waiting != "" || task.Execution == "failed" || task.Execution == "unknown" || task.Execution == "interrupted" {
			t.Fatalf("continued real task did not complete: %+v", task)
		}
		if task.Execution == "completed" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	unchanged, err := os.ReadFile(filepath.Join(project, "acceptance.txt"))
	if err != nil || string(unchanged) != string(data) {
		t.Fatalf("continued read changed the artifact: %q, %v", unchanged, err)
	}
	t.Log("real app-server restart and explicit continuation preserved the original thread and file")
}
