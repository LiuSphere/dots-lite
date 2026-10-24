package hub_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"dots-duet-lite/internal/protocol"
	"github.com/coder/websocket/wsjson"
)

func privateCreateInvocation() protocol.Invocation {
	return protocol.Invocation{Tool: "duet_prepare", Arguments: json.RawMessage(`{"action":"task.create","project_id":"demo","input":"read supplied inputs","inputs":[{"name":"input.txt","local_path":"/allowed/input"},{"name":"empty.txt","local_path":"/allowed/empty"}]}`), Source: protocol.Source{Kind: "gateway", Platform: "test", SenderID: "sender-a", ChatID: "chat-a", SessionID: "session-a"}, AttachmentFiles: []protocol.InvocationAttachment{
		{LocalPath: "/allowed/input", File: protocol.FileUpload{Name: "input.txt", DataBase64: "YWJj", SHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", Size: 3}},
		{LocalPath: "/allowed/empty", File: protocol.FileUpload{Name: "empty.txt", DataBase64: "", SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", Size: 0}},
	}}
}

func TestPrivateInputsAreInvocationBoundAndPublicOperationsOmitBytes(t *testing.T) {
	ctx, h, conn, _ := connectedHubFixture(t, 2)
	inv := privateCreateInvocation()
	originalArgs := append(json.RawMessage(nil), inv.Arguments...)
	ref, err := h.RegisterInvocation(ctx, 1001, inv)
	if err != nil {
		t.Fatal(err)
	}
	inv.AttachmentFiles[0].File.DataBase64 = "YWJk"
	inv.AttachmentFiles[0].File.Name = "mutated.txt"
	if _, err = h.Call(ctx, 1002, protocol.ToolRequest{Tool: inv.Tool, Arguments: originalArgs, ContextRef: ref}); err == nil {
		t.Fatal("another caller used private input reference")
	}
	changed := json.RawMessage(strings.Replace(string(originalArgs), "/allowed/input", "/changed/input", 1))
	if _, err = h.Call(ctx, 1001, protocol.ToolRequest{Tool: inv.Tool, Arguments: changed, ContextRef: ref}); err == nil {
		t.Fatal("changed local input path reused invocation bytes")
	}
	prepared, err := h.Call(ctx, 1001, protocol.ToolRequest{Tool: inv.Tool, Arguments: originalArgs, ContextRef: ref})
	if err != nil {
		t.Fatal(err)
	}
	var ids struct {
		TaskID      string `json:"task_id"`
		OperationID string `json:"operation_id"`
	}
	if err = json.Unmarshal(prepared, &ids); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{`{"query":"operation","operation_id":"` + ids.OperationID + `"}`, `{"query":"operations","task_id":"` + ids.TaskID + `"}`} {
		public := call(t, h, 1001, "duet_query", args)
		if strings.Contains(string(public), "data_base64") || !strings.Contains(string(public), `"name":"input.txt"`) || !strings.Contains(string(public), `"name":"empty.txt"`) {
			t.Fatalf("public input metadata leaked or lost data: %s", public)
		}
	}
	done := make(chan error, 1)
	go func() {
		var frame protocol.Frame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			done <- err
			return
		}
		files := frame.Operation.InputFiles
		if frame.Operation.Action != "task.create" || len(files) != 2 || files[0].Name != "input.txt" || files[0].DataBase64 != "YWJj" || files[1].Name != "empty.txt" || files[1].Size != 0 || files[1].DataBase64 != "" {
			done <- fmt.Errorf("executor did not receive immutable complete input bytes: %+v", frame.Operation)
			return
		}
		data, _ := json.Marshal(protocol.Task{ID: ids.TaskID, Scope: frame.Operation.Scope, Version: 1, Execution: "running"})
		done <- wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: frame.ID, Result: &protocol.Result{OperationID: frame.Operation.ID, TaskID: ids.TaskID, Stage: "accepted", Data: data}})
	}()
	result := call(t, h, 1001, "duet_submit", `{"operation_id":"`+ids.OperationID+`"}`)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result), "data_base64") {
		t.Fatalf("submit response leaked private input bytes: %s", result)
	}
}

func TestPrivateAttachmentRegistrationRejectsMissingTamperedOrWrongSourceData(t *testing.T) {
	h, _, _ := deliveryFixture(t)
	for _, name := range []string{"missing", "path", "name", "hash", "size", "extra", "other_tool", "api", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			inv := privateCreateInvocation()
			switch name {
			case "missing":
				inv.AttachmentFiles = nil
			case "path":
				inv.AttachmentFiles[0].LocalPath = "/different/path"
			case "name":
				inv.AttachmentFiles[0].File.Name = "different.txt"
			case "hash":
				inv.AttachmentFiles[0].File.DataBase64 = "YWJk"
			case "size":
				inv.AttachmentFiles[0].File.Size = 4
			case "extra":
				inv.AttachmentFiles = append(inv.AttachmentFiles, protocol.InvocationAttachment{LocalPath: "/extra", File: inv.AttachmentFiles[1].File})
			case "other_tool":
				inv.Tool = "duet_query"
			case "api":
				inv.Source = protocol.Source{Kind: "api", Platform: "api_server", ChatID: "unregistered-api", SessionID: "native", NativeTaskID: "run", ToolCallID: "call"}
			case "duplicate":
				inv.Arguments = json.RawMessage(strings.Replace(string(inv.Arguments), `"name":"empty.txt"`, `"name":"input.txt"`, 1))
				inv.AttachmentFiles[1].File.Name = "input.txt"
			}
			if _, err := h.RegisterInvocation(context.Background(), 1001, inv); err == nil {
				t.Fatalf("invalid private input registration accepted: %s", name)
			}
		})
	}
}

