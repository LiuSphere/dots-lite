package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
)

type taskRecord struct {
	SubmissionAttempts int `json:"submission_attempts"`
	RunsAccepted       int `json:"runs_accepted"`
	RunsCompleted      int `json:"runs_completed"`
	protocol.Task
	Source               protocol.Source `json:"source"`
	RecoveryUsed         int             `json:"recovery_used"`
	RecoveryAttempts     int             `json:"recovery_attempts"`
	RecoveryStartedAt    time.Time       `json:"recovery_started_at"`
	GoalActive           bool            `json:"goal_active"`
	ProgressAt           time.Time       `json:"progress_at"`
	NextReconcileAt      time.Time       `json:"next_reconcile_at"`
	ReconcileFingerprint string          `json:"reconcile_fingerprint,omitempty"`
	AttachedMessageID    string          `json:"attached_message_id,omitempty"`
}
type operationRecord struct {
	protocol.Operation
	Stage               string           `json:"stage"`
	Source              protocol.Source  `json:"source"`
	Result              *protocol.Result `json:"result,omitempty"`
	ReconciliationError *protocol.Error  `json:"reconciliation_error,omitempty"`
	CreatedAt           time.Time        `json:"created_at"`
	UpdatedAt           time.Time        `json:"updated_at"`
}
type queryArgs struct {
	ThreadID    string `json:"thread_id,omitempty"`
	Archived    string `json:"archived,omitempty"`
	Query       string `json:"query"`
	TaskID      string `json:"task_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	FolderID    string `json:"folder_id,omitempty"`
	Path        string `json:"path,omitempty"`
	Offset      int64  `json:"offset,omitempty"`
	Length      int64  `json:"length,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
}
type prepareArgs struct {
	Archived         string           `json:"archived,omitempty"`
	ThreadID         string           `json:"thread_id,omitempty"`
	Action           string           `json:"action"`
	TaskID           string           `json:"task_id,omitempty"`
	FolderID         string           `json:"folder_id,omitempty"`
	WorkingDirectory string           `json:"working_directory,omitempty"`
	Input            string           `json:"input,omitempty"`
	Name             string           `json:"name,omitempty"`
	ExpectedTurnID   string           `json:"expected_turn_id,omitempty"`
	Goal             json.RawMessage  `json:"goal,omitempty"`
	OutputSchema     json.RawMessage  `json:"output_schema,omitempty"`
	Artifacts        []string         `json:"artifacts,omitempty"`
	Upload           json.RawMessage  `json:"upload,omitempty"`
	Inputs           []inputReference `json:"inputs,omitempty"`
}

func decodeArgs(raw json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return failure("invalid_arguments", err.Error())
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return failure("invalid_arguments", "arguments must contain exactly one JSON value")
	}
	return nil
}
func allowedFolder(b Binding, id string) bool {
	if b.DirectoryMode == "task" && id == "" {
		return true
	}
	for _, p := range b.FolderIDs {
		if p == id {
			return true
		}
	}
	return false
}

