package hub_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"dots-lite/internal/hub"
	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestLostWorkerReceiptBecomesUnknownWithoutReplayingSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h, _, taskID := deliveryFixture(t)
		call(t, h, 1001, "dots_lite_record", `{"task_id":"`+taskID+`","kind":"verification","status":"passed","evidence":{"checks":[{"observed":"report exists"}]}}`)
		raw, err := h.NextDelivery(ctx, 1001)
		if err != nil {
			t.Fatal(err)
		}
		var job struct {
			ID      string `json:"id"`
			Attempt string `json:"attempt_id"`
		}
		if err = json.Unmarshal(raw, &job); err != nil || job.ID == "" {
			t.Fatalf("no send attempt: %s %v", raw, err)
		}
		time.Sleep(149 * time.Second)
		if raw, err = h.NextDelivery(ctx, 1001); err != nil || string(raw) != "null" {
			t.Fatalf("in-flight send replayed: %s %v", raw, err)
		}
		time.Sleep(2 * time.Second)
		if raw, err = h.NextDelivery(ctx, 1001); err != nil || string(raw) != "null" {
			t.Fatalf("expired send replayed: %s %v", raw, err)
		}
		var items []struct {
			Status string `json:"status"`
		}
		if err = json.Unmarshal(call(t, h, 1001, "dots_lite_query", `{"query":"deliveries","task_id":"`+taskID+`"}`), &items); err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Status != "unknown" {
			t.Fatalf("missing worker receipt stayed in flight: %+v", items)
		}
		var task protocol.Task
		if err = json.Unmarshal(call(t, h, 1001, "dots_lite_query", `{"query":"task","task_id":"`+taskID+`"}`), &task); err != nil {
			t.Fatal(err)
		}
		if task.Delivery != "unknown" {
			t.Fatalf("task hid uncertain delivery: %+v", task)
		}
		if _, err = h.ReportDelivery(ctx, 1001, job.ID, json.RawMessage(`{"attempt_id":"`+job.Attempt+`","status":"delivered","receipt":{"success":true,"message_id":"late-native-receipt"}}`)); err != nil {
			t.Fatal(err)
		}
	})
}

func deliveryFixture(t *testing.T) (*hub.Hub, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	var ids struct {
		TaskID string `json:"task_id"`
	}
	if err = json.Unmarshal(call(t, h, 1001, "dots_lite_prepare", `{"action":"task.create","folder_id":"demo","input":"make verified report","name":"report"}`), &ids); err != nil {
		t.Fatal(err)
	}
	return h, path, ids.TaskID
}

