package hub

import (
	"io/fs"
	"regexp"
	"strings"

	"dots-lite/internal/protocol"
)

func workingDirectory(directory string) string {
	if directory == "" {
		return "."
	}
	return directory
}

func validWorkingDirectory(directory string) bool {
	return absoluteDirectory(directory) || (fs.ValidPath(directory) && !strings.ContainsAny(directory, "\\:"))
}

// The hub runs on Linux and must also recognize the executor's Windows paths.
var drivePath = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

func absoluteDirectory(path string) bool {
	return !strings.ContainsRune(path, '\x00') && (strings.HasPrefix(path, "/") || drivePath.MatchString(path))
}

type publicTaskView struct {
	protocol.Task
	FollowupStatistics map[string]any `json:"followup_statistics"`
}

func publicTask(task taskRecord) publicTaskView {
	return publicTaskView{Task: task.Task, FollowupStatistics: map[string]any{"worker_offers_including_legacy": task.RecoveryUsed, "polls_without_run_id_including_legacy": task.RecoveryAttempts, "registered_submission_attempts_since_v4": task.SubmissionAttempts, "native_runs_accepted_since_v4": task.RunsAccepted, "native_runs_ended_since_v4": task.RunsCompleted, "model_calls": nil}}
}

func publicSnapshot(snapshot protocol.Snapshot, binding Binding) protocol.Snapshot {
	view := snapshot
	view.Folders = []protocol.Folder{}
	view.Tasks = append([]protocol.Task{}, snapshot.Tasks...)
	for _, folder := range snapshot.Folders {
		if allowedFolder(binding, folder.ID) {
			view.Folders = append(view.Folders, folder)
		}
	}
	return view
}
