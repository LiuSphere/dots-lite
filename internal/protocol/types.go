// Package protocol defines the versioned business messages between the hub and
// an executor. It is separate from the native Codex JSON-RPC protocol.
package protocol

import (
	"encoding/json"
	"time"
)

// Version 4 binds writes to maintenance epochs and adds native session access.
const Version = 4

// Scope is supplied by authenticated configuration, never by a model argument.
type Scope struct {
	UserID     string `json:"user_id"`
	ProfileID  string `json:"profile_id"`
	ExecutorID string `json:"executor_id"`
}

type Folder struct {
	ID             string `json:"id"`
	Root           string `json:"root,omitempty"`
	Description    string `json:"description,omitempty"`
	Model          string `json:"model,omitempty"`
	ApprovalPolicy string `json:"approval_policy,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
	Available      bool   `json:"available"`
}

type Source struct {
	Kind         string `json:"kind"`
	Platform     string `json:"platform"`
	SenderID     string `json:"sender_id"`
	ChatID       string `json:"chat_id"`
	ThreadID     string `json:"thread_id,omitempty"`
	SessionID    string `json:"session_id"`
	MessageID    string `json:"message_id,omitempty"`
	NativeTaskID string `json:"native_task_id,omitempty"`
	ToolCallID   string `json:"tool_call_id,omitempty"`
}

type Invocation struct {
	Tool               string                 `json:"tool"`
	Arguments          json.RawMessage        `json:"arguments"`
	Source             Source                 `json:"source"`
	AttachmentFiles    []InvocationAttachment `json:"attachment_files,omitempty"`
	DirectorySelection *DirectorySelection    `json:"directory_selection,omitempty"`
}

// Supplied privately by the source plugin, never as model tool arguments.
type DirectorySelection struct {
	Path      string `json:"path,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	MessageID string `json:"message_id"`
}

type Maintenance struct {
	RemoteSyncPending bool   `json:"remote_sync_pending,omitempty"`
	Epoch             int64  `json:"epoch"`
	Phase             string `json:"phase"`
	Error             string `json:"error,omitempty"`
}

type ToolRequest struct {
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	ContextRef string          `json:"context_ref"`
}

// FileUpload is an immutable input attachment. The executor chooses its private
// task-relative location and refuses to replace an existing file.
type FileUpload struct {
	Name       string `json:"name"`
	DataBase64 string `json:"data_base64"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
}

// InvocationAttachment is sent privately by the native source plugin. Its bytes
// are never inserted into model arguments, responses, or tool execution logs.
type InvocationAttachment struct {
	LocalPath string     `json:"local_path"`
	File      FileUpload `json:"file"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Operation is persisted before dispatch. Reusing ID cannot change its payload.
type Operation struct {
	Foreground       bool            `json:"foreground"`
	MaintenanceEpoch int64           `json:"maintenance_epoch"`
	Archived         string          `json:"archived,omitempty"`
	ID               string          `json:"operation_id"`
	TaskID           string          `json:"task_id"`
	Scope            Scope           `json:"scope"`
	TaskVersion      int64           `json:"task_version"`
	FolderID         string          `json:"folder_id"`
	WorkingDirectory string          `json:"working_directory,omitempty"`
	Action           string          `json:"action"`
	ThreadID         string          `json:"thread_id,omitempty"`
	ExpectedTurnID   string          `json:"expected_turn_id,omitempty"`
	Input            string          `json:"input,omitempty"`
	Name             string          `json:"name,omitempty"`
	RequestID        string          `json:"request_id,omitempty"`
	InstanceID       string          `json:"instance_id,omitempty"`
	Reply            json.RawMessage `json:"reply,omitempty"`
	Goal             json.RawMessage `json:"goal,omitempty"`
	OutputSchema     json.RawMessage `json:"output_schema,omitempty"`
	Path             string          `json:"path,omitempty"`
	Cursor           string          `json:"cursor,omitempty"`
	Offset           int64           `json:"offset,omitempty"`
	Length           int64           `json:"length,omitempty"`
	Artifacts        []string        `json:"artifacts,omitempty"`
	Upload           *FileUpload     `json:"upload,omitempty"`
	InputFiles       []FileUpload    `json:"input_files,omitempty"`
}

type Result struct {
	OperationID string          `json:"operation_id"`
	TaskID      string          `json:"task_id"`
	Stage       string          `json:"stage"`
	ThreadID    string          `json:"thread_id,omitempty"`
	TurnID      string          `json:"turn_id,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
	Error       *Error          `json:"error,omitempty"`
}

type Task struct {
	DirectoryMode    string          `json:"directory_mode,omitempty"`
	NativePolicy     json.RawMessage `json:"native_policy,omitempty"`
	ModelProvider    string          `json:"model_provider,omitempty"`
	Model            string          `json:"model,omitempty"`
	ID               string          `json:"task_id"`
	Scope            Scope           `json:"scope"`
	Name             string          `json:"name"`
	Objective        string          `json:"objective"`
	FolderID         string          `json:"folder_id"`
	WorkingDirectory string          `json:"working_directory,omitempty"`
	ThreadID         string          `json:"thread_id,omitempty"`
	TurnID           string          `json:"turn_id,omitempty"`
	Version          int64           `json:"version"`
	Paused           bool            `json:"paused"`
	Execution        string          `json:"execution"`
	Waiting          string          `json:"waiting,omitempty"`
	Verification     string          `json:"verification"`
	Delivery         string          `json:"delivery"`
	Evidence         json.RawMessage `json:"evidence,omitempty"`
	Artifacts        []string        `json:"artifacts,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// Event.Sequence is allocated by the executor and acknowledged after hub commit.
type Event struct {
	Sequence   int64           `json:"sequence"`
	TaskID     string          `json:"task_id"`
	InstanceID string          `json:"instance_id"`
	ThreadID   string          `json:"thread_id,omitempty"`
	TurnID     string          `json:"turn_id,omitempty"`
	Type       string          `json:"type"`
	Data       json.RawMessage `json:"data"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Snapshot struct {
	Maintenance Maintenance `json:"maintenance"`
	InstanceID  string      `json:"instance_id"`
	Initialized bool        `json:"initialized"`
	Folders     []Folder    `json:"folders"`
	Tasks       []Task      `json:"tasks"`
	Error       string      `json:"error,omitempty"`
}

// Frame is the application WebSocket envelope; only its matching body is set.
type Frame struct {
	Version     int        `json:"version"`
	Type        string     `json:"type"`
	ID          string     `json:"id,omitempty"`
	Operation   *Operation `json:"operation,omitempty"`
	Result      *Result    `json:"result,omitempty"`
	Event       *Event     `json:"event,omitempty"`
	Snapshot    *Snapshot  `json:"snapshot,omitempty"`
	AckSequence int64      `json:"ack_sequence,omitempty"`
	Error       *Error     `json:"error,omitempty"`
}
