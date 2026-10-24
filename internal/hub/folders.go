package hub

import (
	"io/fs"
	"strings"

	"dots-lite/internal/protocol"
)

func folderID(project, folder string) (string, error) {
	if project != "" && folder != "" && project != folder {
		return "", failure("invalid_arguments", "folder_id and project_id must identify the same folder")
	}
	if folder != "" {
		return folder, nil
	}
	return project, nil
}

func workingDirectory(directory string) string {
	if directory == "" {
		return "."
	}
	return directory
}

func validWorkingDirectory(directory string) bool {
	return fs.ValidPath(directory) && !strings.ContainsAny(directory, "\\:")
}

type publicTaskView struct {
	protocol.Task
	FolderID string `json:"folder_id"`
}

func publicTask(task protocol.Task) publicTaskView {
	return publicTaskView{Task: task, FolderID: task.ProjectID}
}

type publicSnapshotView struct {
	protocol.Snapshot
	Folders []protocol.Project `json:"folders"`
	Tasks   []publicTaskView   `json:"tasks"`
}

func publicSnapshot(snapshot protocol.Snapshot, binding Binding) publicSnapshotView {
	view := publicSnapshotView{Snapshot: snapshot, Folders: []protocol.Project{}, Tasks: []publicTaskView{}}
	for _, folder := range snapshot.Projects {
		if allowedProject(binding, folder.ID) {
			view.Folders = append(view.Folders, folder)
		}
	}
	view.Projects = view.Folders
	for _, task := range snapshot.Tasks {
		view.Tasks = append(view.Tasks, publicTask(task))
	}
	return view
}