func TestPrivateUploadReferenceAndLegacyRawUploadKeepBytesOffPublicResults(t *testing.T) {
	for _, mode := range []string{"reference", "raw"} {
		t.Run(mode, func(t *testing.T) {
			ctx, h, conn, taskID := connectedHubFixture(t, 2)
			base := privateCreateInvocation()
			file := base.AttachmentFiles[0]
			upload := `{"name":"input.txt","local_path":"/allowed/input"}`
			if mode == "raw" {
				raw, _ := json.Marshal(file.File)
				upload = string(raw)
			}
			inv := protocol.Invocation{Tool: "duet_prepare", Arguments: json.RawMessage(`{"action":"files.upload","task_id":"` + taskID + `","upload":` + upload + `}`), Source: base.Source}
			if mode == "reference" {
				inv.AttachmentFiles = []protocol.InvocationAttachment{file}
			}
			ref, err := h.RegisterInvocation(ctx, 1001, inv)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := h.Call(ctx, 1001, protocol.ToolRequest{Tool: inv.Tool, Arguments: inv.Arguments, ContextRef: ref})
			if err != nil {
				t.Fatal(err)
			}
			var ids struct {
				ID string `json:"operation_id"`
			}
			if err = json.Unmarshal(prepared, &ids); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				var frame protocol.Frame
				if err := wsjson.Read(ctx, conn, &frame); err != nil {
					done <- err
					return
				}
				if frame.Operation.Upload == nil || *frame.Operation.Upload != file.File {
					done <- fmt.Errorf("upload bytes changed between invocation and executor: %+v", frame.Operation)
					return
				}
				done <- wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "result", ID: frame.ID, Result: &protocol.Result{OperationID: frame.Operation.ID, TaskID: taskID, Stage: "accepted", Data: json.RawMessage(`{"path":".dots-duet/attachments/input.txt","size":3}`)}})
			}()
			result := call(t, h, 1001, "duet_submit", `{"operation_id":"`+ids.ID+`"}`)
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			for _, raw := range []json.RawMessage{prepared, result, call(t, h, 1001, "duet_query", `{"query":"operation","operation_id":"`+ids.ID+`"}`), call(t, h, 1001, "duet_query", `{"query":"operations","task_id":"`+taskID+`"}`)} {
				if strings.Contains(string(raw), "data_base64") || strings.Contains(string(raw), "YWJj") {
					t.Fatalf("public upload result leaked private bytes: %s", raw)
				}
			}
		})
	}
}

func TestPreboundAPICannotRegisterPrivateInputBytes(t *testing.T) {
	ctx, h, _, taskID := followupFixture(t, 2)
	raw, err := h.NextFollowup(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	f := readFollowup(t, raw)
	base := privateCreateInvocation()
	base.Arguments = json.RawMessage(`{"action":"files.upload","task_id":"` + taskID + `","upload":{"name":"input.txt","local_path":"/allowed/input"}}`)
	base.AttachmentFiles = base.AttachmentFiles[:1]
	base.Source = protocol.Source{Kind: "api", Platform: "api_server", ChatID: f.SessionID, SessionID: "physical", NativeTaskID: "native-run", ToolCallID: "private-input"}
	if _, err = h.RegisterInvocation(ctx, 1001, base); err == nil {
		t.Fatal("prebound API session admitted gateway-only private bytes")
	}
}

func TestCombinedPrivateInputsRespectEightMiBBoundary(t *testing.T) {
	h, _, _ := deliveryFixture(t)
	for _, size := range []int{4 << 20, 5 << 20} {
		inv := privateCreateInvocation()
		content := bytes.Repeat([]byte("x"), size)
		sum := sha256.Sum256(content)
		for i := range inv.AttachmentFiles {
			inv.AttachmentFiles[i].File.DataBase64 = base64.StdEncoding.EncodeToString(content)
			inv.AttachmentFiles[i].File.Size = int64(size)
			inv.AttachmentFiles[i].File.SHA256 = hex.EncodeToString(sum[:])
		}
		_, err := h.RegisterInvocation(context.Background(), 1001, inv)
		if size == 4<<20 && err != nil {
			t.Fatalf("valid combined 8MiB rejected: %v", err)
		}
		if size == 5<<20 && err == nil {
			t.Fatal("combined 10MiB accepted")
		}
	}
}
