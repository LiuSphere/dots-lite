package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"dots-duet-lite/internal/protocol"
	"dots-duet-lite/internal/store"
)

type AllowedSource struct {
	Platform string `json:"platform"`
	SenderID string `json:"sender_id"`
	ChatID   string `json:"chat_id"`
}
type Binding struct {
	UID        int             `json:"uid"`
	UserID     string          `json:"user_id"`
	ProfileID  string          `json:"profile_id"`
	ExecutorID string          `json:"executor_id"`
	Token      string          `json:"token"`
	ProjectIDs []string        `json:"project_ids"`
	Sources    []AllowedSource `json:"sources"`
}

func (b Binding) scope() protocol.Scope {
	return protocol.Scope{UserID: b.UserID, ProfileID: b.ProfileID, ExecutorID: b.ExecutorID}
}

type Config struct {
	Bindings             []Binding `json:"bindings"`
	InvocationTTLSeconds int       `json:"invocation_ttl_seconds,omitempty"`
	FollowupBudget       int       `json:"followup_budget,omitempty"`
}
type invocation struct {
	mu              sync.Mutex
	UID             int
	Tool            string
	Arguments       string
	Source          protocol.Source
	Expires         time.Time
	Result          json.RawMessage
	TaskID          string
	TaskVersion     int64
	Confirmation    json.RawMessage
	Decision        string
	AttachmentFiles []protocol.InvocationAttachment
}
type Hub struct {
	cfg         Config
	db          *store.DB
	mu          sync.Mutex
	invocations map[string]*invocation
	sessions    map[string]*session
	closeOnce   sync.Once
	closeErr    error
	closed      bool
}

