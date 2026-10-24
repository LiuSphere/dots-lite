package hub

import (
	"context"
	"encoding/json"
	"time"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
)

type pendingRequest struct {
	RequestID  string          `json:"request_id"`
	InstanceID string          `json:"instance_id"`
	Method     string          `json:"method"`
	Params     json.RawMessage `json:"params"`
}
type responseArgs struct {
	TaskID    string          `json:"task_id"`
	RequestID string          `json:"request_id"`
	Answer    json.RawMessage `json:"answer,omitempty"`
}

func (h *Hub) request(ctx context.Context, b Binding, taskID, requestID string) (taskRecord, pendingRequest, error) {
	var task taskRecord
	err := h.db.Update(ctx, func(tx *store.Tx) error { var err error; task, err = loadTask(tx, b, taskID); return err })
	if err != nil {
		return task, pendingRequest{}, err
	}
	if task.Paused {
		return task, pendingRequest{}, failure("paused", "task is paused")
	}
	data, err := h.forwardQuery(ctx, b, queryArgs{Query: "requests", TaskID: taskID})
	if err != nil {
		return task, pendingRequest{}, err
	}
	var requests []pendingRequest
	if err = json.Unmarshal(data, &requests); err != nil {
		return task, pendingRequest{}, err
	}
	for _, r := range requests {
		if r.RequestID == requestID && r.InstanceID != "" {
			return task, r, nil
		}
	}
	return task, pendingRequest{}, failure("expired_request", "native request is no longer pending")
}
func (h *Hub) approval(ctx context.Context, b Binding, inv *invocation, raw json.RawMessage) (json.RawMessage, error) {
	var a responseArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if len(a.Answer) > 0 {
		return nil, failure("invalid_arguments", "approval decisions cannot be model arguments")
	}
	task, pending, err := h.request(ctx, b, a.TaskID, a.RequestID)
	if err != nil {
		return nil, err
	}
	if inv.Source.Kind != "gateway" || inv.Source.Platform != task.Source.Platform || inv.Source.SenderID != task.Source.SenderID || inv.Source.ChatID != task.Source.ChatID || inv.Source.ThreadID != task.Source.ThreadID {
		return nil, failure("forbidden", "approval must return to the original user and chat")
	}
	switch pending.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "execCommandApproval", "applyPatchApproval":
	default:
		return nil, failure("unsupported_confirmation", "this native request needs a structured human decision")
	}
	var params struct {
		AvailableDecisions json.RawMessage `json:"availableDecisions"`
	}
	if err = json.Unmarshal(pending.Params, &params); err != nil {
		return nil, err
	}
	if len(params.AvailableDecisions) > 0 && string(params.AvailableDecisions) != "null" {
		var decisions []string
		if err = json.Unmarshal(params.AvailableDecisions, &decisions); err != nil {
			return nil, failure("unsupported_confirmation", "native approval decisions cannot be reduced to one confirmation")
		}
		accept, decline := false, false
		for _, d := range decisions {
			if d == "accept" {
				accept = true
			}
			if d == "decline" {
				decline = true
			}
		}
		if !accept || !decline {
			return nil, failure("unsupported_confirmation", "native approval does not support accept and decline")
		}
	}
	inv.TaskID = task.ID
	inv.TaskVersion = task.Version
	return json.Marshal(map[string]any{"approval": map[string]any{"task_id": task.ID, "request_id": pending.RequestID, "instance_id": pending.InstanceID, "method": pending.Method, "params": pending.Params}})
}

type Confirmation struct {
	protocol.ToolRequest
	RequestID  string `json:"request_id"`
	InstanceID string `json:"instance_id"`
	Decision   string `json:"decision"`
}

func (h *Hub) ConfirmApproval(ctx context.Context, uid int, in Confirmation) (json.RawMessage, error) {
	if in.Tool != "dots_lite_approve" || (in.Decision != "accept" && in.Decision != "decline") {
		return nil, failure("invalid_confirmation", "only a single accept or decline is allowed")
	}
	b, inv, err := h.validate(uid, in.ToolRequest)
	if err != nil {
		return nil, err
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if inv.Confirmation != nil {
		if inv.Decision != in.Decision {
			return nil, failure("binding_conflict", "confirmation was already consumed with another decision")
		}
		return inv.Confirmation, nil
	}
	var a responseArgs
	if err = decodeArgs(in.Arguments, &a); err != nil {
		return nil, err
	}
	if inv.Result == nil || in.RequestID != a.RequestID || inv.TaskID != a.TaskID {
		return nil, failure("invalid_confirmation", "approval must first be presented by this invocation")
	}
	task, pending, err := h.request(ctx, b, a.TaskID, a.RequestID)
	if err != nil {
		return nil, err
	}
	if task.Version != inv.TaskVersion || pending.InstanceID != in.InstanceID {
		return nil, failure("expired_request", "task or native request changed during confirmation")
	}
	reply, _ := json.Marshal(map[string]string{"decision": in.Decision})
	out, err := h.submitReply(ctx, b, inv.Source, task, pending, "approval.respond", reply)
	if err != nil {
		return nil, err
	}
	inv.Confirmation = out
	inv.Decision = in.Decision
	return out, nil
}
func (h *Hub) answer(ctx context.Context, b Binding, inv *invocation, raw json.RawMessage) (json.RawMessage, error) {
	var a responseArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	task, pending, err := h.request(ctx, b, a.TaskID, a.RequestID)
	if err != nil {
		return nil, err
	}
	if pending.Method != "tool/requestUserInput" {
		return nil, failure("human_approval_required", "this request cannot be answered as a technical question")
	}
	var params struct {
		Questions []struct {
			ID string `json:"id"`
		} `json:"questions"`
	}
	if err = json.Unmarshal(pending.Params, &params); err != nil {
		return nil, err
	}
	var answer struct {
		Answers map[string]struct {
			Answers []string `json:"answers"`
		} `json:"answers"`
	}
	if err = decodeArgs(a.Answer, &answer); err != nil {
		return nil, err
	}
	if len(params.Questions) == 0 {
		return nil, failure("invalid_request", "native question has no question identifiers")
	}
	for _, q := range params.Questions {
		v, ok := answer.Answers[q.ID]
		if !ok || len(v.Answers) == 0 {
			return nil, failure("invalid_answer", "each native question requires a structured answer")
		}
	}
	return h.submitReply(ctx, b, inv.Source, task, pending, "question.respond", a.Answer)
}
func (h *Hub) submitReply(ctx context.Context, b Binding, source protocol.Source, task taskRecord, pending pendingRequest, action string, reply json.RawMessage) (json.RawMessage, error) {
	id, err := randomID("op_")
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	op := operationRecord{Operation: protocol.Operation{ID: id, TaskID: task.ID, Scope: b.scope(), TaskVersion: task.Version, ProjectID: task.ProjectID, WorkingDirectory: task.WorkingDirectory, ThreadID: task.ThreadID, ExpectedTurnID: task.TurnID, Action: action, RequestID: pending.RequestID, InstanceID: pending.InstanceID, Reply: reply}, Stage: "prepared", Source: source, CreatedAt: now, UpdatedAt: now}
	if err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := checkAPIAttempt(tx, b, source); err != nil {
			return err
		}
		return tx.Put("operation", owner(b.scope()), id, op)
	}); err != nil {
		return nil, err
	}
	return h.submit(ctx, b, id)
}
