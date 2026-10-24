package executor_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"dots-duet-lite/internal/executor"
	"dots-duet-lite/internal/protocol"
)

func TestServiceCreatesTaskOnceAndPreservesItsThread(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "op-1", TaskID: "task-1", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "write a report", Name: "report"}
	first := service.Execute(ctx, op)
	if first.Stage != "accepted" || first.ThreadID != "thread-1" || first.TurnID != "turn-1" {
		t.Fatalf("create: %+v", first)
	}
	second := service.Execute(ctx, op)
	if string(first.Data) != string(second.Data) || first.ThreadID != second.ThreadID {
		t.Fatalf("repeated operation changed task: first=%+v second=%+v", first, second)
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].ThreadID != "thread-1" || snapshot.Tasks[0].Verification != "pending" {
		t.Fatalf("snapshot: %+v", snapshot)
	}
}

func serviceConfig(t *testing.T) executor.ServiceConfig {
	t.Helper()
	dir := t.TempDir()
	return executor.ServiceConfig{Scope: protocol.Scope{UserID: "user-a", ProfileID: "profile-a", ExecutorID: "machine-a"}, Projects: []protocol.Project{{ID: "test", Root: dir}}, StatePath: filepath.Join(dir, "executor.db"), Native: executor.Options{Command: []string{os.Args[0], "-test.run=^TestServicePeer$"}, Stderr: io.Discard}}
}

func TestServiceFileReadsStayInsideRegisteredRootAndReportTruncation(t *testing.T) {
	ctx := context.Background()
	config := serviceConfig(t)
	if err := os.WriteFile(filepath.Join(config.Projects[0].Root, "report.txt"), []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	op := protocol.Operation{ID: "read-1", Scope: config.Scope, ProjectID: "test", Action: "files.read", Path: "report.txt", Offset: 2, Length: 3}
	r := service.Execute(ctx, op)
	var data struct {
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
		Size      int64  `json:"size"`
	}
	if r.Stage != "accepted" || json.Unmarshal(r.Data, &data) != nil || data.Text != "cde" || !data.Truncated || data.Size != 6 {
		t.Fatalf("read: %+v data=%s", r, r.Data)
	}
	op.ID, op.Path = "read-escape", "../outside.txt"
	if r := service.Execute(ctx, op); r.Error == nil || r.Error.Code != "path_denied" {
		t.Fatalf("outside path: %+v", r)
	}
	op.ID, op.Path = "read-cross", "report.txt"
	op.Scope.UserID = "other"
	if r := service.Execute(ctx, op); r.Error == nil || r.Error.Code != "forbidden" {
		t.Fatalf("cross-scope path: %+v", r)
	}
}

func TestServiceRejectsConflictingWritersAndChangedOperationArguments(t *testing.T) {
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
	op := protocol.Operation{ID: "writer-1", TaskID: "first", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "first objective"}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("first writer: %+v", r)
	}
	changed := op
	changed.Input = "different objective"
	if r := service.Execute(ctx, changed); r.Error == nil || r.Error.Code != "conflict" {
		t.Fatalf("changed retry: %+v", r)
	}
	op.ID, op.TaskID = "writer-2", "second"
	if r := service.Execute(ctx, op); r.Error == nil || r.Error.Code != "conflict" {
		t.Fatalf("overlapping writer: %+v", r)
	}
}

func TestServiceStopsTheTargetAndPersistsEventsUntilAcknowledged(t *testing.T) {
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
	op := protocol.Operation{ID: "start-stop", TaskID: "target", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "work"}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	op.ID, op.Action, op.TaskVersion = "stop-1", "task.stop", 2
	r := service.Execute(ctx, op)
	var task protocol.Task
	if r.Stage != "accepted" || json.Unmarshal(r.Data, &task) != nil || !task.Paused || task.Version != 2 || task.Execution == "running" {
		t.Fatalf("stop: %+v data=%s", r, r.Data)
	}
	var events []protocol.Event
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		events, err = service.PendingEvents(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(events) == 0 || events[len(events)-1].Type != "turn/completed" {
		t.Fatalf("events: %+v", events)
	}
	if err := service.AckEvents(ctx, events[len(events)-1].Sequence+100); err == nil {
		t.Fatal("acknowledgement deleted events the executor never emitted")
	}
	if err := service.AckEvents(ctx, events[len(events)-1].Sequence); err != nil {
		t.Fatal(err)
	}
	remaining, err := service.PendingEvents(ctx, 0)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("ack remaining=%+v error=%v", remaining, err)
	}
	op.ID, op.TaskID, op.TaskVersion, op.Action = "new-after-stop", "another", 1, "task.create"
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("project was not released after confirmed stop: %+v", r)
	}
}

func TestServiceContinuesTheSameThreadAndRejectsAnExpiredSteer(t *testing.T) {
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
	op := protocol.Operation{ID: "continue-create", TaskID: "target", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "work"}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	op.ID, op.Action, op.TaskVersion = "continue-stop", "task.stop", 2
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("stop: %+v", r)
	}
	op.ID, op.Action, op.TaskVersion, op.Input = "continue-resume", "task.continue", 3, "continue with this requirement"
	r := service.Execute(ctx, op)
	if r.Stage != "accepted" || r.ThreadID != "thread-1" || r.TurnID != "turn-2" {
		t.Fatalf("continue: %+v", r)
	}
	op.ID, op.Action, op.ExpectedTurnID = "stale-steer", "turn.steer", "turn-1"
	if r := service.Execute(ctx, op); r.Error == nil || r.Error.Code != "stale_turn" {
		t.Fatalf("expired steer: %+v", r)
	}
	op.ID, op.ExpectedTurnID = "current-steer", "turn-2"
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("current steer: %+v", r)
	}
}

func TestServiceForwardsOnlyTheLiveExplicitApproval(t *testing.T) {
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
	op := protocol.Operation{ID: "approval-start", TaskID: "approval-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "request approval"}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	var request struct {
		ID         string `json:"request_id"`
		InstanceID string `json:"instance_id"`
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		events, err := service.PendingEvents(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Type == "item/commandExecution/requestApproval" {
				if err := json.Unmarshal(e.Data, &request); err != nil {
					t.Fatal(err)
				}
			}
		}
		if request.ID != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if request.ID == "" {
		t.Fatal("approval event was not retained")
	}
	op.ID, op.Action, op.RequestID, op.InstanceID, op.Reply = "approval-reply", "approval.respond", request.ID, request.InstanceID, json.RawMessage(`{"decision":"accept"}`)
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("approval reply: %+v", r)
	}
	op.ID = "approval-replay"
	if r := service.Execute(ctx, op); r.Error == nil || r.Error.Code != "request_invalid" {
		t.Fatalf("repeated approval was accepted: %+v", r)
	}
}

func TestStopDuringThreadCreationPreventsTheOldTurnFromStarting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := serviceConfig(t)
	config.Native.Command = append(config.Native.Command, "pause-create", listener.Addr().String())
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	control, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	op := protocol.Operation{ID: "blocked-create", TaskID: "pause-target", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "must never run after pause"}
	created := make(chan protocol.Result, 1)
	go func() { created <- service.Execute(ctx, op) }()
	var signal [1]byte
	if _, err := io.ReadFull(control, signal[:]); err != nil {
		t.Fatal(err)
	}
	stop := op
	stop.ID, stop.Action, stop.TaskVersion = "early-stop", "task.stop", 2
	if r := service.Execute(ctx, stop); r.Stage != "unknown" {
		t.Fatalf("unresolved create stop should retain uncertainty: %+v", r)
	}
	if _, err := control.Write([]byte("r")); err != nil {
		t.Fatal(err)
	}
	result := <-created
	if result.Stage != "failed" || result.Error == nil || result.Error.Code != "paused" {
		t.Fatalf("old turn started after pause: %+v", result)
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 1 || !snapshot.Tasks[0].Paused || snapshot.Tasks[0].Version != 2 || snapshot.Tasks[0].TurnID != "" {
		t.Fatalf("pause was overwritten: %+v", snapshot.Tasks)
	}
}

