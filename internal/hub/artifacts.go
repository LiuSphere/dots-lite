package hub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"dots-duet-lite/internal/protocol"
	"dots-duet-lite/internal/store"
)

func validateUpload(upload *protocol.FileUpload) error {
	if upload == nil || upload.Name == "" || upload.Name == "." || upload.Name == ".." || strings.ContainsAny(upload.Name, "/\\\x00<>:\"|?*") || strings.TrimRight(upload.Name, ". ") != upload.Name {
		return failure("invalid_upload", "upload name must be a plain file basename")
	}
	if upload.Size < 0 || upload.Size > 8<<20 || len(upload.DataBase64) > base64.StdEncoding.EncodedLen(8<<20) {
		return failure("invalid_upload", "upload must not exceed 8 MiB")
	}
	bytes, err := base64.StdEncoding.DecodeString(upload.DataBase64)
	if err != nil {
		return failure("invalid_upload", "upload content must be valid base64")
	}
	sum := sha256.Sum256(bytes)
	if int64(len(bytes)) != upload.Size || hex.EncodeToString(sum[:]) != upload.SHA256 {
		return failure("invalid_upload", "upload size and SHA-256 must match its content")
	}
	return nil
}

type artifact struct {
	ID          string          `json:"id"`
	TaskID      string          `json:"task_id"`
	TaskVersion int64           `json:"task_version"`
	File        json.RawMessage `json:"file"`
	Status      string          `json:"status"`
	LocalPath   string          `json:"local_path,omitempty"`
	Error       string          `json:"error,omitempty"`
	Attempts    int             `json:"attempts"`
	NextCheckAt time.Time       `json:"next_check_at"`
	Sequence    int64           `json:"sequence"`
}
type filePayload struct {
	Path          string          `json:"path"`
	TaskVersion   int64           `json:"task_version"`
	Base64        string          `json:"data_base64"`
	SHA256        string          `json:"sha256"`
	Size          int64           `json:"size"`
	ReturnedBytes int64           `json:"returned_bytes"`
	Offset        int64           `json:"offset"`
	Truncated     bool            `json:"truncated"`
	Error         json.RawMessage `json:"error,omitempty"`
}

func (h *Hub) saveArtifact(tx *store.Tx, b Binding, task *taskRecord, event protocol.Event) error {
	var file filePayload
	if err := json.Unmarshal(event.Data, &file); err != nil {
		return err
	}
	if file.Path == "" || file.TaskVersion < 1 {
		return failure("invalid_artifact", "artifact needs a path and task version")
	}
	a := artifact{ID: "artifact_" + strconv.FormatInt(event.Sequence, 10), TaskID: task.ID, TaskVersion: file.TaskVersion, File: event.Data, Status: "available", Sequence: event.Sequence}
	if event.Type == "artifact/failed" {
		a.Status = "failed"
		a.Error = string(file.Error)
		if file.TaskVersion == task.Version {
			task.Delivery = "failed"
		}
	} else {
		bytes, err := base64.StdEncoding.DecodeString(file.Base64)
		if err != nil {
			return failure("invalid_artifact", "artifact payload is not valid base64")
		}
		sum := sha256.Sum256(bytes)
		if len(bytes) > 8<<20 || int64(len(bytes)) != file.Size || file.ReturnedBytes != file.Size || file.Offset != 0 || file.Truncated || hex.EncodeToString(sum[:]) != file.SHA256 {
			return failure("invalid_artifact", "artifact must contain a complete bounded file with matching hash")
		}
		if file.TaskVersion == task.Version {
			task.Delivery = "pending"
		}
	}
	found := false
	for _, path := range task.Artifacts {
		if path == file.Path {
			found = true
		}
	}
	if !found && file.TaskVersion == task.Version {
		task.Artifacts = append(task.Artifacts, file.Path)
	}
	return tx.Put("artifact", owner(b.scope()), a.ID, a)
}
func (h *Hub) NextArtifact(ctx context.Context, uid int) (json.RawMessage, error) {
	b, err := h.binding(uid)
	if err != nil {
		return nil, err
	}
	var next *artifact
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		rows, err := tx.List("artifact", owner(b.scope()))
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, raw := range rows {
			var a artifact
			if err = json.Unmarshal(raw, &a); err != nil {
				return err
			}
			if a.Status != "available" || now.Before(a.NextCheckAt) {
				continue
			}
			task, err := loadTask(tx, b, a.TaskID)
			if err != nil {
				return err
			}
			if task.Paused || task.Version != a.TaskVersion {
				continue
			}
			if a.Attempts >= 4 {
				a.Status = "failed"
				a.Error = "artifact transfer retry budget exhausted"
				task.Delivery = "failed"
				if err = tx.Put("artifact", owner(b.scope()), a.ID, a); err != nil {
					return err
				}
				if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
					return err
				}
				continue
			}
			a.Attempts++
			a.NextCheckAt = now.Add(15 * time.Second)
			if err = tx.Put("artifact", owner(b.scope()), a.ID, a); err != nil {
				return err
			}
			next = &a
			break
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(next)
}

