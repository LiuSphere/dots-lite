package hub

import (
	"context"
	"encoding/json"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
)

func (h *Hub) forwardQuery(ctx context.Context, b Binding, a queryArgs) (json.RawMessage, error) {
	actions := map[string]string{"history": "task.history", "requests": "task.read", "session": "task.read", "goal": "goal.get", "directory": "files.list", "text": "files.read", "download": "files.download"}
	if a.Query == "projects" || a.Query == "folders" {
		h.mu.Lock()
		s := h.sessions[b.ExecutorID]
		h.mu.Unlock()
		if s == nil {
			return nil, failure("offline", "bound executor is offline")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		projects := []protocol.Project{}
		for _, p := range s.snapshot.Projects {
			if allowedProject(b, p.ID) {
				projects = append(projects, p)
			}
		}
		return json.Marshal(projects)
	}
	var task taskRecord
	if a.Query == "directory" && a.TaskID == "" {
		if a.FolderID == "" {
			return nil, failure("invalid_arguments", "directory browsing without a task requires folder_id")
		}
		if !allowedProject(b, a.ProjectID) {
			return nil, failure("forbidden", "folder is not authorized")
		}
		task.ProjectID = a.ProjectID
	} else {
		err := h.db.Update(ctx, func(tx *store.Tx) error { var err error; task, err = loadTask(tx, b, a.TaskID); return err })
		if err != nil {
			return nil, err
		}
		if a.ProjectID != "" && a.ProjectID != task.ProjectID {
			return nil, failure("forbidden", "task folder cannot be changed")
		}
	}
	if a.Query == "artifacts" {
		return h.artifactList(ctx, b, task)
	}
	if a.Query == "deliveries" {
		return h.deliveryList(ctx, b, task)
	}
	action, ok := actions[a.Query]
	if !ok {
		return nil, failure("invalid_query", "query is unavailable")
	}
	id, err := randomID("read_")
	if err != nil {
		return nil, err
	}
	result, err := h.dispatch(ctx, b, protocol.Operation{ID: id, TaskID: task.ID, Scope: b.scope(), TaskVersion: task.Version, ProjectID: task.ProjectID, WorkingDirectory: task.WorkingDirectory, ThreadID: task.ThreadID, Action: action, Path: a.Path, Offset: a.Offset, Length: a.Length, Cursor: a.Cursor})
	if err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, result.Error
	}
	if result.Stage == "failed" || result.Stage == "unknown" {
		return nil, failure(result.Stage, "executor could not complete query")
	}
	if len(result.Data) == 0 {
		return nil, failure("invalid_result", "executor returned no query data")
	}
	if a.Query == "requests" {
		var state struct {
			Requests json.RawMessage `json:"requests"`
		}
		if err = json.Unmarshal(result.Data, &state); err != nil {
			return nil, err
		}
		if state.Requests == nil {
			return json.RawMessage(`[]`), nil
		}
		return state.Requests, nil
	}
	return result.Data, nil
}