func New(cfg Config, db *store.DB) (*Hub, error) {
	if db == nil {
		return nil, errors.New("hub requires a state database")
	}
	if cfg.InvocationTTLSeconds == 0 {
		cfg.InvocationTTLSeconds = 300
	}
	if cfg.FollowupBudget == 0 {
		cfg.FollowupBudget = 8
	}
	if cfg.InvocationTTLSeconds < 1 || cfg.FollowupBudget < 1 {
		return nil, errors.New("invocation TTL and recovery budget must be positive")
	}
	seenUID := map[int]bool{}
	seenExec := map[string]bool{}
	seenToken := map[string]bool{}
	for _, b := range cfg.Bindings {
		if b.UID <= 0 || b.UserID == "" || b.ProfileID == "" || b.ExecutorID == "" || b.Token == "" || seenUID[b.UID] || seenExec[b.ExecutorID] || seenToken[b.Token] {
			return nil, errors.New("invalid or duplicate binding")
		}
		seenUID[b.UID] = true
		seenExec[b.ExecutorID] = true
		seenToken[b.Token] = true
	}
	h := &Hub{cfg: cfg, db: db, invocations: map[string]*invocation{}, sessions: map[string]*session{}}
	if err := h.recoverRecords(context.Background()); err != nil {
		return nil, err
	}
	return h, nil
}
func (h *Hub) binding(uid int) (Binding, error) {
	for _, b := range h.cfg.Bindings {
		if b.UID == uid {
			return b, nil
		}
	}
	return Binding{}, failure("forbidden", "caller has no binding")
}
func failure(code, message string) error { return &protocol.Error{Code: code, Message: message} }
func randomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
func owner(scope protocol.Scope) string { b, _ := json.Marshal(scope); return string(b) }
func canonical(raw json.RawMessage) (string, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return "", errors.New("arguments must contain exactly one JSON value")
	}
	b, err := json.Marshal(value)
	return string(b), err
}
func (h *Hub) RegisterInvocation(ctx context.Context, uid int, in protocol.Invocation) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	b, err := h.binding(uid)
	if err != nil {
		return "", err
	}
	if !knownTool(in.Tool) {
		return "", failure("invalid_tool", "unknown tool")
	}
	var boundTask string
	var boundVersion int64
	if in.Source.Kind == "api" {
		if in.Source.Platform != "api_server" || in.Source.ChatID == "" || in.Source.SessionID == "" || in.Source.NativeTaskID == "" || in.Source.ToolCallID == "" {
			return "", failure("unbound_source", "complete native API context is required")
		}
		err = h.db.Update(ctx, func(tx *store.Tx) error {
			var link apiLink
			err := tx.Get("api_session", owner(b.scope()), in.Source.ChatID, &link)
			if err != nil {
				return failure("unbound_source", "API session has no pre-registered task")
			}
			boundTask = link.TaskID
			boundVersion = link.TaskVersion
			return nil
		})
		if err != nil {
			return "", err
		}
	} else {
		if in.Source.Kind != "gateway" || in.Source.SessionID == "" {
			return "", failure("unbound_source", "a trusted gateway session is required")
		}
		allowed := false
		for _, s := range b.Sources {
			if s.Platform == in.Source.Platform && s.SenderID == in.Source.SenderID && s.ChatID == in.Source.ChatID {
				allowed = true
			}
		}
		if !allowed {
			return "", failure("forbidden", "message source is not bound to this caller")
		}
	}
	args, err := canonical(in.Arguments)
	if err != nil {
		return "", failure("invalid_arguments", "arguments are not JSON")
	}
	attachmentFiles, err := bindInvocationAttachments(in)
	if err != nil {
		return "", err
	}
	if in.Source.Kind == "api" && in.Tool == "duet_query" {
		var query queryArgs
		if err := decodeArgs(in.Arguments, &query); err != nil {
			return "", err
		}
		switch query.Query {
		case "operation":
			if query.OperationID == "" || (query.TaskID != "" && query.TaskID != boundTask) {
				return "", failure("binding_conflict", "API query must identify an operation of its registered task")
			}
		case "task", "operations", "followups", "deliveries", "session", "goal", "history", "requests", "artifacts", "directory", "text", "download":
			if query.TaskID != boundTask {
				return "", failure("binding_conflict", "API query must identify its registered task")
			}
		default:
			return "", failure("binding_conflict", "API followup cannot enumerate other tasks or their state")
		}
	}
	if in.Source.Kind == "gateway" && in.Tool != "duet_query" {
		var target struct {
			TaskID      string `json:"task_id"`
			OperationID string `json:"operation_id"`
		}
		if err = json.Unmarshal(in.Arguments, &target); err != nil {
			return "", failure("invalid_arguments", "arguments must be an object")
		}
		if target.TaskID != "" || target.OperationID != "" {
			err = h.db.Update(ctx, func(tx *store.Tx) error {
				id := target.TaskID
				if target.OperationID != "" {
					op, err := loadOperation(tx, b, target.OperationID)
					if err != nil {
						return err
					}
					id = op.TaskID
				}
				task, err := loadTask(tx, b, id)
				if err != nil {
					return err
				}
				boundTask, boundVersion = task.ID, task.Version
				return nil
			})
			if err != nil {
				return "", err
			}
		}
	}
	ref, err := randomID("ctx_")
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for k, v := range h.invocations {
		if now.After(v.Expires) {
			delete(h.invocations, k)
		}
	}
	h.invocations[ref] = &invocation{UID: uid, Tool: in.Tool, Arguments: args, Source: in.Source, Expires: now.Add(time.Duration(h.cfg.InvocationTTLSeconds) * time.Second), TaskID: boundTask, TaskVersion: boundVersion, AttachmentFiles: attachmentFiles}
	return ref, nil
}
func knownTool(s string) bool {
	switch s {
	case "duet_query", "duet_prepare", "duet_submit", "duet_stop", "duet_answer", "duet_approve", "duet_record":
		return true
	}
	return false
}
func (h *Hub) validate(uid int, req protocol.ToolRequest) (Binding, *invocation, error) {
	b, err := h.binding(uid)
	if err != nil {
		return b, nil, err
	}
	args, err := canonical(req.Arguments)
	if err != nil {
		return b, nil, failure("invalid_arguments", "arguments are not JSON")
	}
	h.mu.Lock()
	v, ok := h.invocations[req.ContextRef]
	h.mu.Unlock()
	if !ok || v.UID != uid || v.Tool != req.Tool || v.Arguments != args || time.Now().After(v.Expires) {
		return b, v, failure("invalid_context", "missing, expired or mismatched invocation reference")
	}
	return b, v, nil
}
func (h *Hub) Call(ctx context.Context, uid int, req protocol.ToolRequest) (json.RawMessage, error) {
	b, inv, err := h.validate(uid, req)
	if err != nil {
		return nil, err
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if req.Tool != "duet_query" && inv.Result != nil {
		return append(json.RawMessage(nil), inv.Result...), nil
	}
	if err = h.checkBoundTask(ctx, b, inv, req); err != nil {
		return nil, err
	}
	var result json.RawMessage
	switch req.Tool {
	case "duet_query":
		result, err = h.query(ctx, b, req.Arguments)
	case "duet_prepare":
		result, err = h.prepare(ctx, b, inv, req.Arguments)
	case "duet_submit":
		var a struct {
			OperationID string `json:"operation_id"`
		}
		if err = decodeArgs(req.Arguments, &a); err == nil {
			result, err = h.submit(ctx, b, a.OperationID)
		}
	case "duet_stop":
		var a struct {
			TaskID string `json:"task_id"`
		}
		if err = decodeArgs(req.Arguments, &a); err == nil {
			result, err = h.stop(ctx, b, inv.Source, a.TaskID)
		}
	case "duet_approve":
		result, err = h.approval(ctx, b, inv, req.Arguments)
	case "duet_answer":
		result, err = h.answer(ctx, b, inv, req.Arguments)
	case "duet_record":
		result, err = h.record(ctx, b, inv, req.Arguments)
	default:
		err = failure("not_implemented", fmt.Sprintf("tool %s is not implemented", req.Tool))
	}
	if err == nil && req.Tool != "duet_query" {
		inv.Result = append(json.RawMessage(nil), result...)
	}
	return result, err
}
