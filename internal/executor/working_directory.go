package executor

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"dots-lite/internal/protocol"
)

func workingDirectory(name string) (string, error) {
	if name == "" {
		return ".", nil
	}
	if filepath.IsAbs(name) && !strings.ContainsRune(name, '\x00') {
		return filepath.Clean(name), nil
	}
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") {
		return "", errors.New("working_directory must be a relative slash-separated directory inside the authorized folder")
	}
	return name, nil
}

func openDirectoryRoot(directory, path string) (*os.Root, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	current, currentErr := os.Stat(directory)
	if err != nil || currentErr != nil || !os.SameFile(opened, current) {
		return nil, errors.Join(errors.New("directory changed while opening"), err, currentErr, root.Close())
	}
	return root, nil
}

// The authorized root, not a string prefix, resolves the selected directory.
// Its physical name is checked against the same opened directory before it is
// used for native APIs that can accept only a path.
func (s *Service) openTaskRootLocked(folderID, directory string) (*os.Root, string, error) {
	if filepath.IsAbs(directory) {
		path, err := filepath.EvalSymlinks(directory)
		if err != nil {
			return nil, "", err
		}
		if s.config.DirectoryMode == "allowlist" {
			p, ok := s.folderLocked(folderID)
			if !ok || !p.Available || !pathWithin(p.Root, path) {
				return nil, "", errors.New("directory is outside the authorized allowlist")
			}
		}
		root, err := openDirectoryRoot(directory, path)
		if err != nil {
			return nil, "", err
		}
		return root, path, nil
	}
	p, exists := s.folderLocked(folderID)
	if !exists || !p.Available {
		return nil, "", errors.New("authorized folder is unavailable")
	}
	relative, err := workingDirectory(directory)
	if err != nil {
		return nil, "", err
	}
	root, err := s.roots[folderID].OpenRoot(filepath.FromSlash(relative))
	if err != nil {
		return nil, "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(p.Root, filepath.FromSlash(relative)))
	if err == nil && !pathWithin(p.Root, path) {
		err = errors.New("working directory escapes the authorized folder")
	}
	if err == nil {
		opened, openErr := root.Stat(".")
		current, pathErr := os.Stat(path)
		if openErr != nil || pathErr != nil {
			err = errors.Join(openErr, pathErr)
		} else if !os.SameFile(opened, current) {
			err = errors.New("working directory changed while it was opened")
		}
	}
	if err != nil {
		return nil, "", errors.Join(err, root.Close())
	}
	return root, path, nil
}

// A restored task may reopen only its persisted absolute directory. While the
// service lives, the original handle also rejects replacement at that path.
func (s *Service) taskRootLocked(ctx context.Context, task protocol.Task) (*os.Root, string, error) {
	var expected string
	if err := s.db.QueryRowContext(ctx, `SELECT folder_root FROM executor_tasks WHERE task_id=?`, task.ID).Scan(&expected); err != nil {
		return nil, "", err
	}
	opened, path, err := s.openTaskRootLocked(task.FolderID, task.WorkingDirectory)
	if err != nil {
		return nil, "", err
	}
	return s.verifyTaskRootLocked(ctx, task, opened, path, expected)
}

// Stopping an existing thread must remain possible after its grant changes.
// This opens only the persisted directory for native stop inspection; business
// writes and file operations must still use taskRootLocked and the allowlist.
func (s *Service) taskRootForStopLocked(ctx context.Context, task protocol.Task) (*os.Root, string, error) {
	var expected string
	if err := s.db.QueryRowContext(ctx, `SELECT folder_root FROM executor_tasks WHERE task_id=?`, task.ID).Scan(&expected); err != nil {
		return nil, "", err
	}
	path, err := filepath.EvalSymlinks(expected)
	if err != nil {
		return nil, "", err
	}
	opened, err := openDirectoryRoot(expected, path)
	if err != nil {
		return nil, "", err
	}
	return s.verifyTaskRootLocked(ctx, task, opened, path, expected)
}

func (s *Service) verifyTaskRootLocked(ctx context.Context, task protocol.Task, opened *os.Root, path, expected string) (*os.Root, string, error) {
	identity, err := directoryIdentity(opened)
	if err != nil {
		return nil, "", errors.Join(err, opened.Close())
	}
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM executor_meta WHERE key=?`, "task_root_identity:"+task.ID).Scan(&stored); err != nil || identity != stored {
		if errors.Is(err, sql.ErrNoRows) {
			err = errors.New("legacy directory identity is unverified; offline migration is required")
		}
		return nil, "", errors.Join(errors.New("task directory identity changed or is unavailable"), err, opened.Close())
	}
	relative, compareErr := filepath.Rel(expected, path)
	if compareErr != nil || relative != "." {
		return nil, "", errors.Join(errors.New("task working directory no longer matches its stored path"), opened.Close())
	}
	if existing := s.taskRoots[task.ID]; existing != nil {
		oldInfo, oldErr := existing.Stat(".")
		newInfo, newErr := opened.Stat(".")
		closeErr := opened.Close()
		if oldErr != nil || newErr != nil || closeErr != nil {
			return nil, "", errors.Join(oldErr, newErr, closeErr)
		}
		if !os.SameFile(oldInfo, newInfo) {
			return nil, "", errors.New("task working directory was replaced")
		}
		return existing, expected, nil
	}
	s.taskRoots[task.ID] = opened
	return opened, expected, nil
}

func matchingWorkingDirectory(op protocol.Operation, task protocol.Task) error {
	if op.WorkingDirectory == "" {
		return nil
	}
	want, err := workingDirectory(op.WorkingDirectory)
	if err != nil {
		return err
	}
	existing, err := workingDirectory(task.WorkingDirectory)
	if err != nil {
		return err
	}
	if want != existing {
		return errors.New("task working_directory cannot be changed")
	}
	return nil
}
