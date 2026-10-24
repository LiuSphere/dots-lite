package hub

import (
	"io/fs"
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
	return fs.ValidPath(directory) && !strings.ContainsAny(directory, "\\:")
}

type publicTaskView = protocol.Task

func publicTask(task protocol.Task) publicTaskView {
	return task
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