func TestStopDuringTurnSubmissionIsAppliedWhenTheTurnIDArrives(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := serviceConfig(t)
	config.Native.Command = append(config.Native.Command, "pause-turn", listener.Addr().String())
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	control, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	op := protocol.Operation{ID: "pending-turn", TaskID: "pause-turn-target", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "work"}
	created := make(chan protocol.Result, 1)
	go func() { created <- service.Execute(ctx, op) }()
	var signal [1]byte
	if _, err := io.ReadFull(control, signal[:]); err != nil {
		t.Fatal(err)
	}
	stop := op
	stop.ID, stop.Action, stop.TaskVersion = "stop-pending-turn", "task.stop", 2
	_ = service.Execute(ctx, stop)
	result := <-created
	if result.Error == nil || result.Error.Code != "paused" {
		t.Fatalf("late turn acknowledgement ignored pause: %+v", result)
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 1 || !snapshot.Tasks[0].Paused || snapshot.Tasks[0].Version != 2 || (snapshot.Tasks[0].Execution != "stopped" && snapshot.Tasks[0].Execution != "interrupted") {
		t.Fatalf("late turn was not interrupted: %+v", snapshot.Tasks)
	}
}

func TestServiceRestartPreservesReceiptAndInvalidatesOldRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	first, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "persisted-start", TaskID: "persisted-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "request approval"}
	receipt := first.Execute(ctx, op)
	if receipt.Stage != "accepted" {
		t.Fatalf("create: %+v", receipt)
	}
	var oldRequest struct {
		ID       string `json:"request_id"`
		Instance string `json:"instance_id"`
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		events, err := first.PendingEvents(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Type == "item/commandExecution/requestApproval" {
				_ = json.Unmarshal(e.Data, &oldRequest)
			}
		}
		if oldRequest.ID != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if oldRequest.ID == "" {
		t.Fatal("missing approval before restart")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	recovered := second.Execute(ctx, op)
	if recovered.Stage != receipt.Stage || string(recovered.Data) != string(receipt.Data) {
		t.Fatalf("restart changed original operation receipt: %+v", recovered)
	}
	snapshot, err := second.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].Execution != "unknown" || snapshot.Tasks[0].Waiting != "" {
		t.Fatalf("restart fabricated live execution or old approval: %+v", snapshot.Tasks)
	}
	op.ID, op.Action, op.RequestID, op.InstanceID, op.Reply = "stale-reply", "approval.respond", oldRequest.ID, oldRequest.Instance, json.RawMessage(`{"decision":"accept"}`)
	if r := second.Execute(ctx, op); r.Error == nil || r.Error.Code != "request_invalid" {
		t.Fatalf("old instance approval revived: %+v", r)
	}
}

func TestServiceNeverReadsAnOutsideTargetDuringSymlinkReplacement(t *testing.T) {
	config := serviceConfig(t)
	root := config.Projects[0].Root
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("authorized"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("must-not-transfer"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "switch.txt")
	if err := os.Symlink("inside.txt", link); err != nil {
		t.Skipf("symlink creation unavailable on this host: %v", err)
	}
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	op := protocol.Operation{ID: "link-first", Scope: config.Scope, ProjectID: "test", Action: "files.read", Path: "switch.txt"}
	if r := service.Execute(context.Background(), op); r.Stage != "accepted" {
		t.Fatalf("authorized relative symlink: %+v", r)
	}
	done := make(chan struct{})
	var swapping sync.WaitGroup
	swapping.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = os.Remove(link)
			_ = os.Symlink(outside, link)
			_ = os.Remove(link)
			_ = os.Symlink("inside.txt", link)
		}
	})
	defer func() { close(done); swapping.Wait() }()
	for i := 0; i < 50; i++ {
		op.ID = fmt.Sprintf("link-race-%d", i)
		r := service.Execute(context.Background(), op)
		if r.Stage == "accepted" {
			var data struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(r.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Text != "authorized" {
				t.Fatalf("transferred outside bytes: %q", data.Text)
			}
		}
	}
}