type acquisition struct {
	Acquired      bool   `json:"acquired"`
	LocalPath     string `json:"local_path,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Size          int64  `json:"size,omitempty"`
	ReturnedBytes int64  `json:"returned_bytes,omitempty"`
	Offset        int64  `json:"offset,omitempty"`
	Error         string `json:"error,omitempty"`
}

func (h *Hub) RecordArtifactAcquisition(ctx context.Context, uid int, id string, raw json.RawMessage) (json.RawMessage, error) {
	b, err := h.binding(uid)
	if err != nil {
		return nil, err
	}
	var receipt acquisition
	if err = decodeArgs(raw, &receipt); err != nil {
		return nil, err
	}
	var a artifact
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.Get("artifact", owner(b.scope()), id, &a); err != nil {
			return err
		}
		var file filePayload
		if err = json.Unmarshal(a.File, &file); err != nil {
			return err
		}
		task, err := loadTask(tx, b, a.TaskID)
		if err != nil {
			return err
		}
		if receipt.Acquired {
			if receipt.LocalPath == "" || receipt.SHA256 != file.SHA256 || receipt.Size != file.Size || receipt.ReturnedBytes != file.Size || receipt.Offset != 0 || file.Truncated {
				return failure("binding_conflict", "acquisition receipt does not match the complete artifact")
			}
			if a.Status == "acquired" {
				if a.LocalPath != receipt.LocalPath {
					return failure("binding_conflict", "artifact already acquired at another path")
				}
				return nil
			}
			a.Status = "acquired"
			a.LocalPath = receipt.LocalPath
			a.Error = ""
		} else {
			if a.Status == "acquired" {
				return failure("binding_conflict", "an acquired artifact cannot be replaced by a failed receipt")
			}
			a.Error = receipt.Error
			a.NextCheckAt = time.Now().UTC().Add(15 * time.Second)
			if task.Version == a.TaskVersion {
				task.Delivery = "failed"
			}
		}
		if err = tx.Put("artifact", owner(b.scope()), id, a); err != nil {
			return err
		}
		if receipt.Acquired && task.Version == a.TaskVersion {
			rows, err := tx.List("artifact", owner(b.scope()))
			if err != nil {
				return err
			}
			latest := map[string]artifact{}
			for _, raw := range rows {
				var current artifact
				if err = json.Unmarshal(raw, &current); err != nil {
					return err
				}
				if current.TaskID != task.ID || current.TaskVersion != task.Version {
					continue
				}
				var info filePayload
				if err = json.Unmarshal(current.File, &info); err != nil {
					return err
				}
				if old, ok := latest[info.Path]; !ok || current.Sequence > old.Sequence {
					latest[info.Path] = current
				}
			}
			all := len(task.Artifacts) > 0
			for _, path := range task.Artifacts {
				if latest[path].Status != "acquired" {
					all = false
				}
			}
			if all {
				task.Delivery = "acquired"
			}
		}
		task.UpdatedAt = time.Now().UTC()
		if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
			return err
		}
		if receipt.Acquired && task.Version == a.TaskVersion && task.Verification == "passed" {
			return queueFinalDeliveries(tx, b, task)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return artifactMetadata(a)
}
func artifactMetadata(a artifact) (json.RawMessage, error) {
	var file map[string]json.RawMessage
	if err := json.Unmarshal(a.File, &file); err != nil {
		return nil, err
	}
	delete(file, "data_base64")
	return json.Marshal(map[string]any{"id": a.ID, "task_id": a.TaskID, "task_version": a.TaskVersion, "status": a.Status, "file": file, "local_path": a.LocalPath, "error": a.Error})
}
func (h *Hub) artifactList(ctx context.Context, b Binding, task taskRecord) (json.RawMessage, error) {
	items := []json.RawMessage{}
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		rows, err := tx.List("artifact", owner(b.scope()))
		if err != nil {
			return err
		}
		for _, raw := range rows {
			var a artifact
			if err = json.Unmarshal(raw, &a); err != nil {
				return err
			}
			if a.TaskID != task.ID {
				continue
			}
			metadata, err := artifactMetadata(a)
			if err != nil {
				return err
			}
			items = append(items, metadata)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"artifacts": task.Artifacts, "items": items, "delivery": task.Delivery})
}
