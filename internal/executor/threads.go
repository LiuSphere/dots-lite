package executor

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"dots-lite/internal/protocol"
)

type nativeThread struct {
	CanAcceptDirectInput *bool           `json:"canAcceptDirectInput"`
	ID                   string          `json:"id"`
	Name                 string          `json:"name"`
	Preview              string          `json:"preview"`
	Cwd                  string          `json:"cwd"`
	UpdatedAt            int64           `json:"updatedAt"`
	Source               json.RawMessage `json:"source"`
	ModelProvider        string          `json:"modelProvider"`
	Model                string          `json:"model"`
	Status               struct {
		Type string `json:"type"`
	} `json:"status"`
}

type threadCursor struct {
	Mode     string `json:"mode"`
	Archived bool   `json:"archived"`
	Cursor   string `json:"cursor"`
}

type attachmentFailure struct {
	Code       string    `json:"code"`
	Message    string    `json:"message"`
	ObservedAt time.Time `json:"observed_at"`
}

func (s *Service) threadQuery(ctx context.Context, op protocol.Operation, client *Client) protocol.Result {
	r := protocol.Result{OperationID: op.ID, Stage: "failed"}
	if client == nil {
		return failResult(r, "not_ready", "app-server is not initialized")
	}
	if op.Action == "thread.read" {
		thread, err := readThread(ctx, client, op.ThreadID)
		if err != nil {
			return failResult(r, "native", err.Error())
		}
		view, err := s.threadView(ctx, thread, op.Archived == "yes")
		if err != nil {
			return failResult(r, "storage", err.Error())
		}
		view["archived"] = nil
		view["archive_status_reason"] = "thread/read does not establish archive state; use the native inventory archive branches"
		r.Stage, r.Data = "accepted", mustJSON(view)
		return r
	}
	mode := op.Archived
	if mode == "" {
		mode = "all"
	}
	if mode != "all" && mode != "yes" && mode != "no" {
		return failResult(r, "invalid", "archived must be all, yes or no")
	}
	cursor := threadCursor{Mode: mode, Archived: mode == "yes"}
	if op.Cursor != "" {
		encoded, err := base64.RawURLEncoding.DecodeString(op.Cursor)
		if err != nil || json.Unmarshal(encoded, &cursor) != nil || cursor.Mode != mode || (mode == "no" && cursor.Archived) || (mode == "yes" && !cursor.Archived) {
			return failResult(r, "invalid_cursor", "cursor does not match the archive selection")
		}
	}
	s.mu.Lock()
	sources := append([]string(nil), s.config.ThreadSourceKinds...)
	schemaError := s.config.SourceSchemaError
	s.mu.Unlock()
	if len(sources) == 0 {
		return failResult(r, "source_schema_unverified", "cannot claim a complete native inventory without the installed source schema: "+schemaError)
	}
	params := map[string]any{"modelProviders": []string{}, "sourceKinds": sources, "archived": cursor.Archived, "limit": 50}
	if cursor.Cursor != "" {
		params["cursor"] = cursor.Cursor
	}
	data, err := client.Call(ctx, "thread/list", params)
	if err != nil {
		return failResult(r, "native", err.Error())
	}
	var page struct {
		Data       []nativeThread `json:"data"`
		NextCursor string         `json:"nextCursor"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		return failResult(r, "native_response", err.Error())
	}
	if page.Data == nil {
		return failResult(r, "native_response", "thread/list omitted its data array; completeness is unverified")
	}
	views := []map[string]any{}
	for _, thread := range page.Data {
		if thread.ID == "" {
			return failResult(r, "native_response", "native inventory returned an entry without a thread identity")
		}
		view, err := s.threadView(ctx, thread, cursor.Archived)
		if err != nil {
			return failResult(r, "storage", err.Error())
		}
		views = append(views, view)
	}
	next := ""
	if page.NextCursor != "" {
		cursor.Cursor = page.NextCursor
		next = base64.RawURLEncoding.EncodeToString(mustJSON(cursor))
	} else if mode == "all" && !cursor.Archived {
		cursor.Archived, cursor.Cursor = true, ""
		next = base64.RawURLEncoding.EncodeToString(mustJSON(cursor))
	}
	r.Stage, r.Data = "accepted", mustJSON(map[string]any{"data": views, "next_cursor": next, "scope": s.config.Scope, "observation_scope": "this executor app-server and configured Codex user environment"})
	return r
}

func readThread(ctx context.Context, client *Client, id string) (nativeThread, error) {
	var result struct {
		Thread nativeThread `json:"thread"`
	}
	if id == "" {
		return result.Thread, errors.New("thread_id is required")
	}
	data, err := client.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
	if err != nil {
		return result.Thread, err
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return result.Thread, err
	}
	if result.Thread.ID != id || result.Thread.Cwd == "" || result.Thread.ModelProvider == "" {
		return result.Thread, errors.New("native thread identity, directory or provider is unverified")
	}
	return result.Thread, nil
}

func (s *Service) threadView(ctx context.Context, thread nativeThread, archived bool) (map[string]any, error) {
	availability, reason := "needs_check", "native ownership, Goal and permissions require protected attachment; notLoaded is local to this instance"
	if thread.Status.Type == "active" {
		availability, reason = "unavailable", "active thread: no verified control entry for an external writer"
	}
	if thread.CanAcceptDirectInput != nil && !*thread.CanAcceptDirectInput {
		availability, reason = "unavailable", "native thread does not accept direct input through this control entry"
	}
	if s.config.DirectoryMode == "allowlist" {
		allowed := false
		s.mu.Lock()
		for id := range s.folders {
			p, ok := s.folderLocked(id)
			if ok && p.Available && pathWithin(p.Root, thread.Cwd) {
				allowed = true
				break
			}
		}
		s.mu.Unlock()
		if !allowed {
			availability, reason = "unavailable", "directory is outside the configured allowlist"
		}
	}
	var taskID string
	err := s.db.QueryRowContext(ctx, `SELECT task_id FROM executor_tasks WHERE json_extract(payload,'$.thread_id')=?`, thread.ID).Scan(&taskID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var access string
	var lastFailure *attachmentFailure
	err = s.db.QueryRowContext(ctx, `SELECT value FROM executor_meta WHERE key=?`, "thread_access:"+thread.ID).Scan(&access)
	if err == nil {
		var failure attachmentFailure
		if err := json.Unmarshal([]byte(access), &failure); err != nil {
			return nil, err
		}
		lastFailure = &failure
		if availability == "needs_check" {
			reason = "last attachment failed; current cause needs recheck: " + failure.Code + ": " + failure.Message
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return map[string]any{"thread_id": thread.ID, "name": thread.Name, "preview": thread.Preview, "working_directory": thread.Cwd, "updated_at": thread.UpdatedAt, "source": thread.Source, "archived": archived, "runtime_status": thread.Status.Type, "task_id": taskID, "availability": availability, "reason": reason, "last_attachment_error": lastFailure, "observed_at": time.Now().UTC(), "instance_id": s.instance, "model_provider": thread.ModelProvider, "model": thread.Model}, nil
}

func (s *Service) attachThread(ctx context.Context, op protocol.Operation, client *Client) (r protocol.Result) {
	r = protocol.Result{OperationID: op.ID, TaskID: op.TaskID, ThreadID: op.ThreadID, Stage: "failed"}
	defer func() {
		if r.Error != nil {
			_, err := s.db.ExecContext(context.WithoutCancel(ctx), `INSERT INTO executor_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "thread_access:"+op.ThreadID, string(mustJSON(attachmentFailure{Code: r.Error.Code, Message: r.Error.Message, ObservedAt: time.Now().UTC()})))
			if err != nil {
				r.Stage = "unknown"
				r = failResult(r, "storage", err.Error())
			}
		}
	}()
	thread, err := readThread(ctx, client, op.ThreadID)
	if err != nil {
		return failResult(r, "state_unknown", err.Error())
	}
	if thread.Status.Type != "idle" && thread.Status.Type != "notLoaded" {
		return failResult(r, "active_writer", "native thread is active or unknown; no competing writer was started")
	}
	if thread.CanAcceptDirectInput != nil && !*thread.CanAcceptDirectInput {
		return failResult(r, "control_unavailable", "native thread does not accept direct input")
	}
	if !filepath.IsAbs(op.WorkingDirectory) || !samePath(op.WorkingDirectory, thread.Cwd) {
		return failResult(r, "directory_unavailable", "selected directory does not match the native thread")
	}
	goal, err := readNativeGoalStatus(ctx, client, thread.ID)
	if err != nil || !quietGoal(goal) {
		return failResult(r, "state_unknown", fmt.Sprintf("native Goal must be verified quiet: %s %v", goal, err))
	}
	s.mu.Lock()
	root, cwd, err := s.openTaskRootLocked(op.FolderID, op.WorkingDirectory)
	s.mu.Unlock()
	if err != nil {
		return failResult(r, "directory_unavailable", err.Error())
	}
	defer root.Close()
	task := protocol.Task{ID: op.TaskID, Scope: op.Scope, Name: op.Name, Objective: op.Input, FolderID: op.FolderID, WorkingDirectory: op.WorkingDirectory, ThreadID: thread.ID, DirectoryMode: s.config.DirectoryMode, Version: op.TaskVersion, Paused: true, Execution: "attached", Verification: "pending", Delivery: "pending", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), ModelProvider: thread.ModelProvider, Model: thread.Model}
	old, loadErr := s.loadTask(ctx, task.ID)
	if loadErr == nil {
		if old.ThreadID != task.ThreadID || old.WorkingDirectory != task.WorkingDirectory || !old.Paused {
			return failResult(r, "conflict", "existing attachment identity or pause does not match")
		}
		task = old
		if err := s.claimTask(ctx, task); err != nil {
			return failResult(r, "conflict", err.Error())
		}
	} else if errors.Is(loadErr, sql.ErrNoRows) {
		if err := s.insertTask(ctx, task, cwd); err != nil {
			return failResult(r, "conflict", err.Error())
		}
	} else {
		return failResult(r, "storage", loadErr.Error())
	}
	if op.Archived == "yes" {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO executor_meta(key,value) VALUES(?,?)`, "attach_stage:"+op.ID, "unarchive_submitted"); err != nil {
			return failResult(r, "storage", err.Error())
		}
		if _, err := s.nativeWrite(ctx, client, op, "thread/unarchive", map[string]string{"threadId": thread.ID}); err != nil {
			return s.recordNativeFailure(ctx, r, task.ID, err)
		}
	}
	data, err := s.resumePreservingPolicy(ctx, client, op, task, cwd)
	if err != nil {
		result := s.recordNativeFailure(ctx, r, task.ID, err)
		if result.Error != nil && strings.Contains(strings.ToLower(err.Error()), "active writer") {
			result.Error.Code = "active_writer"
		}
		return result
	}
	task.NativePolicy = data
	goal, err = readNativeGoalStatus(ctx, client, thread.ID)
	if err != nil || !quietGoal(goal) {
		r.Stage = "unknown"
		return failResult(r, "state_unknown", fmt.Sprintf("native Goal is not verified quiet after resume: %s %v", goal, err))
	}
	status, err := readNativeStatus(ctx, client, thread.ID)
	if err != nil || status != "idle" {
		r.Stage = "unknown"
		return failResult(r, "state_unknown", fmt.Sprintf("native thread is not verified idle after resume: %s %v", status, err))
	}
	if err := s.saveTask(ctx, task); err != nil {
		r.Stage = "unknown"
		return failResult(r, "storage", err.Error())
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM executor_meta WHERE key=?`, "thread_access:"+thread.ID); err != nil {
		return failResult(r, "storage", err.Error())
	}
	r.Stage, r.Data = "accepted", mustJSON(task)
	return r
}