func businessWaiting(reason string) bool {
	return reason == "user_decision" || reason == "recovery_budget_exhausted" || reason == "submission_unknown" || reason == "migration_review"
}
func loadTask(tx *store.Tx, b Binding, id string) (taskRecord, error) {
	var t taskRecord
	if err := tx.Get("task", owner(b.scope()), id, &t); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return t, failure("not_found", "task not found in caller scope")
		}
		return t, err
	}
	return t, nil
}
func loadOperation(tx *store.Tx, b Binding, id string) (operationRecord, error) {
	var o operationRecord
	if err := tx.Get("operation", owner(b.scope()), id, &o); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return o, failure("not_found", "operation not found in caller scope")
		}
		return o, err
	}
	return o, nil
}
func (h *Hub) query(ctx context.Context, b Binding, raw json.RawMessage) (json.RawMessage, error) {
	var a queryArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	var err error
	if a.Query == "machine" {
		return json.Marshal(h.machine(b))
	}
	if a.Query == "followups" {
		out := []json.RawMessage{}
		err := h.db.Update(ctx, func(tx *store.Tx) error {
			rows, err := tx.List("followup", owner(b.scope()))
			if err != nil {
				return err
			}
			for _, raw := range rows {
				var f followup
				if err := json.Unmarshal(raw, &f); err != nil {
					return err
				}
				if a.TaskID == "" || f.TaskID == a.TaskID {
					out = append(out, raw)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(out) > 100 {
			out = out[:100]
		}
		return json.Marshal(out)
	}
	if a.Query != "task" && a.Query != "tasks" && a.Query != "operation" && a.Query != "operations" {
		return h.forwardQuery(ctx, b, a)
	}
	if a.Query == "operation" {
		if err := h.reconcileOperation(ctx, b, a.OperationID); err != nil {
			return nil, err
		}
	}
	var result any
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		switch a.Query {
		case "task":
			t, err := loadTask(tx, b, a.TaskID)
			if err != nil {
				return err
			}
			result = publicTask(t)
		case "operation":
			o, err := loadOperation(tx, b, a.OperationID)
			if err != nil {
				return err
			}
			result = publicOperation(o)
		case "tasks":
			rows, err := tx.List("task", owner(b.scope()))
			if err != nil {
				return err
			}
			out := []publicTaskView{}
			for i, r := range rows {
				if i == 100 {
					break
				}
				var t taskRecord
				if err := json.Unmarshal(r, &t); err != nil {
					return err
				}
				out = append(out, publicTask(t))
			}
			result = out
		case "operations":
			rows, err := tx.List("operation", owner(b.scope()))
			if err != nil {
				return err
			}
			out := []publicOperationView{}
			for _, r := range rows {
				var o operationRecord
				if err := json.Unmarshal(r, &o); err != nil {
					return err
				}
				if a.TaskID == "" || o.TaskID == a.TaskID {
					out = append(out, publicOperation(o))
				}
				if len(out) == 100 {
					break
				}
			}
			result = out
		default:
			return failure("invalid_query", "query is unavailable")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}
func (h *Hub) prepare(ctx context.Context, b Binding, inv *invocation, raw json.RawMessage) (json.RawMessage, error) {
	source := inv.Source
	var a prepareArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	var err error
	if a.WorkingDirectory != "" && !validWorkingDirectory(a.WorkingDirectory) {
		return nil, failure("invalid_arguments", "working_directory must be a relative slash-separated path inside the authorized folder")
	}
	upload, inputs, err := resolvePrepareFiles(a, inv)
	if err != nil {
		return nil, err
	}
	switch a.Action {
	case "task.create", "task.attach", "task.continue", "turn.steer":
		if a.Input == "" {
			return nil, failure("invalid_arguments", "input is required")
		}
	case "goal.set":
		if len(a.Goal) == 0 {
			return nil, failure("invalid_arguments", "goal is required")
		}
	case "files.upload":
	default:
		return nil, failure("invalid_action", "action cannot be prepared")
	}
	id, err := randomID("op_")
	if err != nil {
		return nil, err
	}
	taskID := a.TaskID
	if a.Action == "task.create" || a.Action == "task.attach" {
		if taskID != "" {
			return nil, failure("invalid_arguments", "create cannot select an existing task")
		}
		taskID, err = randomID("task_")
		if err != nil {
			return nil, err
		}
	}
	var o operationRecord
	reuseAttach := false
	if a.Action == "task.attach" {
		if source.Kind != "gateway" || a.ThreadID == "" || inv.DirectorySelection == nil || inv.DirectorySelection.ThreadID != a.ThreadID {
			return nil, failure("needs_directory_selection", "select the native thread in the current user message")
		}
		var bound string
		err := h.db.Update(ctx, func(tx *store.Tx) error {
			err := tx.Get("thread_binding", owner(b.scope()), a.ThreadID, &bound)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		if bound != "" {
			var task taskRecord
			if err := h.db.Update(ctx, func(tx *store.Tx) error { var err error; task, err = loadTask(tx, b, bound); return err }); err != nil {
				return nil, err
			}
			if len(task.NativePolicy) > 0 || !task.Paused {
				return json.Marshal(map[string]string{"task_id": bound, "stage": "existing_binding", "next": "query task and continue the existing binding"})
			}
			taskID, reuseAttach = bound, true
		}
		data, err := h.forwardQuery(ctx, b, queryArgs{Query: "thread", ThreadID: a.ThreadID})
		if err != nil {
			return nil, err
		}
		var view struct {
			Cwd string `json:"working_directory"`
		}
		if json.Unmarshal(data, &view) != nil || !absoluteDirectory(view.Cwd) {
			return nil, failure("directory_unavailable", "native directory is unverified")
		}
		if a.WorkingDirectory != "" && a.WorkingDirectory != view.Cwd {
			return nil, failure("directory_unavailable", "native cwd cannot be replaced")
		}
		a.WorkingDirectory = view.Cwd
	}
	if a.Action == "task.create" && (b.DirectoryMode == "task" || absoluteDirectory(a.WorkingDirectory)) {
		if !absoluteDirectory(a.WorkingDirectory) || inv.DirectorySelection == nil || inv.DirectorySelection.Path != a.WorkingDirectory {
			return nil, failure("needs_directory_selection", "an absolute directory selected in the current admitted message is required")
		}
	}
	epoch, phase := h.maintenanceState(b)
	if phase != "running" {
		return nil, failure("maintenance", "executor is not accepting writes")
	}
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := checkAPIAttempt(tx, b, source); err != nil {
			return err
		}
		now := time.Now().UTC()
		var task taskRecord
		if a.Action == "task.create" || (a.Action == "task.attach" && !reuseAttach) {
			if !allowedFolder(b, a.FolderID) {
				return failure("forbidden", "folder is not authorized")
			}
			task = taskRecord{Task: protocol.Task{ID: taskID, Scope: b.scope(), Name: a.Name, Objective: a.Input, FolderID: a.FolderID, WorkingDirectory: workingDirectory(a.WorkingDirectory), Version: 1, Execution: "prepared", Verification: "pending", Delivery: "pending", Artifacts: a.Artifacts, CreatedAt: now, UpdatedAt: now}, Source: source}
			task.DirectoryMode = b.DirectoryMode
			if a.Action == "task.attach" {
				var binding string
				err := tx.Get("thread_binding", owner(b.scope()), a.ThreadID, &binding)
				if err == nil {
					return failure("already_bound", "native thread already belongs to task "+binding)
				}
				if !errors.Is(err, store.ErrNotFound) {
					return err
				}
				task.ThreadID, task.Paused, task.AttachedMessageID = a.ThreadID, true, source.MessageID
				if err := tx.Put("thread_binding", owner(b.scope()), a.ThreadID, taskID); err != nil {
					return err
				}
			}
			if task.Name == "" {
				task.Name = taskID
			}
		} else {
			var err error
			task, err = loadTask(tx, b, taskID)
			if err != nil {
				return err
			}
			if a.FolderID != "" && a.FolderID != task.FolderID {
				return failure("forbidden", "task folder cannot be changed")
			}
			if a.WorkingDirectory != "" && a.WorkingDirectory != workingDirectory(task.WorkingDirectory) {
				return failure("forbidden", "task working directory cannot be changed")
			}
			if task.Paused && a.Action != "task.continue" && a.Action != "task.attach" {
				return failure("paused", "task is paused")
			}
			if a.Action == "task.continue" && task.AttachedMessageID != "" && (source.Kind != "gateway" || source.MessageID == task.AttachedMessageID) {
				return failure("permission_unverified", "review the attached native permissions and request continuation in a new user message")
			}
			if a.Action == "task.continue" && task.AttachedMessageID != "" && len(task.NativePolicy) == 0 {
				return failure("permission_unverified", "retry attachment and inspect actual native permissions before continuing")
			}
		}
		if a.Action == "task.attach" {
			task.AttachedMessageID = source.MessageID
		}
		rows, err := tx.List("operation", owner(b.scope()))
		if err != nil {
			return err
		}
		for _, r := range rows {
			var old operationRecord
			if err := json.Unmarshal(r, &old); err != nil {
				return err
			}
			if (old.Stage == "unknown" || old.Stage == "dispatched") && (old.TaskID == taskID || (task.FolderID != "" && old.FolderID == task.FolderID)) {
				return failure("unknown_conflict", "an earlier operation needs reconciliation")
			}
		}
		o = operationRecord{Operation: protocol.Operation{ID: id, TaskID: taskID, Scope: b.scope(), TaskVersion: task.Version, FolderID: task.FolderID, WorkingDirectory: task.WorkingDirectory, Action: a.Action, ThreadID: task.ThreadID, ExpectedTurnID: a.ExpectedTurnID, Input: a.Input, Name: a.Name, Goal: a.Goal, OutputSchema: a.OutputSchema, Artifacts: a.Artifacts, Upload: upload, InputFiles: inputs}, Source: source, Stage: "prepared", CreatedAt: now, UpdatedAt: now}
		o.MaintenanceEpoch = epoch
		o.Foreground = source.Kind == "gateway"
		o.Archived = a.Archived
		if err := tx.Put("task", owner(b.scope()), taskID, task); err != nil {
			return err
		}
		return tx.Put("operation", owner(b.scope()), id, o)
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"task_id": taskID, "operation_id": id, "stage": o.Stage, "task_version": o.TaskVersion})
}