func TestServiceRejectsOversizedFilesAndTransfersBinaryBytes(t *testing.T) {
	config := serviceConfig(t)
	root := config.Projects[0].Root
	if err := os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0, 1, 255}, 0600); err != nil {
		t.Fatal(err)
	}
	large, err := os.Create(filepath.Join(root, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate((8 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	large.Close()
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	op := protocol.Operation{ID: "binary-read", Scope: config.Scope, ProjectID: "test", Action: "files.download", Path: "binary.bin"}
	r := service.Execute(context.Background(), op)
	var data struct {
		Base64    string `json:"data_base64"`
		Size      int64  `json:"size"`
		Truncated bool   `json:"truncated"`
	}
	if r.Stage != "accepted" || json.Unmarshal(r.Data, &data) != nil || data.Base64 != "AAH/" || data.Size != 3 || data.Truncated {
		t.Fatalf("binary download: %+v data=%s", r, r.Data)
	}
	op.ID, op.Path = "large-read", "large.bin"
	if r := service.Execute(context.Background(), op); r.Error == nil || r.Error.Code != "file_too_large" {
		t.Fatalf("oversized download: %+v", r)
	}
}

func TestServiceReadsAnOriginalOperationReceiptWithoutResubmitting(t *testing.T) {
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
	op := protocol.Operation{ID: "receipt-original", TaskID: "receipt-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "work"}
	original := service.Execute(ctx, op)
	if original.Stage != "accepted" {
		t.Fatalf("create: %+v", original)
	}
	query := protocol.Operation{ID: "receipt-read", Scope: config.Scope, Action: "operation.read", RequestID: op.ID, TaskID: op.TaskID}
	result := service.Execute(ctx, query)
	var recovered protocol.Result
	if result.Stage != "accepted" || json.Unmarshal(result.Data, &recovered) != nil || recovered.OperationID != op.ID || recovered.ThreadID != original.ThreadID || string(recovered.Data) != string(original.Data) {
		t.Fatalf("read receipt: %+v data=%s", result, result.Data)
	}
	query.ID, query.TaskID = "receipt-wrong-task", "another-task"
	if r := service.Execute(ctx, query); r.Error == nil || r.Error.Code != "forbidden" {
		t.Fatalf("wrong task receipt read: %+v", r)
	}
}

func TestServiceDistinguishesNativeRejectionFromUnknownSubmission(t *testing.T) {
	for _, stage := range []string{"thread", "turn"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			config := serviceConfig(t)
			input := "work"
			if stage == "thread" {
				config.Projects[0].Model = "reject-model"
			} else {
				input = "reject turn"
			}
			service, err := executor.NewService(config)
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			if err := service.StartNative(ctx); err != nil {
				t.Fatal(err)
			}
			r := service.Execute(ctx, protocol.Operation{ID: "rejected", TaskID: "rejected-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: input})
			if r.Stage != "failed" || r.Error == nil || r.Error.Code != "native" {
				t.Fatalf("explicit native rejection was reported as uncertain: %+v", r)
			}
			if stage == "turn" && r.ThreadID != "thread-1" {
				t.Fatalf("successful thread creation was lost: %+v", r)
			}
		})
	}
}

func TestStoppingAnAlreadyIdleCompletedTaskDoesNotInterruptItsOldTurn(t *testing.T) {
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
	op := protocol.Operation{ID: "completed-start", TaskID: "completed-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "complete immediately"}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		state, err := service.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Tasks) == 1 && state.Tasks[0].Execution == "completed" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	op.ID, op.Action, op.TaskVersion = "completed-stop", "task.stop", 2
	r := service.Execute(ctx, op)
	var task protocol.Task
	if r.Stage != "accepted" || json.Unmarshal(r.Data, &task) != nil || !task.Paused || task.Execution != "completed" {
		t.Fatalf("stop idle completed turn: %+v data=%s", r, r.Data)
	}
}

func TestServicePublishesDeclaredArtifactsBeforeQuiescenceAndReleasesProject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := serviceConfig(t)
	if err := os.WriteFile(filepath.Join(config.Projects[0].Root, "report.txt"), []byte("verified report"), 0600); err != nil {
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
	op := protocol.Operation{ID: "artifact-start", TaskID: "artifact-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "complete immediately", Artifacts: []string{"report.txt", "missing.txt"}}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	var events []protocol.Event
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		events, err = service.PendingEvents(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) > 0 && events[len(events)-1].Type == "task/quiescent" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	var available, failed, quiet int64
	for _, event := range events {
		switch event.Type {
		case "artifact/available":
			var artifact struct {
				Path    string `json:"path"`
				Data    string `json:"data_base64"`
				Version int64  `json:"task_version"`
			}
			if json.Unmarshal(event.Data, &artifact) != nil || artifact.Path != "report.txt" || artifact.Data != "dmVyaWZpZWQgcmVwb3J0" || artifact.Version != 1 {
				t.Fatalf("artifact: %s", event.Data)
			}
			available = event.Sequence
		case "artifact/failed":
			var artifact struct {
				Path  string         `json:"path"`
				Error protocol.Error `json:"error"`
			}
			if json.Unmarshal(event.Data, &artifact) != nil || artifact.Path != "missing.txt" || artifact.Error.Code != "not_found" {
				t.Fatalf("missing artifact: %s", event.Data)
			}
			failed = event.Sequence
		case "task/quiescent":
			var state struct {
				Goal   string `json:"goal_status"`
				Native string `json:"native_status"`
			}
			if json.Unmarshal(event.Data, &state) != nil || state.Goal != "none" || state.Native != "idle" {
				t.Fatalf("quiet state: %s", event.Data)
			}
			quiet = event.Sequence
		}
	}
	if available == 0 || failed == 0 || quiet <= available || quiet <= failed {
		t.Fatalf("artifact events must precede final quiet event: %+v", events)
	}
	op.ID, op.TaskID, op.Input, op.Artifacts = "next-start", "next-task", "write next report", nil
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("completed project remains occupied: %+v", r)
	}
}

func TestServiceDoesNotReleaseAProjectWithoutProvenNativeQuiescence(t *testing.T) {
	for _, input := range []string{"complete with active goal", "complete with unknown goal", "complete with missing goal", "complete with unloaded thread"} {
		t.Run(input, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
			op := protocol.Operation{ID: "uncertain-start", TaskID: "uncertain-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: input}
			if r := service.Execute(ctx, op); r.Stage != "accepted" {
				t.Fatalf("create: %+v", r)
			}
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				snapshot, err := service.Snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(snapshot.Tasks) == 1 && snapshot.Tasks[0].Execution == "completed" {
					break
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			events, err := service.PendingEvents(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == "task/quiescent" || event.Type == "artifact/available" {
					t.Fatalf("unproven quiescence emitted: %+v", event)
				}
			}
			op.ID, op.TaskID, op.Input = "competing-start", "competing-task", "write next report"
			if r := service.Execute(ctx, op); r.Stage != "failed" || r.Error == nil || r.Error.Code != "conflict" {
				t.Fatalf("unproven project released: %+v", r)
			}
		})
	}
}

func TestServiceRequiresABoundedBudgetBeforeActivatingNativeGoal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	op := protocol.Operation{ID: "bounded-start", TaskID: "bounded-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "write report"}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	op.ID, op.Action, op.Goal = "unbounded-goal", "goal.set", json.RawMessage(`{"objective":"finish report","status":"active"}`)
	if r := service.Execute(ctx, op); r.Stage != "failed" || r.Error == nil || r.Error.Code != "invalid" {
		t.Fatalf("unbounded goal accepted: %+v", r)
	}
	op.ID, op.Goal = "bounded-goal", json.RawMessage(`{"objective":"finish report","status":"active","tokenBudget":5000,"origin":"user"}`)
	r := service.Execute(ctx, op)
	var response struct {
		Goal struct {
			Status string `json:"status"`
			Budget int64  `json:"tokenBudget"`
			Origin string `json:"origin"`
		} `json:"goal"`
	}
	if r.Stage != "accepted" || json.Unmarshal(r.Data, &response) != nil || response.Goal.Status != "active" || response.Goal.Budget != 5000 || response.Goal.Origin != "automatic" {
		t.Fatalf("bounded goal mutation: %+v data=%s", r, r.Data)
	}
}

func TestServiceUploadsBoundInputAttachmentsWithoutReplacingExistingFiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	op := protocol.Operation{ID: "upload-start", TaskID: "upload-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "read attachments"}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	for i, data := range [][]byte{{}, {0, 255, 128, 1}} {
		name := fmt.Sprintf("input-%d.bin", i)
		sum := sha256.Sum256(data)
		op.ID, op.Action, op.Upload = fmt.Sprintf("upload-%d", i), "files.upload", &protocol.FileUpload{Name: name, DataBase64: base64.StdEncoding.EncodeToString(data), SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
		r := service.Execute(ctx, op)
		if r.Stage != "accepted" {
			t.Fatalf("upload %d: %+v", i, r)
		}
		var uploaded struct {
			Path string `json:"path"`
			Hash string `json:"sha256"`
			Size int64  `json:"size"`
		}
		if json.Unmarshal(r.Data, &uploaded) != nil || uploaded.Path != filepath.ToSlash(filepath.Join(".dots-duet", "attachments", "upload-task", name)) || uploaded.Hash != op.Upload.SHA256 || uploaded.Size != int64(len(data)) {
			t.Fatalf("upload metadata: %s", r.Data)
		}
		first := r
		if replay := service.Execute(ctx, op); replay.Stage != "accepted" || string(replay.Data) != string(first.Data) {
			t.Fatalf("same upload replay: %+v", replay)
		}
		read := service.Execute(ctx, protocol.Operation{ID: fmt.Sprintf("read-upload-%d", i), TaskID: op.TaskID, Scope: op.Scope, ProjectID: op.ProjectID, Action: "files.download", Path: uploaded.Path})
		var fetched struct {
			Data string `json:"data_base64"`
		}
		if read.Stage != "accepted" || json.Unmarshal(read.Data, &fetched) != nil || fetched.Data != op.Upload.DataBase64 {
			t.Fatalf("uploaded bytes: %+v data=%s", read, read.Data)
		}
		op.ID = fmt.Sprintf("replace-upload-%d", i)
		if r := service.Execute(ctx, op); r.Stage != "failed" || r.Error == nil || r.Error.Code != "file_exists" {
			t.Fatalf("existing attachment overwritten: %+v", r)
		}
	}
}

func TestServiceRejectsAnUnboundOrCorruptAttachmentBeforeWritingIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := serviceConfig(t)
	config.Projects = append(config.Projects, protocol.Project{ID: "other", Root: t.TempDir()})
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "invalid-upload-start", TaskID: "upload-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "read attachments"}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	sum := sha256.Sum256([]byte("input"))
	valid := protocol.FileUpload{Name: "input.txt", DataBase64: "aW5wdXQ=", SHA256: hex.EncodeToString(sum[:]), Size: 5}
	tests := []struct {
		name   string
		change func(*protocol.Operation)
		code   string
	}{
		{"hash", func(o *protocol.Operation) { o.Upload.SHA256 = "bad" }, "integrity_mismatch"},
		{"size", func(o *protocol.Operation) { o.Upload.Size = 6 }, "integrity_mismatch"},
		{"base64", func(o *protocol.Operation) { o.Upload.DataBase64 = "invalid!" }, "invalid_upload"},
		{"traversal", func(o *protocol.Operation) { o.Upload.Name = "../outside.txt" }, "invalid_upload"},
		{"backslash", func(o *protocol.Operation) { o.Upload.Name = `..\outside.txt` }, "invalid_upload"},
		{"absolute", func(o *protocol.Operation) { o.Upload.Name = "/outside.txt" }, "invalid_upload"},
		{"alternate stream", func(o *protocol.Operation) { o.Upload.Name = "input.txt:stream" }, "invalid_upload"},
		{"other task", func(o *protocol.Operation) { o.TaskID = "other-task" }, "task_not_found"},
		{"other project", func(o *protocol.Operation) { o.ProjectID = "other" }, "forbidden"},
		{"old version", func(o *protocol.Operation) { o.TaskVersion = 0 }, "stale_version"},
		{"other scope", func(o *protocol.Operation) { o.Scope.ProfileID = "other-profile" }, "forbidden"},
	}
	for i, test := range tests {
		candidate := op
		copy := valid
		candidate.ID, candidate.Action, candidate.Upload = fmt.Sprintf("bad-upload-%d", i), "files.upload", &copy
		test.change(&candidate)
		if r := service.Execute(ctx, candidate); r.Stage != "failed" || r.Error == nil || r.Error.Code != test.code {
			t.Fatalf("%s attachment: %+v", test.name, r)
		}
	}
	op.ID, op.Action, op.Upload = "valid-after-invalid", "files.upload", &valid
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("invalid upload already created target: %+v", r)
	}
}

func TestServiceNeverUploadsOutsideTheProjectDuringSymlinkReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := serviceConfig(t)
	root, outside := config.Projects[0].Root, t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "allowed-attachments"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".dots-duet"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ".dots-duet", "attachments")
	if err := os.Symlink(filepath.Join("..", "allowed-attachments"), link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "race-upload-start", TaskID: "upload-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "read attachments"}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	done := make(chan struct{})
	var swapping sync.WaitGroup
	swapping.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = os.Remove(link)
			_ = os.Symlink(outside, link)
			_ = os.Remove(link)
			_ = os.Symlink(filepath.Join("..", "allowed-attachments"), link)
		}
	})
	defer func() { close(done); swapping.Wait() }()
	sum := sha256.Sum256([]byte("input"))
	for i := 0; i < 50; i++ {
		op.ID, op.Action, op.Upload = fmt.Sprintf("race-upload-%d", i), "files.upload", &protocol.FileUpload{Name: fmt.Sprintf("input-%d.txt", i), DataBase64: "aW5wdXQ=", SHA256: hex.EncodeToString(sum[:]), Size: 5}
		r := service.Execute(ctx, op)
		if r.Stage != "accepted" && (r.Error == nil || (r.Error.Code != "path_denied" && r.Error.Code != "conflict")) {
			t.Fatalf("racing upload: %+v", r)
		}
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("upload escaped authorized project: %v", entries)
	}
}