func TestFinalDeliveryUsesOriginalTargetAndRequiresNativeReceipt(t *testing.T) {
	ctx := context.Background()
	h, _, taskID := deliveryFixture(t)
	args := `{"task_id":"` + taskID + `","kind":"verification","status":"passed","evidence":{"checks":[{"command":"check report","exit_code":0,"observed":"report content matches"}]}}`
	call(t, h, 1001, "dots_lite_record", args)
	call(t, h, 1001, "dots_lite_record", args)
	if _, err := h.Call(ctx, 1001, registered(t, h, 1001, "dots_lite_record", `{"task_id":"`+taskID+`","kind":"delivery","status":"delivered","evidence":{"receipt":{"success":true,"message_id":"model-invented"}}}`)); err == nil {
		t.Fatal("model self-report bypassed native delivery outbox")
	}
	other, err := h.NextDelivery(ctx, 1002)
	if err != nil || string(other) != "null" {
		t.Fatalf("delivery leaked to another UID: %s %v", other, err)
	}
	raw, err := h.NextDelivery(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		ID        string `json:"id"`
		AttemptID string `json:"attempt_id"`
		Kind      string `json:"kind"`
		Target    struct {
			Platform string `json:"platform"`
			ChatID   string `json:"chat_id"`
		} `json:"target"`
	}
	if err = json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || job.AttemptID == "" || job.Kind != "text" || job.Target.Platform != "test" || job.Target.ChatID != "chat-a" {
		t.Fatalf("verified result did not produce fixed-target delivery: %s", raw)
	}
	again, err := h.NextDelivery(ctx, 1001)
	if err != nil || string(again) != "null" {
		t.Fatalf("dispatch intent was replayed: %s %v", again, err)
	}
	for _, receipt := range []string{`{}`, `{"success":true}`, `{"success":false,"message_id":"false-success"}`} {
		if _, err = h.ReportDelivery(ctx, 1001, job.ID, json.RawMessage(`{"attempt_id":"`+job.AttemptID+`","status":"delivered","receipt":`+receipt+`}`)); err == nil {
			t.Fatalf("invalid native receipt accepted: %s", receipt)
		}
	}
	valid := json.RawMessage(`{"attempt_id":"` + job.AttemptID + `","status":"delivered","receipt":{"success":true,"platform":"test","chat_id":"chat-a","message_id":"native-message-1"}}`)
	first, err := h.ReportDelivery(ctx, 1001, job.ID, valid)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.ReportDelivery(ctx, 1001, job.ID, valid)
	if err != nil || string(first) != string(second) {
		t.Fatalf("duplicate receipt changed result: %s %s %v", first, second, err)
	}
	var task protocol.Task
	if err = json.Unmarshal(call(t, h, 1001, "dots_lite_query", `{"query":"task","task_id":"`+taskID+`"}`), &task); err != nil {
		t.Fatal(err)
	}
	if task.Delivery != "delivered" {
		t.Fatalf("native result was not reflected in delivery state: %+v", task)
	}
}

