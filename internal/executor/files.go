package executor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"dots-lite/internal/protocol"
)

const maxFileBytes = 8 << 20

type inputFileInfo struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func validateFileUpload(upload protocol.FileUpload) ([]byte, *protocol.Error) {
	if !attachmentName(upload.Name) {
		return nil, &protocol.Error{Code: "invalid_upload", Message: "attachment requires a plain basename"}
	}
	if upload.Size < 0 || upload.Size > maxFileBytes || len(upload.DataBase64) > base64.StdEncoding.EncodedLen(maxFileBytes) {
		return nil, &protocol.Error{Code: "file_too_large", Message: "attachment exceeds the 8 MiB transfer limit"}
	}
	data, err := base64.StdEncoding.Strict().DecodeString(upload.DataBase64)
	if err != nil {
		return nil, &protocol.Error{Code: "invalid_upload", Message: "attachment data is not valid base64"}
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != upload.Size || !strings.EqualFold(hex.EncodeToString(sum[:]), upload.SHA256) {
		return nil, &protocol.Error{Code: "integrity_mismatch", Message: "attachment bytes do not match the declared size and SHA-256"}
	}
	return data, nil
}

func validateInputFiles(taskID string, files []protocol.FileUpload) *protocol.Error {
	if len(files) == 0 {
		return nil
	}
	if !attachmentName(taskID) {
		return &protocol.Error{Code: "invalid_upload", Message: "input attachments require a task with a plain identifier"}
	}
	var total int64
	names := map[string]bool{}
	for _, file := range files {
		if _, err := validateFileUpload(file); err != nil {
			return err
		}
		name := file.Name
		if runtime.GOOS == "windows" {
			name = strings.ToLower(name)
		}
		if names[name] {
			return &protocol.Error{Code: "invalid_upload", Message: "input attachment basenames must be unique"}
		}
		names[name] = true
		total += file.Size
		if total > maxFileBytes {
			return &protocol.Error{Code: "file_too_large", Message: "input attachments exceed the combined 8 MiB transfer limit"}
		}
	}
	return nil
}

func (s *Service) stageInputFiles(ctx context.Context, op protocol.Operation, task protocol.Task) (string, *protocol.Result) {
	staged := make([]inputFileInfo, 0, len(op.InputFiles))
	for _, file := range op.InputFiles {
		upload := op
		upload.Action, upload.TaskVersion, upload.Upload = "files.upload", task.Version, &file
		result := s.uploadFileOperation(ctx, upload)
		if result.Stage != "accepted" {
			result = s.recordInputFailure(op.TaskID, result, staged)
			return "", &result
		}
		var info inputFileInfo
		if err := json.Unmarshal(result.Data, &info); err != nil {
			result.Stage = "unknown"
			result = failResult(result, "file_metadata", err.Error())
			result = s.recordInputFailure(op.TaskID, result, staged)
			return "", &result
		}
		staged = append(staged, info)
	}
	if len(staged) == 0 {
		return op.Input, nil
	}
	return op.Input + "\n\nInput files (paths are relative to the folder workspace):\n" + string(mustJSON(staged)), nil
}

func (s *Service) recordInputFailure(taskID string, result protocol.Result, staged []inputFileInfo) protocol.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	task, err := s.loadTask(ctx, taskID)
	if err == nil {
		if !task.Paused {
			if result.Stage == "failed" {
				task.Execution = "failed"
			} else {
				task.Execution = "unknown"
			}
		}
		err = s.saveTask(ctx, task)
		if err == nil && result.Stage == "failed" {
			_, err = s.db.ExecContext(ctx, `UPDATE executor_tasks SET occupied=0 WHERE task_id=?`, taskID)
		}
	}
	result.Data = mustJSON(struct {
		protocol.Task
		InputFiles []inputFileInfo `json:"input_files"`
	}{task, staged})
	if err != nil {
		result.Stage = "unknown"
		return failResult(result, "storage", errors.Join(result.Error, err).Error())
	}
	return result
}

