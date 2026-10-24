package hub

import "dots-duet-lite/internal/protocol"

type publicFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type publicOperationView struct {
	operationRecord
	Upload     *publicFile  `json:"upload,omitempty"`
	InputFiles []publicFile `json:"input_files,omitempty"`
}

// Only model-facing responses use this view. Stored operations and transport
// messages retain their immutable input bytes.
func publicOperation(op operationRecord) publicOperationView {
	view := publicOperationView{operationRecord: op}
	metadata := func(file protocol.FileUpload) publicFile {
		return publicFile{Name: file.Name, SHA256: file.SHA256, Size: file.Size}
	}
	if op.Upload != nil {
		file := metadata(*op.Upload)
		view.Upload = &file
	}
	for _, file := range op.InputFiles {
		view.InputFiles = append(view.InputFiles, metadata(file))
	}
	return view
}