func TestServicePersistsUnknownTaskEventWhenTheNativeProcessExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := serviceConfig(t)
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "crashing-start", TaskID: "crashing-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "exit process"}
	r := service.Execute(ctx, op)
	if r.Stage != "accepted" && r.Stage != "unknown" {
		t.Fatalf("crashing receipt: %+v", r)
	}
	var event protocol.Event
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		events, err := service.PendingEvents(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, current := range events {
			if current.Type == "task/native_exited" {
				event = current
			}
		}
		if event.Sequence != 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	var exit struct {
		Task   protocol.Task  `json:"task"`
		Error  protocol.Error `json:"error"`
		Native string         `json:"native_status"`
	}
	if event.TaskID != op.TaskID || json.Unmarshal(event.Data, &exit) != nil || exit.Task.Execution != "unknown" || exit.Error.Code != "native_exited" || exit.Error.Message == "" || exit.Native != "unknown" {
		_ = service.Close()
		t.Fatalf("native exit event: %+v data=%s", event, event.Data)
	}
	_ = service.Close()
	restarted, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	events, err := restarted.PendingEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Sequence != event.Sequence || string(events[0].Data) != string(event.Data) {
		t.Fatalf("native exit event lost across restart: %+v", events)
	}
	snapshot, err := restarted.Snapshot(ctx)
	if err != nil || len(snapshot.Tasks) != 1 || snapshot.Tasks[0].Execution != "unknown" {
		t.Fatalf("native exit task: %+v err=%v", snapshot, err)
	}
}