func (s *Service) uploadFileOperation(ctx context.Context, op protocol.Operation) (r protocol.Result) {
	r = protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "failed"}
	if op.Upload == nil || !attachmentName(op.TaskID) {
		return failResult(r, "invalid_upload", "upload requires an existing task and a plain attachment basename")
	}
	upload := op.Upload
	data, uploadErr := validateFileUpload(*upload)
	if uploadErr != nil {
		r.Error = uploadErr
		return r
	}
	sum := sha256.Sum256(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	folder, exists := s.folderLocked(op.FolderID)
	if !exists || !folder.Available {
		return failResult(r, "folder_unavailable", "registered folder directory is unavailable")
	}
	task, err := s.loadTask(ctx, op.TaskID)
	if err != nil {
		return failResult(r, "task_not_found", err.Error())
	}
	if task.FolderID != op.FolderID {
		return failResult(r, "forbidden", "attachment folder differs from the task folder")
	}
	if task.Version != op.TaskVersion || task.Paused {
		return failResult(r, "stale_version", "attachment task was paused or changed version")
	}
	root, cwd, err := s.taskRootLocked(ctx, task)
	if err != nil {
		return failResult(r, "working_directory_unavailable", err.Error())
	}
	rows, err := s.db.QueryContext(ctx, `SELECT folder_root FROM executor_tasks WHERE occupied=1 AND task_id<>?`, task.ID)
	if err != nil {
		return failResult(r, "storage", err.Error())
	}
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			rows.Close()
			return failResult(r, "storage", err.Error())
		}
		if rootsOverlap(root, cwd) {
			rows.Close()
			return failResult(r, "conflict", "attachment folder is occupied by another task")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return failResult(r, "storage", err.Error())
	}
	if err := ctx.Err(); err != nil {
		return failResult(r, "canceled", err.Error())
	}
	dir := filepath.Join(".dots-lite", "attachments", task.ID)
	if err := root.MkdirAll(dir, 0700); err != nil {
		return failResult(r, "path_denied", err.Error())
	}
	path := filepath.Join(dir, upload.Name)
	f, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		code := "path_denied"
		if errors.Is(err, fs.ErrExist) {
			code = "file_exists"
		}
		return failResult(r, code, err.Error())
	}
	defer func() {
		if err := f.Close(); err != nil {
			r.Stage = "unknown"
			r = failResult(r, "file_close", err.Error())
		}
	}()
	r.Stage = "unknown"
	if _, err := f.Write(data); err != nil {
		return failResult(r, "file_write", err.Error())
	}
	if err := f.Sync(); err != nil {
		return failResult(r, "file_write", err.Error())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return failResult(r, "file_read", err.Error())
	}
	actual, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return failResult(r, "file_read", err.Error())
	}
	actualSum := sha256.Sum256(actual)
	if len(actual) != len(data) || actualSum != sum {
		return failResult(r, "integrity_mismatch", "attachment changed while its stored bytes were being verified")
	}
	r.Stage, r.Data = "accepted", mustJSON(map[string]any{"path": filepath.ToSlash(path), "size": len(actual), "sha256": hex.EncodeToString(actualSum[:]), "task_id": task.ID, "task_version": task.Version})
	return r
}

func attachmentName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.IsLocal(name) && !strings.ContainsAny(name, `/\\:`)
}

// Native fs/readFile accepts a path, not an already-authorized open handle.
// Root.Open supplies the missing atomic traversal restriction for transfer.
func (s *Service) readFileOperation(ctx context.Context, op protocol.Operation) protocol.Result {
	s.mu.Lock()
	p, exists := s.folderLocked(op.FolderID)
	root := s.roots[op.FolderID]
	var taskErr error
	if exists && p.Available && op.TaskID != "" {
		var task protocol.Task
		task, taskErr = s.loadTask(ctx, op.TaskID)
		if taskErr == nil && task.FolderID != op.FolderID {
			s.mu.Unlock()
			return failResult(protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "failed"}, "forbidden", "file folder differs from task folder")
		}
		if taskErr == nil {
			root, _, taskErr = s.taskRootLocked(ctx, task)
		}
	}
	s.mu.Unlock()
	if !exists || !p.Available {
		return failResult(protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "failed"}, "folder_unavailable", "registered folder directory is unavailable")
	}
	if taskErr != nil {
		return failResult(protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "failed"}, "working_directory_unavailable", taskErr.Error())
	}
	return s.readFileAtRoot(ctx, op, root)
}

