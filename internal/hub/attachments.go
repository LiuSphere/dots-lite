package hub

import (
	"encoding/json"
	"strings"

	"dots-lite/internal/protocol"
)

type inputReference struct {
	Name      string `json:"name"`
	LocalPath string `json:"local_path"`
}

func uploadReference(raw json.RawMessage) (*inputReference, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, failure("invalid_arguments", "upload must be an object")
	}
	if fields["local_path"] == nil {
		return nil, nil
	}
	var ref inputReference
	if err := decodeArgs(raw, &ref); err != nil {
		return nil, err
	}
	return &ref, nil
}
func attachmentReferences(raw json.RawMessage) ([]inputReference, error) {
	var args struct {
		Action string           `json:"action"`
		Inputs []inputReference `json:"inputs"`
		Upload json.RawMessage  `json:"upload"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, failure("invalid_arguments", "attachment arguments must be an object")
	}
	refs := args.Inputs
	if len(refs) > 0 && args.Action != "task.create" {
		return nil, failure("invalid_arguments", "inputs are supported only for task.create")
	}
	upload, err := uploadReference(args.Upload)
	if err != nil {
		return nil, err
	}
	if upload != nil {
		if args.Action != "files.upload" {
			return nil, failure("invalid_arguments", "upload references are supported only for files.upload")
		}
		refs = append(refs, *upload)
	}
	names := map[string]bool{}
	for _, ref := range refs {
		if ref.Name == "" || ref.LocalPath == "" || strings.ContainsRune(ref.LocalPath, '\x00') || names[ref.Name] {
			return nil, failure("invalid_attachment", "each attachment needs a distinct name and exact nonempty local path")
		}
		names[ref.Name] = true
	}
	return refs, nil
}
func bindInvocationAttachments(in protocol.Invocation) ([]protocol.InvocationAttachment, error) {
	if in.Tool != "dots_lite_prepare" {
		if len(in.AttachmentFiles) > 0 {
			return nil, failure("invalid_attachment", "private attachments require a gateway prepare invocation")
		}
		return nil, nil
	}
	refs, err := attachmentReferences(in.Arguments)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 && len(in.AttachmentFiles) == 0 {
		return nil, nil
	}
	if in.Source.Kind != "gateway" {
		return nil, failure("invalid_attachment", "private attachments require a trusted gateway invocation")
	}
	if len(refs) != len(in.AttachmentFiles) {
		return nil, failure("invalid_attachment", "private files must match every exact input reference")
	}
	provided := map[inputReference]protocol.FileUpload{}
	var total int64
	for _, file := range in.AttachmentFiles {
		if err = validateUpload(&file.File); err != nil {
			return nil, err
		}
		ref := inputReference{Name: file.File.Name, LocalPath: file.LocalPath}
		if _, exists := provided[ref]; exists {
			return nil, failure("invalid_attachment", "duplicate private attachment")
		}
		provided[ref] = file.File
		total += file.File.Size
		if total > 8<<20 {
			return nil, failure("invalid_attachment", "combined input attachments exceed 8 MiB")
		}
	}
	files := make([]protocol.InvocationAttachment, 0, len(refs))
	for _, ref := range refs {
		file, ok := provided[ref]
		if !ok {
			return nil, failure("invalid_attachment", "private file name and path do not match the invocation reference")
		}
		files = append(files, protocol.InvocationAttachment{LocalPath: ref.LocalPath, File: file})
	}
	return files, nil
}
func resolvePrepareFiles(args prepareArgs, inv *invocation) (*protocol.FileUpload, []protocol.FileUpload, error) {
	if len(args.Inputs) > 0 && args.Action != "task.create" {
		return nil, nil, failure("invalid_arguments", "inputs are supported only for task.create")
	}
	if len(args.Upload) > 0 && args.Action != "files.upload" {
		return nil, nil, failure("invalid_arguments", "upload is supported only for files.upload")
	}
	lookup := func(ref inputReference) (protocol.FileUpload, error) {
		for _, private := range inv.AttachmentFiles {
			if private.LocalPath == ref.LocalPath && private.File.Name == ref.Name {
				return private.File, nil
			}
		}
		return protocol.FileUpload{}, failure("invalid_attachment", "input reference has no privately bound file")
	}
	var inputs []protocol.FileUpload
	for _, ref := range args.Inputs {
		file, err := lookup(ref)
		if err != nil {
			return nil, nil, err
		}
		inputs = append(inputs, file)
	}
	if args.Action != "files.upload" {
		return nil, inputs, nil
	}
	ref, err := uploadReference(args.Upload)
	if err != nil {
		return nil, nil, err
	}
	if ref != nil {
		file, err := lookup(*ref)
		return &file, nil, err
	}
	var upload protocol.FileUpload
	if err = decodeArgs(args.Upload, &upload); err != nil {
		return nil, nil, err
	}
	if err = validateUpload(&upload); err != nil {
		return nil, nil, err
	}
	return &upload, nil, nil
}
