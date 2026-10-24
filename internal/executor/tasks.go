package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"dots-lite/internal/protocol"
)

func (s *Service) taskOperation(ctx context.Context, op protocol.Operation, client *Client) protocol.Result {
	r := protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "failed"}
	task, err := s.loadTask(ctx, op.TaskID)
	if err != nil {
		return failResult(r, "task_not_found", err.Error())
	}
	if task.FolderID != op.FolderID || (op.ThreadID != "" && op.ThreadID != task.ThreadID) {
		return failResult(r, "forbidden", "task folder or thread does not match")
	}
	r.ThreadID, r.TurnID = task.ThreadID, task.TurnID
	readonly := op.Action == "task.read" || op.Action == "task.history" || op.Action == "goal.get"
	if !readonly && op.TaskVersion != task.Version {
		if op.Action != "task.continue" || !task.Paused || op.TaskVersion <= task.Version {
			return failResult(r, "stale_version", "operation version does not match task")
		}
	}
	if task.Paused && !readonly && op.Action != "task.continue" {
		return failResult(r, "paused", "task is paused")
	}
	switch op.Action {
	case "task.read":
		data, err := client.Call(ctx, "thread/read", map[string]any{"threadId": task.ThreadID, "includeTurns": false})
		if err != nil {
			return failResult(r, "native", err.Error())
		}
		requests, err := s.taskRequests(ctx, task.ID)
		if err != nil {
			return failResult(r, "storage", err.Error())
		}
		r.Stage, r.Data = "accepted", mustJSON(map[string]any{"task": task, "native": data, "requests": requests})
		return r
	case "task.history":
		params := map[string]any{"threadId": task.ThreadID, "limit": 20, "itemsView": "summary"}
		if op.Cursor != "" {
			params["cursor"] = op.Cursor
		}
		data, err := client.Call(ctx, "thread/turns/list", params)
		if err != nil {
			return failResult(r, "native", err.Error())
		}
		r.Stage, r.Data = "accepted", data
		return r
	case "goal.get":
		data, err := client.Call(ctx, "thread/goal/get", map[string]string{"threadId": task.ThreadID})
		if err != nil {
			return failResult(r, "native", err.Error())
		}
		r.Stage, r.Data = "accepted", data
		return r
	case "goal.set":
		var goal struct {
			Objective   *string `json:"objective"`
			Status      *string `json:"status"`
			TokenBudget *int64  `json:"tokenBudget"`
		}
		if len(op.Goal) == 0 {
			return failResult(r, "invalid", "goal parameters are required")
		}
		if err := json.Unmarshal(op.Goal, &goal); err != nil {
			return failResult(r, "invalid", err.Error())
		}
		activates := goal.Status == nil || *goal.Status == "active"
		if activates && goal.TokenBudget == nil {
			return failResult(r, "invalid", "activating a goal requires an explicit positive tokenBudget")
		}
		params := map[string]any{"threadId": task.ThreadID, "origin": "automatic"}
		if goal.Objective != nil {
			params["objective"] = *goal.Objective
		}
		if goal.Status != nil {
			params["status"] = *goal.Status
		}
		if goal.TokenBudget != nil {
			if *goal.TokenBudget <= 0 {
				return failResult(r, "invalid", "goal tokenBudget must be positive")
			}
			params["tokenBudget"] = *goal.TokenBudget
		}
		if activates {
			if err := s.claimTask(ctx, task); err != nil {
				return failResult(r, "conflict", err.Error())
			}
		}
		data, err := client.Call(ctx, "thread/goal/set", params)
		if err != nil {
			return failNativeResult(r, err)
		}
		s.mu.Lock()
		latest, err := s.loadTask(context.WithoutCancel(ctx), task.ID)
		s.mu.Unlock()
		if err != nil {
			r.Stage = "unknown"
			return failResult(r, "storage", err.Error())
		}
		if latest.Paused {
			return s.applyLatePause(ctx, op, latest, client)
		}
		r.Stage, r.Data = "accepted", data
		return r
	case "turn.steer":
		if op.ExpectedTurnID == "" || op.ExpectedTurnID != task.TurnID {
			return failResult(r, "stale_turn", "expected turn does not match task's current turn")
		}
		if op.Input == "" {
			return failResult(r, "invalid", "steer input is required")
		}
		if task.Execution != "running" {
			return failResult(r, "stale_turn", "task no longer has the expected active turn")
		}
		data, err := client.Call(ctx, "turn/steer", map[string]any{"threadId": task.ThreadID, "expectedTurnId": op.ExpectedTurnID, "input": []map[string]string{{"type": "text", "text": op.Input}}})
		if err != nil {
			return failNativeResult(r, err)
		}
		r.Stage, r.Data = "accepted", data
		return r
	case "task.continue":
		if op.Input == "" {
			return failResult(r, "invalid", "continuation input is required")
		}
		status, err := readNativeStatus(ctx, client, task.ThreadID)
		if err != nil {
			return failResult(r, "native", err.Error())
		}
		if status != "idle" && status != "notLoaded" {
			return failResult(r, "conflict", "thread is active or its state is unknown; use steer for an active turn")
		}
		goalStatus, err := readNativeGoalStatus(ctx, client, task.ThreadID)
		if err != nil {
			return failResult(r, "native", err.Error())
		}
		if !quietGoal(goalStatus) {
			return failResult(r, "conflict", "native Goal is active or unknown; do not load it or start a competing turn")
		}
		if err := s.claimTask(ctx, task); err != nil {
			return failResult(r, "conflict", err.Error())
		}
		s.mu.Lock()
		p, _ := s.folderLocked(task.FolderID)
		_, cwd, directoryErr := s.taskRootLocked(ctx, task)
		s.mu.Unlock()
		if !p.Available {
			return failResult(r, "folder_unavailable", "registered folder directory is unavailable")
		}
		if directoryErr != nil {
			return failResult(r, "working_directory_unavailable", directoryErr.Error())
		}
		if _, err := client.Call(ctx, "thread/resume", map[string]any{"threadId": task.ThreadID, "cwd": cwd, "approvalPolicy": p.ApprovalPolicy, "sandbox": p.Sandbox, "approvalsReviewer": "user"}); err != nil {
			return failNativeResult(r, err)
		}
		goalStatus, err = readNativeGoalStatus(ctx, client, task.ThreadID)
		if err != nil {
			return failResult(r, "native", err.Error())
		}
		if !quietGoal(goalStatus) {
			return failResult(r, "conflict", "native Goal owns continuation after the thread was loaded")
		}
		status, err = readNativeStatus(ctx, client, task.ThreadID)
		if err != nil {
			return failResult(r, "native", err.Error())
		}
		if status != "idle" {
			return failResult(r, "conflict", "loaded thread has not been proven idle")
		}
		task.Paused, task.Version, task.Execution = false, op.TaskVersion, "starting"
		if err := s.saveTask(ctx, task); err != nil {
			r.Stage = "unknown"
			return failResult(r, "storage", err.Error())
		}
		params := map[string]any{"threadId": task.ThreadID, "input": []map[string]string{{"type": "text", "text": op.Input}}}
		if len(op.OutputSchema) > 0 {
			params["outputSchema"] = op.OutputSchema
		}
		data, err := client.Call(ctx, "turn/start", params)
		if err != nil {
			return s.recordNativeFailure(ctx, r, task.ID, err)
		}
		var accepted struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(data, &accepted); err != nil || accepted.Turn.ID == "" {
			r.Stage = "unknown"
			return failResult(r, "native_response", "turn/start returned no turn id")
		}
		task, err = s.recordAcceptedTurn(ctx, task.ID, accepted.Turn.ID)
		if err != nil {
			r.Stage = "unknown"
			return failResult(r, "storage", err.Error())
		}
		if task.Paused {
			return s.applyLatePause(ctx, op, task, client)
		}
		r.Stage, r.TurnID, r.Data = "accepted", task.TurnID, mustJSON(task)
		return r
	}
	return failResult(r, "unsupported", "action is not supported")
}