func (s *Service) readFileAtRoot(ctx context.Context, op protocol.Operation, root *os.Root) protocol.Result {
	r := protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "failed"}
	if err := ctx.Err(); err != nil {
		return failResult(r, "canceled", err.Error())
	}
	if op.TaskID != "" {
		task, err := s.loadTask(ctx, op.TaskID)
		if err != nil {
			return failResult(r, "task_not_found", err.Error())
		}
		if task.FolderID != op.FolderID {
			return failResult(r, "forbidden", "file folder differs from task folder")
		}
	}
	name := op.Path
	if name == "" && op.Action == "files.list" {
		name = "."
	}
	if !filepath.IsLocal(name) {
		return failResult(r, "path_denied", "file path must remain inside the registered folder")
	}
	f, err := root.Open(name)
	if err != nil {
		code := "path_denied"
		if errors.Is(err, fs.ErrNotExist) {
			code = "not_found"
		}
		if errors.Is(err, fs.ErrPermission) {
			code = "permission_denied"
		}
		return failResult(r, code, err.Error())
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return failResult(r, "file_metadata", err.Error())
	}
	if op.Action == "files.list" {
		if !before.IsDir() {
			return failResult(r, "not_directory", "requested path is not a directory")
		}
		entries, err := f.ReadDir(1001)
		if err != nil && !errors.Is(err, io.EOF) {
			return failResult(r, "file_list", err.Error())
		}
		type entry struct {
			Name        string `json:"name"`
			IsDirectory bool   `json:"is_directory"`
			IsFile      bool   `json:"is_file"`
			IsSymlink   bool   `json:"is_symlink"`
		}
		out := make([]entry, 0, len(entries))
		truncated := len(entries) > 1000
		if truncated {
			entries = entries[:1000]
		}
		for _, e := range entries {
			out = append(out, entry{e.Name(), e.IsDir(), e.Type().IsRegular(), e.Type()&fs.ModeSymlink != 0})
		}
		r.Stage, r.Data = "accepted", mustJSON(map[string]any{"path": name, "entries": out, "truncated": truncated})
		return r
	}
	if !before.Mode().IsRegular() {
		return failResult(r, "not_regular_file", "only regular files may be transferred")
	}
	if before.Size() > maxFileBytes {
		return failResult(r, "file_too_large", "file exceeds 8 MiB transfer limit")
	}
	if op.Offset < 0 || op.Length < 0 || op.Offset > before.Size() {
		return failResult(r, "invalid_range", "offset and length must describe a valid byte range")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return failResult(r, "file_read", err.Error())
	}
	if len(data) > maxFileBytes {
		return failResult(r, "file_too_large", "file grew beyond the transfer limit")
	}
	after, err := f.Stat()
	if err != nil {
		return failResult(r, "file_metadata", err.Error())
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(data)) != after.Size() {
		return failResult(r, "file_changed", "file changed during transfer; fetch it again")
	}
	end := int64(len(data))
	if op.Length > 0 && op.Length < end-op.Offset {
		end = op.Offset + op.Length
	}
	chunk := data[op.Offset:end]
	sum := sha256.Sum256(chunk)
	out := map[string]any{"path": name, "size": int64(len(data)), "offset": op.Offset, "returned_bytes": len(chunk), "truncated": op.Offset > 0 || end < int64(len(data)), "sha256": hex.EncodeToString(sum[:]), "modified_at": after.ModTime().UTC()}
	if op.Action == "files.read" {
		if !utf8.Valid(chunk) {
			return failResult(r, "not_utf8", "requested byte range is not valid UTF-8; use files.download")
		}
		out["text"] = string(chunk)
	} else {
		out["data_base64"] = base64.StdEncoding.EncodeToString(chunk)
	}
	r.Stage, r.Data = "accepted", mustJSON(out)
	return r
}
