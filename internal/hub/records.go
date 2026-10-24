package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"dots-duet-lite/internal/protocol"
	"dots-duet-lite/internal/store"
)

type taskRecord struct {
	protocol.Task
	Source               protocol.Source `json:"source"`
	RecoveryUsed         int             `json:"recovery_used"`
	RecoveryAttempts     int             `json:"recovery_attempts"`
	RecoveryStartedAt    time.Time       `json:"recovery_started_at"`
	GoalActive           bool            `json:"goal_active"`
	ProgressAt           time.Time       `json:"progress_at"`
	NextReconcileAt      time.Time       `json:"next_reconcile_at"`
	ReconcileFingerprint string          `json:"reconcile_fingerprint,omitempty"`
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
	Query       string `json:"query"`
	TaskID      string `json:"task_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	FolderID    string `json:"folder_id,omitempty"`
	Path        string `json:"path,omitempty"`
	Offset      int64  `json:"offset,omitempty"`
	Length      int64  `json:"length,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
}
type prepareArgs struct {
	Action           string           `json:"action"`
	TaskID           string           `json:"task_id,omitempty"`
	ProjectID        string           `json:"project_id,omitempty"`
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
func allowedProject(b Binding, id string) bool {
	for _, p := range b.ProjectIDs {
		if p == id {
			return true
		}
	}
	return false
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
	if a.ProjectID, err = folderID(a.ProjectID, a.FolderID); err != nil {
		return nil, err
	}
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
			result = publicTask(t.Task)
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
				out = append(out, publicTask(t.Task))
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
	if a.ProjectID, err = folderID(a.ProjectID, a.FolderID); err != nil {
		return nil, err
	}
	if a.WorkingDirectory != "" && !validWorkingDirectory(a.WorkingDirectory) {
		return nil, failure("invalid_arguments", "working_directory must be a relative slash-separated path inside the authorized folder")
	}
	upload, inputs, err := resolvePrepareFiles(a, inv)
	if err != nil {
		return nil, err
	}
	switch a.Action {
	case "task.create", "task.continue", "turn.steer":
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
	if a.Action == "task.create" {
		if taskID != "" {
			return nil, failure("invalid_arguments", "create cannot select an existing task")
		}
		taskID, err = randomID("task_")
		if err != nil {
			return nil, err
		}
	}
	var o operationRecord
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := checkAPIAttempt(tx, b, source); err != nil {
			return err
		}
		now := time.Now().UTC()
		var task taskRecord
		if a.Action == "task.create" {
			if !allowedProject(b, a.ProjectID) {
				return failure("forbidden", "project is not authorized")
			}
			task = taskRecord{Task: protocol.Task{ID: taskID, Scope: b.scope(), Name: a.Name, Objective: a.Input, ProjectID: a.ProjectID, WorkingDirectory: workingDirectory(a.WorkingDirectory), Version: 1, Execution: "prepared", Verification: "pending", Delivery: "pending", Artifacts: a.Artifacts, CreatedAt: now, UpdatedAt: now}, Source: source}
			if task.Name == "" {
				task.Name = taskID
			}
		} else {
			var err error
			task, err = loadTask(tx, b, taskID)
			if err != nil {
				return err
			}
			if a.ProjectID != "" && a.ProjectID != task.ProjectID {
				return failure("forbidden", "task project cannot be changed")
			}
			if a.WorkingDirectory != "" && a.WorkingDirectory != workingDirectory(task.WorkingDirectory) {
				return failure("forbidden", "task working directory cannot be changed")
			}
			if task.Paused && a.Action != "task.continue" {
				return failure("paused", "task is paused")
			}
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
			if (old.Stage == "unknown" || old.Stage == "dispatched") && (old.TaskID == taskID || old.ProjectID == task.ProjectID) {
				return failure("unknown_conflict", "an earlier operation needs reconciliation")
			}
		}
		o = operationRecord{Operation: protocol.Operation{ID: id, TaskID: taskID, Scope: b.scope(), TaskVersion: task.Version, ProjectID: task.ProjectID, WorkingDirectory: task.WorkingDirectory, Action: a.Action, ThreadID: task.ThreadID, ExpectedTurnID: a.ExpectedTurnID, Input: a.Input, Name: a.Name, Goal: a.Goal, OutputSchema: a.OutputSchema, Artifacts: a.Artifacts, Upload: upload, InputFiles: inputs}, Source: source, Stage: "prepared", CreatedAt: now, UpdatedAt: now}
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
