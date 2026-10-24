package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"dots-lite/internal/protocol"
)

func (s *Service) respondOperation(ctx context.Context, op protocol.Operation, client *Client) protocol.Result {
	r := protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "failed"}
	request, err := s.validRequest(ctx, op)
	if err != nil {
		return failResult(r, "request_invalid", err.Error())
	}
	if op.Action == "approval.respond" {
		if request.Method != "item/commandExecution/requestApproval" && request.Method != "item/fileChange/requestApproval" {
			return failResult(r, "unsupported_request", "this request needs a structured native reply, not a boolean approval")
		}
		var reply struct {
			Decision string `json:"decision"`
		}
		decoder := json.NewDecoder(bytes.NewReader(op.Reply))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&reply); err != nil {
			return failResult(r, "invalid_reply", err.Error())
		}
		if reply.Decision != "accept" && reply.Decision != "decline" && reply.Decision != "cancel" {
			return failResult(r, "invalid_reply", "only one-time accept, decline, or cancel is supported")
		}
		var params struct {
			AvailableDecisions []json.RawMessage `json:"availableDecisions"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return failResult(r, "invalid_request", err.Error())
		}
		if len(params.AvailableDecisions) > 0 {
			available := false
			for _, raw := range params.AvailableDecisions {
				var decision string
				if json.Unmarshal(raw, &decision) == nil && decision == reply.Decision {
					available = true
				}
			}
			if !available {
				return failResult(r, "invalid_reply", "decision is not offered by the original request")
			}
		}
	} else {
		if request.Method != "tool/requestUserInput" {
			return failResult(r, "unsupported_request", "original request is not a technical question")
		}
		var reply struct {
			Answers map[string]struct {
				Answers []string `json:"answers"`
			} `json:"answers"`
		}
		decoder := json.NewDecoder(bytes.NewReader(op.Reply))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&reply); err != nil {
			return failResult(r, "invalid_reply", err.Error())
		}
		var params struct {
			Questions []struct {
				ID string `json:"id"`
			} `json:"questions"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return failResult(r, "invalid_request", err.Error())
		}
		if len(params.Questions) == 0 || len(reply.Answers) != len(params.Questions) {
			return failResult(r, "invalid_reply", "answers must match every original question")
		}
		for _, question := range params.Questions {
			if len(reply.Answers[question.ID].Answers) == 0 {
				return failResult(r, "invalid_reply", "an original question is unanswered")
			}
		}
	}
	check := func() error { _, err := s.validRequest(ctx, op); return err }
	if err := client.respondIf(ctx, request.NativeID, op.Reply, check); err != nil {
		r.Stage = "unknown"
		return failResult(r, "native", err.Error())
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE executor_requests SET valid=0 WHERE request_id=?`, request.ID); err != nil {
		r.Stage = "unknown"
		return failResult(r, "storage", err.Error())
	}
	r.Stage, r.ThreadID, r.TurnID, r.Data = "accepted", request.ThreadID, request.TurnID, mustJSON(map[string]any{"request_id": request.ID, "submitted": true, "execution_verified": false})
	return r
}

func (s *Service) validRequest(ctx context.Context, op protocol.Operation) (pendingRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var request pendingRequest
	if op.InstanceID == "" || op.InstanceID != s.instance {
		return request, errors.New("request belongs to a previous app-server instance")
	}
	var text string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM executor_requests WHERE request_id=? AND valid=1`, op.RequestID).Scan(&text); err != nil {
		return request, errors.New("request is no longer pending")
	}
	if err := json.Unmarshal([]byte(text), &request); err != nil {
		return request, err
	}
	if request.InstanceID != s.instance || request.TaskID != op.TaskID {
		return request, errors.New("request target does not match operation")
	}
	task, err := s.loadTask(ctx, op.TaskID)
	if err != nil {
		return request, err
	}
	if task.ProjectID != op.ProjectID || task.ThreadID != request.ThreadID || task.Paused || task.Version != op.TaskVersion {
		return request, fmt.Errorf("task was paused, replaced, or moved to a different version")
	}
	if request.TurnID != "" && task.TurnID != request.TurnID {
		return request, errors.New("request turn is no longer current")
	}
	return request, nil
}