func (s *Service) claimTask(ctx context.Context, task protocol.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, exists := s.folderLocked(task.FolderID)
	if !exists || !p.Available {
		return errors.New("registered folder directory is unavailable")
	}
	_, cwd, directoryErr := s.taskRootLocked(ctx, task)
	if directoryErr != nil {
		return directoryErr
	}
	latest, err := s.loadTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if latest.Version != task.Version || latest.Paused != task.Paused {
		return errors.New("task was paused or replaced before its folder could be claimed")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT folder_root FROM executor_tasks WHERE occupied=1 AND task_id<>?`, task.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var other string
		if err := rows.Scan(&other); err != nil {
			rows.Close()
			return err
		}
		if rootsOverlap(cwd, other) {
			rows.Close()
			return errors.New("folder is occupied by another task")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	_, err = s.db.ExecContext(ctx, `UPDATE executor_tasks SET occupied=1 WHERE task_id=?`, task.ID)
	return err
}

func (s *Service) taskRequests(ctx context.Context, id string) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM executor_requests WHERE valid=1 AND json_extract(payload,'$.task_id')=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	requests := []map[string]any{}
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		var request pendingRequest
		if err := json.Unmarshal([]byte(text), &request); err != nil {
			return nil, err
		}
		if strings.TrimSpace(request.InstanceID) == s.instance {
			requests = append(requests, map[string]any{"request_id": request.ID, "instance_id": request.InstanceID, "task_id": request.TaskID, "method": request.Method, "params": request.Params})
		}
	}
	return requests, rows.Err()
}