func TestServiceKeepsMachineReadyWhenARegisteredProjectIsUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := serviceConfig(t)
	root := filepath.Join(t.TempDir(), "temporarily-unavailable")
	config.Projects[0].Root = root
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatalf("unavailable project disabled whole service: %v", err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil || !snapshot.Initialized || len(snapshot.Projects) != 1 || snapshot.Projects[0].Available {
		t.Fatalf("separate readiness: %+v err=%v", snapshot, err)
	}
	op := protocol.Operation{ID: "missing-project-start", TaskID: "missing-project-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "write report"}
	if r := service.Execute(ctx, op); r.Stage != "failed" || r.Error == nil || r.Error.Code != "project_unavailable" {
		t.Fatalf("unavailable project started: %+v", r)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("executor created unavailable root: %v", err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot, err = service.Snapshot(ctx)
	if err != nil || !snapshot.Projects[0].Available {
		t.Fatalf("restored project unavailable: %+v err=%v", snapshot, err)
	}
	op.ID = "restored-project-start"
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("restored project start: %+v", r)
	}
}

func TestServiceDoesNotAuthorizeAReplacementOfTheRegisteredProjectDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := serviceConfig(t)
	parent := t.TempDir()
	root, original := filepath.Join(parent, "project"), filepath.Join(parent, "original-project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	config.Projects[0].Root = root
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	op := protocol.Operation{ID: "directory-start", TaskID: "directory-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "write report"}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	if err := os.Rename(root, original); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows denied replacement of the held root directory: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "replacement.txt"), []byte("unregistered replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil || !snapshot.Initialized || snapshot.Projects[0].Available {
		t.Fatalf("replacement authorized: %+v err=%v", snapshot, err)
	}
	read := protocol.Operation{ID: "directory-read", Scope: config.Scope, ProjectID: "test", Action: "files.read", Path: "replacement.txt"}
	if r := service.Execute(ctx, read); r.Stage != "failed" || r.Error == nil || r.Error.Code != "project_unavailable" {
		t.Fatalf("replacement file read: %+v", r)
	}
	op.ID, op.Action, op.TaskVersion = "directory-stop", "task.stop", 2
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("unavailable project prevented stop: %+v", r)
	}
}

func TestServiceFailsClosedWithoutPartialRequestStateWhenEventPersistenceFails(t *testing.T) {
	for _, table := range []string{"executor_requests", "executor_events"} {
		t.Run(table, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			config := serviceConfig(t)
			service, err := executor.NewService(config)
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
			defer hub.Close()
			runDone := make(chan error, 1)
			go func() {
				runDone <- service.Run(ctx, "ws"+strings.TrimPrefix(hub.URL, "http")+"/v1/executor", "test-token")
			}()
			defer func() { cancel(); <-runDone }()
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				snapshot, err := service.Snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.Initialized {
					break
				}
				time.Sleep(time.Millisecond)
			}
			db, err := sql.Open("sqlite", config.StatePath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TRIGGER injected_storage_failure BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(FAIL, 'injected critical persistence failure'); END`); err != nil {
				t.Fatal(err)
			}
			op := protocol.Operation{ID: "storage-fault-create", TaskID: "storage-fault-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "request approval"}
			service.Execute(ctx, op)
			var snapshot protocol.Snapshot
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				snapshot, err = service.Snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !snapshot.Initialized && strings.Contains(snapshot.Error, "injected critical persistence failure") {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if snapshot.Initialized || !strings.Contains(snapshot.Error, "injected critical persistence failure") {
				t.Fatalf("critical failure did not stop or retain its cause: %+v", snapshot)
			}
			if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].Waiting != "" {
				t.Fatalf("partially persisted native request became visible: %+v", snapshot.Tasks)
			}
			time.Sleep(1200 * time.Millisecond)
			after, err := service.Snapshot(ctx)
			if err != nil || after.Initialized || after.InstanceID != snapshot.InstanceID || !strings.Contains(after.Error, "injected critical persistence failure") {
				t.Fatalf("native recovery cleared critical storage failure: %+v err=%v", after, err)
			}
			op.ID, op.Action, op.Goal = "storage-fault-write", "goal.set", json.RawMessage(`{"status":"active","tokenBudget":1000}`)
			if r := service.Execute(ctx, op); r.Stage != "failed" || r.Error == nil || r.Error.Code != "event_storage" {
				t.Fatalf("business write accepted after critical event loss: %+v", r)
			}
			events, err := service.PendingEvents(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == "item/commandExecution/requestApproval" {
					t.Fatalf("uncommitted request emitted: %+v", event)
				}
			}
		})
	}
}

func TestStopDoesNotConfirmOrReleaseTheProjectBeforeAnOlderGoalActivationSettles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := serviceConfig(t)
	config.Native.Command = append(config.Native.Command, "pause-goal", listener.Addr().String())
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	control, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	op := protocol.Operation{ID: "before-goal-create", TaskID: "goal-race-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "complete immediately"}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("create: %+v", r)
	}
	quiet := false
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && !quiet; {
		events, err := service.PendingEvents(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == "task/quiescent" {
				quiet = true
			}
		}
		if !quiet {
			time.Sleep(time.Millisecond)
		}
	}
	if !quiet {
		t.Fatal("initial turn never became quiet")
	}
	activation := op
	activation.ID, activation.Action, activation.Goal = "older-goal-activation", "goal.set", json.RawMessage(`{"objective":"continue native goal","status":"active","tokenBudget":1000}`)
	activated := make(chan protocol.Result, 1)
	go func() { activated <- service.Execute(ctx, activation) }()
	var signal [1]byte
	if _, err := io.ReadFull(control, signal[:]); err != nil {
		t.Fatal(err)
	}
	stop := op
	stop.ID, stop.Action, stop.TaskVersion = "stop-before-old-goal", "task.stop", 2
	if r := service.Execute(ctx, stop); r.Stage != "unknown" || r.Error == nil || r.Error.Code != "stop_unconfirmed" {
		t.Fatalf("stop confirmed while older Goal can still activate: %+v", r)
	}
	competing := op
	competing.ID, competing.TaskID, competing.Input = "competing-before-settle", "competing-task", "write another report"
	if r := service.Execute(ctx, competing); r.Stage != "failed" || r.Error == nil || r.Error.Code != "conflict" {
		t.Fatalf("project released before older Goal settled: %+v", r)
	}
	if _, err := control.Write([]byte("r")); err != nil {
		t.Fatal(err)
	}
	if r := <-activated; r.Stage != "failed" || r.Error == nil || r.Error.Code != "paused" {
		t.Fatalf("older Goal ignored newer pause: %+v", r)
	}
	read := stop
	read.ID, read.Action = "read-after-late-pause", "goal.get"
	r := service.Execute(ctx, read)
	var state struct {
		Goal struct {
			Status string `json:"status"`
		} `json:"goal"`
	}
	if r.Stage != "accepted" || json.Unmarshal(r.Data, &state) != nil || state.Goal.Status != "paused" {
		t.Fatalf("late Goal was left active: %+v data=%s", r, r.Data)
	}
	competing.ID = "competing-after-settle"
	if r := service.Execute(ctx, competing); r.Stage != "accepted" {
		t.Fatalf("verified late pause kept project occupied: %+v", r)
	}
}

func TestServiceRunRestartsTheFailedNativeInstanceWithoutReplayingItsUnknownWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer hub.Close()
	config := serviceConfig(t)
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	runDone := make(chan error, 1)
	go func() {
		runDone <- service.Run(ctx, "ws"+strings.TrimPrefix(hub.URL, "http")+"/v1/executor", "test-token")
	}()
	defer func() { cancel(); <-runDone }()
	var first protocol.Snapshot
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		first, err = service.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if first.Initialized {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !first.Initialized {
		t.Fatalf("native never ready: %+v", first)
	}
	op := protocol.Operation{ID: "restart-unknown-write", TaskID: "restart-unknown-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "exit without receipt"}
	original := service.Execute(ctx, op)
	if original.Stage != "unknown" {
		t.Fatalf("unacknowledged write: %+v", original)
	}
	var recovered protocol.Snapshot
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		recovered, err = service.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if recovered.Initialized && recovered.InstanceID != first.InstanceID {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !recovered.Initialized || recovered.InstanceID == first.InstanceID {
		t.Fatalf("failed native was never replaced: %+v", recovered)
	}
	if len(recovered.Tasks) != 1 || recovered.Tasks[0].Execution != "unknown" || recovered.Tasks[0].ThreadID != original.ThreadID {
		t.Fatalf("recovery changed uncertain task: %+v", recovered.Tasks)
	}
	if replay := service.Execute(ctx, op); replay.Stage != "unknown" || string(replay.Data) != string(original.Data) {
		t.Fatalf("unknown write was replayed: %+v", replay)
	}
	op.ID, op.TaskID, op.Input = "restart-competing-write", "restart-competing-task", "write report"
	if r := service.Execute(ctx, op); r.Stage != "failed" || r.Error == nil || r.Error.Code != "conflict" {
		t.Fatalf("recovery released uncertain project occupancy: %+v", r)
	}
	time.Sleep(100 * time.Millisecond)
	stable, err := service.Snapshot(ctx)
	if err != nil || !stable.Initialized || stable.InstanceID != recovered.InstanceID {
		t.Fatalf("recovered native replayed crashing write: %+v err=%v", stable, err)
	}
}

func TestServiceNativeRestartBudgetSurvivesTransportReconnectsAndServiceRecreation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer hub.Close()
	config := serviceConfig(t)
	for i := 1; i < 5; i++ {
		config.Projects = append(config.Projects, protocol.Project{ID: fmt.Sprintf("project-%d", i), Root: t.TempDir()})
	}
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	var runDone chan error
	var stopRun context.CancelFunc
	startRun := func() {
		runCtx, stop := context.WithCancel(ctx)
		stopRun = stop
		runDone = make(chan error, 1)
		s := service
		go func() {
			runDone <- s.Run(runCtx, "ws"+strings.TrimPrefix(hub.URL, "http")+"/v1/executor", "test-token")
		}()
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
			snapshot, err := service.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Initialized {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("native never became ready")
	}
	defer func() {
		if stopRun != nil {
			stopRun()
			<-runDone
		}
		_ = service.Close()
	}()
	startRun()
	for i := 0; i < 5; i++ {
		before, err := service.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		op := protocol.Operation{ID: fmt.Sprintf("budget-crash-%d", i), TaskID: fmt.Sprintf("budget-task-%d", i), Scope: config.Scope, ProjectID: config.Projects[i].ID, TaskVersion: 1, Action: "task.create", Input: "exit without receipt"}
		if r := service.Execute(ctx, op); r.Stage != "unknown" {
			t.Fatalf("crash %d: %+v", i, r)
		}
		var snapshot protocol.Snapshot
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			snapshot, err = service.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if i < 3 && snapshot.Initialized && snapshot.InstanceID != before.InstanceID {
				break
			}
			if i >= 3 && strings.Contains(snapshot.Error, "native restart budget exhausted (3/3)") {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if i < 3 {
			if !snapshot.Initialized || snapshot.InstanceID == before.InstanceID {
				t.Fatalf("allowed restart %d failed: %+v", i+1, snapshot)
			}
			continue
		}
		if snapshot.Initialized || !strings.Contains(snapshot.Error, "native restart budget exhausted (3/3)") {
			t.Fatalf("native exceeded persistent restart budget: %+v", snapshot)
		}
		if i == 3 {
			stopRun()
			<-runDone
			stopRun = nil
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			service, err = executor.NewService(config)
			if err != nil {
				t.Fatal(err)
			}
			startRun()
		}
	}
}

func TestServiceContinuesAnUnloadedThreadOnlyWhenItsGoalIsProvenInactive(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active-goal-%v", active), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
			input := "complete with unloaded thread"
			if active {
				input = "complete with unloaded active goal"
			}
			op := protocol.Operation{ID: "unloaded-create", TaskID: "unloaded-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: input}
			if r := service.Execute(ctx, op); r.Stage != "accepted" {
				t.Fatalf("create: %+v", r)
			}
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				snapshot, err := service.Snapshot(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.Tasks[0].Execution == "completed" {
					break
				}
				time.Sleep(time.Millisecond)
			}
			op.ID, op.Action, op.Input = "unloaded-continue", "task.continue", "continue this explicit requirement"
			r := service.Execute(ctx, op)
			if !active {
				if r.Stage != "accepted" || r.ThreadID != "thread-1" || r.TurnID != "turn-2" {
					t.Fatalf("authorized unloaded continuation: %+v", r)
				}
				return
			}
			if r.Stage != "failed" || r.Error == nil || r.Error.Code != "conflict" {
				t.Fatalf("active Goal received a competing continuation: %+v", r)
			}
			op.ID, op.Action = "unloaded-read", "task.read"
			r = service.Execute(ctx, op)
			var read struct {
				Native struct {
					Thread struct {
						Status struct {
							Type string `json:"type"`
						} `json:"status"`
					} `json:"thread"`
				} `json:"native"`
			}
			if r.Stage != "accepted" || json.Unmarshal(r.Data, &read) != nil || read.Native.Thread.Status.Type != "notLoaded" {
				t.Fatalf("active Goal was loaded despite refusal: %+v data=%s", r, r.Data)
			}
		})
	}
}

func TestServiceStagesNewTaskInputsBeforeNativeExecutionAndPassesOnlyFileMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := serviceConfig(t)
	config.Native.Command = append(config.Native.Command, "expect-inputs")
	service, err := executor.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.StartNative(ctx); err != nil {
		t.Fatal(err)
	}
	emptyHash := sha256.Sum256(nil)
	binaryHash := sha256.Sum256([]byte{0, 255, 128, 1})
	op := protocol.Operation{ID: "create-with-inputs", TaskID: "input-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "inspect staged input files", InputFiles: []protocol.FileUpload{
		{Name: "empty.bin", DataBase64: "", Size: 0, SHA256: hex.EncodeToString(emptyHash[:])},
		{Name: "binary.bin", DataBase64: "AP+AAQ==", Size: 4, SHA256: hex.EncodeToString(binaryHash[:])},
	}}
	first := service.Execute(ctx, op)
	if first.Stage != "accepted" || first.ThreadID != "thread-1" || first.TurnID != "turn-1" {
		t.Fatalf("native did not receive staged inputs and metadata: %+v", first)
	}
	if replay := service.Execute(ctx, op); replay.Stage != "accepted" || string(replay.Data) != string(first.Data) || replay.TurnID != first.TurnID {
		t.Fatalf("input task replay changed its files or turn: %+v", replay)
	}
	snapshot, err := service.Snapshot(ctx)
	if err != nil || len(snapshot.Tasks) != 1 || snapshot.Tasks[0].Objective != "inspect staged input files" {
		t.Fatalf("attachment metadata changed original objective: %+v err=%v", snapshot, err)
	}
}

func TestServiceRejectsInvalidNewTaskInputsBeforeWritingOrStartingNativeWork(t *testing.T) {
	makeFile := func(name string, data []byte) protocol.FileUpload {
		sum := sha256.Sum256(data)
		return protocol.FileUpload{Name: name, DataBase64: base64.StdEncoding.EncodeToString(data), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	}
	valid := makeFile("first.bin", []byte{1, 2, 3})
	badHash, badSize, badPath := valid, valid, valid
	badHash.SHA256, badSize.Size, badPath.Name = "bad", 4, "../escape.bin"
	largeA, largeB := makeFile("large-a.bin", make([]byte, 4<<20)), makeFile("large-b.bin", make([]byte, (4<<20)+1))
	for _, test := range []struct {
		name  string
		files []protocol.FileUpload
		code  string
	}{
		{"hash", []protocol.FileUpload{valid, badHash}, "integrity_mismatch"},
		{"size", []protocol.FileUpload{valid, badSize}, "integrity_mismatch"},
		{"path", []protocol.FileUpload{valid, badPath}, "invalid_upload"},
		{"duplicate", []protocol.FileUpload{valid, valid}, "invalid_upload"},
		{"combined limit", []protocol.FileUpload{largeA, largeB}, "file_too_large"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
			op := protocol.Operation{ID: "bad-input-create", TaskID: "input-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "inspect input", InputFiles: test.files}
			if r := service.Execute(ctx, op); r.Stage != "failed" || r.Error == nil || r.Error.Code != test.code {
				t.Fatalf("invalid inputs reached native: %+v", r)
			}
			snapshot, err := service.Snapshot(ctx)
			if err != nil || len(snapshot.Tasks) != 0 {
				t.Fatalf("invalid inputs created partial task: %+v err=%v", snapshot, err)
			}
			read := service.Execute(ctx, protocol.Operation{ID: "inspect-invalid-files", Scope: config.Scope, ProjectID: "test", Action: "files.list", Path: ".dots-duet"})
			if read.Stage != "failed" || read.Error == nil || read.Error.Code != "not_found" {
				t.Fatalf("invalid batch wrote early attachments: %+v", read)
			}
			op.ID, op.InputFiles = "good-after-invalid", []protocol.FileUpload{valid}
			if r := service.Execute(ctx, op); r.Stage != "accepted" || r.ThreadID != "thread-1" || r.TurnID != "turn-1" {
				t.Fatalf("invalid batch already started a native thread: %+v", r)
			}
		})
	}
}

func TestServiceRetainsStagedFilesAndReportsARealInputWriteFailureWithoutStartingNativeWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config := serviceConfig(t)
	dir := filepath.Join(config.Projects[0].Root, ".dots-duet", "attachments", "input-task")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second.bin"), []byte("existing"), 0600); err != nil {
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
	sum := sha256.Sum256([]byte("new"))
	op := protocol.Operation{ID: "partial-input-create", TaskID: "input-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "inspect input", InputFiles: []protocol.FileUpload{
		{Name: "first.bin", DataBase64: "bmV3", SHA256: hex.EncodeToString(sum[:]), Size: 3},
		{Name: "second.bin", DataBase64: "bmV3", SHA256: hex.EncodeToString(sum[:]), Size: 3},
	}}
	r := service.Execute(ctx, op)
	var partial struct {
		protocol.Task
		InputFiles []struct {
			Path string `json:"path"`
		} `json:"input_files"`
	}
	if r.Stage != "failed" || r.Error == nil || r.Error.Code != "file_exists" || json.Unmarshal(r.Data, &partial) != nil || partial.ThreadID != "" || partial.Execution != "failed" || len(partial.InputFiles) != 1 || partial.InputFiles[0].Path != ".dots-duet/attachments/input-task/first.bin" {
		t.Fatalf("partial input failure was hidden: %+v data=%s", r, r.Data)
	}
	for i, test := range []struct{ name, content string }{{"first.bin", "new"}, {"second.bin", "existing"}} {
		read := service.Execute(ctx, protocol.Operation{ID: fmt.Sprintf("partial-read-%d", i), TaskID: op.TaskID, Scope: op.Scope, ProjectID: op.ProjectID, Action: "files.read", Path: ".dots-duet/attachments/input-task/" + test.name})
		var file struct {
			Text string `json:"text"`
		}
		if read.Stage != "accepted" || json.Unmarshal(read.Data, &file) != nil || file.Text != test.content {
			t.Fatalf("staged or existing bytes were removed/replaced: %+v data=%s", read, read.Data)
		}
	}
	if replay := service.Execute(ctx, op); replay.Stage != "failed" || string(replay.Data) != string(r.Data) {
		t.Fatalf("partial failure was replayed: %+v", replay)
	}
	op.ID, op.TaskID, op.InputFiles = "after-partial-inputs", "other-task", nil
	if next := service.Execute(ctx, op); next.Stage != "accepted" || next.ThreadID != "thread-1" || next.TurnID != "turn-1" {
		t.Fatalf("failed input staging already ran native work: %+v", next)
	}
}

func TestServiceAcceptsNewTaskInputFilesAtTheCombinedEightMiBLimit(t *testing.T) {
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
	data := make([]byte, 4<<20)
	sum := sha256.Sum256(data)
	first := protocol.FileUpload{Name: "limit-first.bin", DataBase64: base64.StdEncoding.EncodeToString(data), SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	second := first
	second.Name = "limit-second.bin"
	op := protocol.Operation{ID: "limit-input-create", TaskID: "input-task", Scope: config.Scope, ProjectID: "test", TaskVersion: 1, Action: "task.create", Input: "inspect input", InputFiles: []protocol.FileUpload{first, second}}
	if r := service.Execute(ctx, op); r.Stage != "accepted" {
		t.Fatalf("valid combined 8 MiB inputs rejected: %+v", r)
	}
	for i, file := range op.InputFiles {
		r := service.Execute(ctx, protocol.Operation{ID: fmt.Sprintf("limit-input-read-%d", i), TaskID: op.TaskID, Scope: op.Scope, ProjectID: op.ProjectID, Action: "files.download", Path: ".dots-duet/attachments/input-task/" + file.Name})
		var stored struct {
			Hash string `json:"sha256"`
			Size int64  `json:"size"`
		}
		if r.Stage != "accepted" || json.Unmarshal(r.Data, &stored) != nil || stored.Hash != file.SHA256 || stored.Size != file.Size {
			t.Fatalf("valid limit attachment not available: stage=%s error=%v metadata=%+v", r.Stage, r.Error, stored)
		}
	}
}

func TestServicePeer(t *testing.T) {
	if len(os.Args) < 2 || os.Args[1] != "-test.run=^TestServicePeer$" {
		return
	}
	dec, enc := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	threads := 0
	turns := 0
	interrupted := false
	goalStatus, forcedStatus := "", ""
	if len(os.Args) == 4 && os.Args[2] == "expect-resume-cwd" {
		interrupted = true
	}
	projectRoot := ""
	var control net.Conn
	var pendingTurnID json.RawMessage
	var pendingGoalID json.RawMessage
	var pendingGoal map[string]any
	var goalReleased chan struct{}
	if len(os.Args) == 4 && (os.Args[2] == "pause-create" || os.Args[2] == "pause-turn" || os.Args[2] == "pause-goal") {
		var err error
		control, err = net.DialTimeout("tcp", os.Args[3], time.Second)
		if err != nil {
			os.Exit(4)
		}
		defer control.Close()
		_ = control.SetDeadline(time.Now().Add(5 * time.Second))
		if os.Args[2] == "pause-goal" {
			goalReleased = make(chan struct{})
			release := goalReleased
			go func() {
				var signal [1]byte
				if _, err := io.ReadFull(control, signal[:]); err != nil {
					return
				}
				close(release)
			}()
		}
	}
	type peerFrame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	frames := make(chan peerFrame)
	readError := make(chan error, 1)
	go func() {
		for {
			var frame peerFrame
			if err := dec.Decode(&frame); err != nil {
				readError <- err
				return
			}
			frames <- frame
		}
	}()
	for {
		var frame peerFrame
		select {
		case <-goalReleased:
			goalReleased = nil
			goalStatus = "active"
			if err := enc.Encode(map[string]any{"id": pendingGoalID, "result": map[string]any{"goal": pendingGoal}}); err != nil {
				os.Exit(3)
			}
			pendingGoalID = nil
			continue
		case err := <-readError:
			if err == io.EOF {
				os.Exit(0)
			}
			os.Exit(2)
		case frame = <-frames:
		}
		if len(frame.ID) == 0 {
			continue
		}
		if frame.Method == "" && string(frame.ID) == `"native-approval"` {
			if err := enc.Encode(map[string]any{"method": "serverRequest/resolved", "params": map[string]string{"threadId": "thread-1", "requestId": "native-approval"}}); err != nil {
				os.Exit(3)
			}
			continue
		}
		var result any
		switch frame.Method {
		case "initialize":
			result = map[string]string{"userAgent": "service-peer/1"}
		case "thread/start":
			if len(os.Args) == 4 && os.Args[2] == "expect-resume-cwd" {
				_ = enc.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32602, "message": "continuation must resume the existing thread"}})
				continue
			}
			var params struct {
				Model string `json:"model"`
				Cwd   string `json:"cwd"`
			}
			_ = json.Unmarshal(frame.Params, &params)
			projectRoot = params.Cwd
			if len(os.Args) == 4 && os.Args[2] == "expect-cwd" && filepath.Clean(params.Cwd) != filepath.Clean(os.Args[3]) {
				_ = enc.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32602, "message": "native cwd did not match the selected task directory"}})
				continue
			}
			if len(os.Args) == 3 && os.Args[2] == "expect-inputs" {
				empty, emptyErr := os.ReadFile(filepath.Join(projectRoot, ".dots-duet", "attachments", "input-task", "empty.bin"))
				binary, binaryErr := os.ReadFile(filepath.Join(projectRoot, ".dots-duet", "attachments", "input-task", "binary.bin"))
				if emptyErr != nil || binaryErr != nil || len(empty) != 0 || string(binary) != string([]byte{0, 255, 128, 1}) {
					_ = enc.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32602, "message": "input attachments were not staged before thread/start"}})
					continue
				}
			}
			if params.Model == "reject-model" {
				_ = enc.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32602, "message": "model rejected"}})
				continue
			}
			threads++
			if control != nil && os.Args[2] == "pause-create" {
				if _, err := control.Write([]byte("t")); err != nil {
					os.Exit(5)
				}
				var signal [1]byte
				if _, err := io.ReadFull(control, signal[:]); err != nil {
					os.Exit(5)
				}
			}
			result = map[string]any{"thread": map[string]string{"id": fmt.Sprintf("thread-%d", threads)}}
		case "turn/start":
			var params struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			_ = json.Unmarshal(frame.Params, &params)
			if len(os.Args) == 3 && os.Args[2] == "expect-inputs" {
				var files []struct {
					Path string `json:"path"`
					Hash string `json:"sha256"`
					Size int64  `json:"size"`
				}
				var text string
				if len(params.Input) > 0 {
					text = params.Input[0].Text
				}
				_, manifest, found := strings.Cut(text, "\n\nInput files (paths are relative to the project workspace):\n")
				valid := found && !strings.Contains(text, "data_base64") && !strings.Contains(text, "AP+AAQ==") && json.Unmarshal([]byte(manifest), &files) == nil && len(files) == 2
				for _, file := range files {
					if !filepath.IsLocal(filepath.FromSlash(file.Path)) {
						valid = false
					}
					data, err := os.ReadFile(filepath.Join(projectRoot, filepath.FromSlash(file.Path)))
					sum := sha256.Sum256(data)
					if err != nil || int64(len(data)) != file.Size || hex.EncodeToString(sum[:]) != file.Hash {
						valid = false
					}
				}
				if !valid {
					_ = enc.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32602, "message": "initial input must contain only verified relative attachment metadata"}})
					continue
				}
			}
			if len(params.Input) > 0 && params.Input[0].Text == "exit without receipt" {
				os.Exit(7)
			}
			if len(params.Input) > 0 && params.Input[0].Text == "reject turn" {
				_ = enc.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32602, "message": "input rejected"}})
				continue
			}
			interrupted = false
			turns++
			if control != nil && os.Args[2] == "pause-turn" {
				pendingTurnID = frame.ID
				if _, err := control.Write([]byte("t")); err != nil {
					os.Exit(5)
				}
				continue
			}
			result = map[string]any{"turn": map[string]string{"id": fmt.Sprintf("turn-%d", turns), "status": "inProgress"}}
		case "thread/resume":
			if len(os.Args) == 4 && (os.Args[2] == "expect-cwd" || os.Args[2] == "expect-resume-cwd") {
				var params struct {
					Cwd string `json:"cwd"`
				}
				if json.Unmarshal(frame.Params, &params) != nil || filepath.Clean(params.Cwd) != filepath.Clean(os.Args[3]) {
					_ = enc.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32602, "message": "native resume cwd did not match the original task directory"}})
					continue
				}
			}
			forcedStatus, interrupted = "", true
			result = map[string]any{"thread": map[string]string{"id": "thread-1"}}
		case "turn/steer":
			result = map[string]string{"turnId": fmt.Sprintf("turn-%d", turns)}
		case "thread/goal/get":
			result = map[string]any{"goal": nil}
			if goalStatus == "missing" {
				result = map[string]any{}
			} else if goalStatus != "" {
				result = map[string]any{"goal": map[string]string{"status": goalStatus}}
			}
		case "thread/goal/set":
			var params map[string]any
			if err := json.Unmarshal(frame.Params, &params); err != nil {
				os.Exit(6)
			}
			if params["status"] == "active" && control != nil && os.Args[2] == "pause-goal" {
				pendingGoalID, pendingGoal = frame.ID, params
				if _, err := control.Write([]byte("g")); err != nil {
					os.Exit(5)
				}
				continue
			}
			if status, ok := params["status"].(string); ok {
				goalStatus = status
			}
			result = map[string]any{"goal": params}
		case "thread/read":
			state := "active"
			if interrupted {
				state = "idle"
			}
			if forcedStatus != "" {
				state = forcedStatus
			}
			result = map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]string{"type": state}}}
		case "turn/interrupt":
			if interrupted {
				_ = enc.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32600, "message": "no active turn to interrupt"}})
				continue
			}
			interrupted = true
			if err := enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]string{"id": "turn-1", "status": "interrupted"}}}); err != nil {
				os.Exit(3)
			}
			result = map[string]any{}
		default:
			_ = enc.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32601, "message": "unexpected method"}})
			continue
		}
		if err := enc.Encode(map[string]any{"id": frame.ID, "result": result}); err != nil {
			os.Exit(3)
		}
		if frame.Method == "thread/read" && len(pendingTurnID) > 0 {
			if err := enc.Encode(map[string]any{"id": pendingTurnID, "result": map[string]any{"turn": map[string]string{"id": "turn-1", "status": "inProgress"}}}); err != nil {
				os.Exit(3)
			}
			pendingTurnID = nil
		}
		if frame.Method == "turn/start" {
			var params struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			_ = json.Unmarshal(frame.Params, &params)
			if len(params.Input) > 0 && params.Input[0].Text == "exit process" {
				os.Exit(7)
			}
			if len(params.Input) > 0 && params.Input[0].Text == "request approval" {
				if err := enc.Encode(map[string]any{"id": "native-approval", "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "command": "write report", "availableDecisions": []string{"accept", "decline"}}}); err != nil {
					os.Exit(3)
				}
			}
			if len(params.Input) > 0 && (params.Input[0].Text == "complete immediately" || params.Input[0].Text == "complete with active goal" || params.Input[0].Text == "complete with unknown goal" || params.Input[0].Text == "complete with missing goal" || params.Input[0].Text == "complete with unloaded thread" || params.Input[0].Text == "complete with unloaded active goal") {
				interrupted = true
				switch params.Input[0].Text {
				case "complete with active goal":
					goalStatus = "active"
				case "complete with unknown goal":
					goalStatus = "futureStatus"
				case "complete with missing goal":
					goalStatus = "missing"
				case "complete with unloaded thread":
					forcedStatus = "notLoaded"
				case "complete with unloaded active goal":
					forcedStatus, goalStatus = "notLoaded", "active"
				}
				if err := enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]string{"id": fmt.Sprintf("turn-%d", turns), "status": "completed"}}}); err != nil {
					os.Exit(3)
				}
			}
		}
	}
}