func samePath(a, b string) bool { rel, err := filepath.Rel(a, b); return err == nil && rel == "." }

func (s *Service) resumePreservingPolicy(ctx context.Context, client *Client, op protocol.Operation, task protocol.Task, cwd string) (json.RawMessage, error) {
	params := map[string]any{"threadId": task.ThreadID, "excludeTurns": true}
	if task.ModelProvider != "" {
		params["modelProvider"] = task.ModelProvider
	}
	if task.Model != "" {
		params["model"] = task.Model
	}
	data, err := s.nativeWrite(ctx, client, op, "thread/resume", params)
	if err != nil {
		return nil, err
	}
	var result struct {
		Cwd               string          `json:"cwd"`
		Reviewer          json.RawMessage `json:"approvalsReviewer"`
		WorkspaceRoots    json.RawMessage `json:"runtimeWorkspaceRoots"`
		PermissionProfile json.RawMessage `json:"activePermissionProfile"`
		Thread            nativeThread    `json:"thread"`
		ApprovalPolicy    json.RawMessage `json:"approvalPolicy"`
		Sandbox           json.RawMessage `json:"sandbox"`
		ModelProvider     string          `json:"modelProvider"`
		Model             string          `json:"model"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result.Thread.ID != task.ThreadID || !samePath(cwd, result.Thread.Cwd) || !samePath(cwd, result.Cwd) {
		return nil, errors.New("resumed thread identity or cwd changed")
	}
	if result.Thread.CanAcceptDirectInput == nil || !*result.Thread.CanAcceptDirectInput {
		return nil, errors.New("native direct input capability is unavailable after protected resume")
	}
	provider := result.ModelProvider
	if provider == "" {
		provider = result.Thread.ModelProvider
	}
	if task.ModelProvider != "" && provider != task.ModelProvider {
		return nil, errors.New("resumed provider differs from the original thread")
	}
	if task.Model != "" && result.Model != task.Model {
		return nil, errors.New("resumed model differs from the original thread")
	}
	if len(result.ApprovalPolicy) == 0 || len(result.Sandbox) == 0 {
		return nil, errors.New("permission_unverified: native resume did not expose actual permissions")
	}
	policy := mustJSON(map[string]json.RawMessage{"approvalPolicy": result.ApprovalPolicy, "sandbox": result.Sandbox, "approvalsReviewer": result.Reviewer, "runtimeWorkspaceRoots": result.WorkspaceRoots, "activePermissionProfile": result.PermissionProfile})
	if len(task.NativePolicy) > 0 {
		var old, actual any
		if json.Unmarshal(task.NativePolicy, &old) != nil || json.Unmarshal(policy, &actual) != nil || !reflect.DeepEqual(old, actual) {
			return nil, errors.New("permission_unverified: native permissions changed; user decision is required")
		}
	}
	return policy, nil
}