func TestAcquiredArtifactNeedsSeparateVerifiedNativeMediaReceipt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := testConfig()
	cfg.Bindings[0].Sources[0].Platform = "weixin"
	h, err := hub.New(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	invoke := func(tool, args string) json.RawMessage {
		t.Helper()
		raw := json.RawMessage(args)
		ref, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: tool, Arguments: raw, Source: protocol.Source{Kind: "gateway", Platform: "weixin", SenderID: "sender-a", ChatID: "chat-a", SessionID: "original"}})
		if err != nil {
			t.Fatal(err)
		}
		out, err := h.Call(ctx, 1001, protocol.ToolRequest{Tool: tool, Arguments: raw, ContextRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	var ids struct {
		TaskID string `json:"task_id"`
	}
	if err = json.Unmarshal(invoke("dots_lite_prepare", `{"action":"task.create","folder_id":"demo","input":"deliver report","artifacts":["out.txt"]}`), &ids); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.WebsocketHandler())
	defer srv.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/executor", &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer token-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "hello", Snapshot: &protocol.Snapshot{InstanceID: "media-instance", Initialized: true}}); err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	file := json.RawMessage(`{"path":"out.txt","task_version":1,"data_base64":"YWJj","sha256":"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad","size":3,"returned_bytes":3,"offset":0,"truncated":false}`)
	if err = wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &protocol.Event{Sequence: 1, TaskID: ids.TaskID, InstanceID: "media-instance", Type: "artifact/available", Data: file, CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err = wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	raw, err := h.NextArtifact(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	var acquired struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(raw, &acquired); err != nil {
		t.Fatal(err)
	}
	if _, err = h.RecordArtifactAcquisition(ctx, 1001, acquired.ID, json.RawMessage(`{"acquired":true,"local_path":"/private/out.txt","sha256":"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad","size":3,"returned_bytes":3,"offset":0}`)); err != nil {
		t.Fatal(err)
	}
	if pending, err := h.NextDelivery(ctx, 1001); err != nil || string(pending) != "null" {
		t.Fatalf("unverified acquisition created outbound message: %s %v", pending, err)
	}
	invoke("dots_lite_record", `{"task_id":"`+ids.TaskID+`","kind":"verification","status":"passed","evidence":{"checks":[{"observed":"file hash matches expected report"}]}}`)
	for i := 0; i < 2; i++ {
		jobRaw, err := h.NextDelivery(ctx, 1001)
		if err != nil {
			t.Fatal(err)
		}
		var job struct {
			ID       string `json:"id"`
			Attempt  string `json:"attempt_id"`
			Kind     string `json:"kind"`
			Artifact struct {
				ID   string `json:"id"`
				SHA  string `json:"sha256"`
				Size int64  `json:"size"`
			} `json:"artifact"`
		}
		if err = json.Unmarshal(jobRaw, &job); err != nil {
			t.Fatal(err)
		}
		if job.ID == "" {
			t.Fatalf("missing separate final text or artifact unit: %s", jobRaw)
		}
		verification := ""
		if job.Kind == "artifact" {
			verification = `,"verification":{"media_only":true,"target_exact":true,"artifact_id":"` + job.Artifact.ID + `","sha256":"` + job.Artifact.SHA + `","size":3}`
			for _, bad := range []string{"", strings.Replace(verification, `"size":3`, `"size":4`, 1)} {
				if _, err = h.ReportDelivery(ctx, 1001, job.ID, json.RawMessage(`{"attempt_id":"`+job.Attempt+`","status":"delivered","receipt":{"success":true,"message_id":"wrong-proof"}`+bad+`}`)); err == nil {
					t.Fatal("file delivery accepted without matching media-only proof")
				}
			}
		}
		if _, err = h.ReportDelivery(ctx, 1001, job.ID, json.RawMessage(`{"attempt_id":"`+job.Attempt+`","status":"delivered","receipt":{"success":true,"message_id":"native-`+job.Kind+`","platform":"weixin","chat_id":"chat-a"}`+verification+`}`)); err != nil {
			t.Fatal(err)
		}
		var task protocol.Task
		if err = json.Unmarshal(invoke("dots_lite_query", `{"query":"task","task_id":"`+ids.TaskID+`"}`), &task); err != nil {
			t.Fatal(err)
		}
		if i == 0 && task.Delivery == "delivered" {
			t.Fatal("one send unit marked all result delivery complete")
		}
		if i == 1 && task.Delivery != "delivered" {
			t.Fatalf("native text and media did not complete delivery: %+v", task)
		}
	}
}

func TestUnacknowledgedDeliveryRemainsUnknownAcrossRestart(t *testing.T) {
	ctx := context.Background()
	h, path, taskID := deliveryFixture(t)
	call(t, h, 1001, "dots_lite_record", `{"task_id":"`+taskID+`","kind":"verification","status":"passed","evidence":{"checks":[{"observed":"report exists"}]}}`)
	if _, err := h.NextDelivery(ctx, 1001); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restarted, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	next, err := restarted.NextDelivery(ctx, 1001)
	if err != nil || string(next) != "null" {
		t.Fatalf("unknown send was automatically replayed: %s %v", next, err)
	}
	var items []struct {
		Status string `json:"status"`
	}
	if err = json.Unmarshal(call(t, restarted, 1001, "dots_lite_query", `{"query":"deliveries","task_id":"`+taskID+`"}`), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "unknown" {
		t.Fatalf("lost native receipt was not visible as unknown: %+v", items)
	}
}

func TestOnlyDefiniteNonSendCanRetryWithinPersistedBudget(t *testing.T) {
	ctx := context.Background()
	h, _, taskID := deliveryFixture(t)
	call(t, h, 1001, "dots_lite_record", `{"task_id":"`+taskID+`","kind":"verification","status":"passed","evidence":{"checks":[{"observed":"report exists"}]}}`)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		raw, err := h.NextDelivery(ctx, 1001)
		if err != nil {
			t.Fatal(err)
		}
		var job struct {
			ID      string `json:"id"`
			Attempt string `json:"attempt_id"`
		}
		if err = json.Unmarshal(raw, &job); err != nil {
			t.Fatal(err)
		}
		if job.ID == "" || seen[job.Attempt] {
			t.Fatalf("invalid bounded retry: %s", raw)
		}
		seen[job.Attempt] = true
		if _, err = h.ReportDelivery(ctx, 1001, job.ID, json.RawMessage(`{"attempt_id":"`+job.Attempt+`","status":"failed","definitely_not_sent":true,"error":"sender preflight rejected before network I/O"}`)); err != nil {
			t.Fatal(err)
		}
	}
	next, err := h.NextDelivery(ctx, 1001)
	if err != nil || string(next) != "null" {
		t.Fatalf("exhausted send was retried: %s %v", next, err)
	}
	var items []struct {
		Status   string            `json:"status"`
		Attempts int               `json:"attempts"`
		History  []json.RawMessage `json:"attempt_history"`
	}
	if err = json.Unmarshal(call(t, h, 1001, "dots_lite_query", `{"query":"deliveries","task_id":"`+taskID+`"}`), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != "blocked" || items[0].Attempts != 3 || len(items[0].History) != 3 {
		t.Fatalf("bounded sends lost attempt history: %+v", items)
	}
}

func TestAmbiguousAndPartialDeliveryCannotBeRetried(t *testing.T) {
	ctx := context.Background()
	h, _, taskID := deliveryFixture(t)
	call(t, h, 1001, "dots_lite_record", `{"task_id":"`+taskID+`","kind":"verification","status":"passed","evidence":{"checks":[{"observed":"report exists"}]}}`)
	raw, err := h.NextDelivery(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		ID      string `json:"id"`
		Attempt string `json:"attempt_id"`
	}
	if err = json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	if _, err = h.ReportDelivery(ctx, 1001, job.ID, json.RawMessage(`{"attempt_id":"`+job.Attempt+`","status":"failed","definitely_not_sent":true,"error":"partial transport response","receipt":{"success":true,"partial_success":true,"message_id":"partial-id"}}`)); err == nil {
		t.Fatal("partial native send was eligible for automatic replay")
	}
	if _, err = h.ReportDelivery(ctx, 1001, job.ID, json.RawMessage(`{"attempt_id":"`+job.Attempt+`","status":"unknown","error":"partial transport response","receipt":{"success":true,"partial_success":true,"message_id":"partial-id"}}`)); err != nil {
		t.Fatal(err)
	}
	next, err := h.NextDelivery(ctx, 1001)
	if err != nil || string(next) != "null" {
		t.Fatalf("ambiguous native send was replayed: %s %v", next, err)
	}
}

func TestApprovalNoticeDeliveryDoesNotApproveOrCompleteTask(t *testing.T) {
	ctx, h, conn, taskID := connectedHubFixture(t, 2)
	event := protocol.Event{Sequence: 1, TaskID: taskID, InstanceID: "retry-instance", Type: "item/commandExecution/requestApproval", Data: json.RawMessage(`{"request_id":"native-request-1","instance_id":"retry-instance","params":{"command":"needs human decision"}}`), CreatedAt: time.Now()}
	if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &event}); err != nil {
		t.Fatal(err)
	}
	var ack protocol.Frame
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	raw, err := h.NextDelivery(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	var job struct {
		ID      string `json:"id"`
		Attempt string `json:"attempt_id"`
		Purpose string `json:"purpose"`
	}
	if err = json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || job.Purpose != "attention" {
		t.Fatalf("approval did not create an original-chat notice: %s", raw)
	}
	if _, err = h.ReportDelivery(ctx, 1001, job.ID, json.RawMessage(`{"attempt_id":"`+job.Attempt+`","status":"delivered","receipt":{"success":true,"message_id":"notice-id"}}`)); err != nil {
		t.Fatal(err)
	}
	var task protocol.Task
	if err = json.Unmarshal(call(t, h, 1001, "dots_lite_query", `{"query":"task","task_id":"`+taskID+`"}`), &task); err != nil {
		t.Fatal(err)
	}
	if task.Waiting != "native-request-1" || task.Delivery == "delivered" {
		t.Fatalf("notice changed native approval or task delivery: %+v", task)
	}
}

func TestNativeQuestionsReachHermesBeforeAnyUserNotice(t *testing.T) {
	for _, method := range []string{"tool/requestUserInput", "mcpServer/elicitation/request"} {
		t.Run(method, func(t *testing.T) {
			ctx, h, conn, taskID := connectedHubFixture(t, 2)
			event := protocol.Event{Sequence: 1, TaskID: taskID, InstanceID: "retry-instance", Type: method,
				Data: json.RawMessage(`{"request_id":"question-1","instance_id":"retry-instance","params":{"question":"Which folder formatter is already configured?"}}`), CreatedAt: time.Now()}
			if err := wsjson.Write(ctx, conn, protocol.Frame{Version: protocol.Version, Type: "event", Event: &event}); err != nil {
				t.Fatal(err)
			}
			var ack protocol.Frame
			if err := wsjson.Read(ctx, conn, &ack); err != nil {
				t.Fatal(err)
			}
			notice, err := h.NextDelivery(ctx, 1001)
			if err != nil || string(notice) != "null" {
				t.Fatalf("ordinary question interrupted user before diagnosis: %s %v", notice, err)
			}
			raw, err := h.NextFollowup(ctx, 1001)
			if err != nil {
				t.Fatal(err)
			}
			f := readFollowup(t, raw)
			if f.ID == "" || f.TaskID != taskID {
				t.Fatalf("question was not offered to its Hermes task: %s", raw)
			}
			args := json.RawMessage(`{"task_id":"` + taskID + `","kind":"followup","status":"waiting_user","evidence":{"reason":"The folder has two valid formatters and needs the user's choice."}}`)
			ref, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "dots_lite_record", Arguments: args,
				Source: protocol.Source{Kind: "api", Platform: "api_server", ChatID: f.SessionID, SessionID: "question-session", NativeTaskID: "diagnose-question", ToolCallID: "needs-choice"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.Call(ctx, 1001, protocol.ToolRequest{Tool: "dots_lite_record", Arguments: args, ContextRef: ref}); err != nil {
				t.Fatal(err)
			}
			notice, err = h.NextDelivery(ctx, 1001)
			if err != nil || !strings.Contains(string(notice), "The folder has two valid formatters") {
				t.Fatalf("confirmed user decision did not notify original chat: %s %v", notice, err)
			}
		})
	}
}

func TestExhaustedRecoveryNoticeIncludesFactsAndAUserDecision(t *testing.T) {
	ctx, h, _, taskID := followupFixture(t, 1)
	raw, err := h.NextFollowup(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	f := readFollowup(t, raw)
	if _, err = h.SaveFollowupRun(ctx, 1001, f.ID, json.RawMessage(`{"run_id":"run-budget","session_id":"`+f.SessionID+`","status":"failed"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = h.ReportFollowup(ctx, 1001, f.ID, json.RawMessage(`{"run_id":"run-budget","status":"failed","evidence":{"error":"provider temporarily unavailable","output":"No verified repair was made"}}`)); err != nil {
		t.Fatal(err)
	}
	raw, err = h.NextDelivery(ctx, 1001)
	if err != nil {
		t.Fatal(err)
	}
	var notice struct {
		Text   string `json:"text"`
		TaskID string `json:"task_id"`
	}
	if err = json.Unmarshal(raw, &notice); err != nil {
		t.Fatal(err)
	}
	for _, fact := range []string{taskID, "provider temporarily unavailable", "failed", "1", "核验", "暂停", "保留", "？"} {
		if !strings.Contains(notice.Text, fact) {
			t.Fatalf("budget escalation omitted %q: %s", fact, raw)
		}
	}
	if notice.TaskID != taskID {
		t.Fatalf("escalation changed task: %s", raw)
	}
}
