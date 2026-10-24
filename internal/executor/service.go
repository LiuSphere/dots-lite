package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dots-lite/internal/protocol"
	_ "modernc.org/sqlite"
)

type ServiceConfig struct {
	Scope     protocol.Scope     `json:"scope"`
	Projects  []protocol.Project `json:"projects"`
	StatePath string             `json:"state_path"`
	Native    Options            `json:"-"`
}

type Service struct {
	config           ServiceConfig
	db               *sql.DB
	mu               sync.Mutex
	client           *Client
	instance         string
	roots            map[string]*os.Root
	taskRoots        map[string]*os.Root
	projects         map[string]protocol.Project
	cancel           context.CancelFunc
	pumpDone         chan struct{}
	nativeErr        error
	eventFault       error
	transportErr     error
	starting         bool
	background       sync.WaitGroup
	reconciling      map[string]bool
	reconcilePending map[string]protocol.Task
}

func NewService(config ServiceConfig) (*Service, error) {
	if config.Scope.UserID == "" || config.Scope.ProfileID == "" || config.Scope.ExecutorID == "" || config.StatePath == "" {
		return nil, errors.New("executor scope and state_path are required")
	}
	if err := os.MkdirAll(filepath.Dir(config.StatePath), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", config.StatePath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Service{config: config, db: db, instance: randomID(), roots: map[string]*os.Root{}, taskRoots: map[string]*os.Root{}, projects: map[string]protocol.Project{}, reconciling: map[string]bool{}, reconcilePending: map[string]protocol.Task{}}
	for _, statement := range []string{
		`PRAGMA journal_mode=WAL`, `PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS executor_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS executor_tasks (task_id TEXT PRIMARY KEY, payload TEXT NOT NULL, project_root TEXT NOT NULL, occupied INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS executor_operations (operation_id TEXT PRIMARY KEY, digest TEXT NOT NULL, result TEXT NOT NULL, action TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS executor_events (sequence INTEGER PRIMARY KEY AUTOINCREMENT, payload TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS executor_requests (request_id TEXT PRIMARY KEY, payload TEXT NOT NULL, valid INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	columns, err := db.Query(`PRAGMA table_info(executor_operations)`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	hasAction := false
	for columns.Next() {
		var number, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := columns.Scan(&number, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			columns.Close()
			_ = db.Close()
			return nil, err
		}
		if name == "action" {
			hasAction = true
		}
	}
	if err := columns.Err(); err != nil {
		columns.Close()
		_ = db.Close()
		return nil, err
	}
	columns.Close()
	if !hasAction {
		if _, err := db.Exec(`ALTER TABLE executor_operations ADD COLUMN action TEXT NOT NULL DEFAULT ''`); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	owner, _ := json.Marshal(config.Scope)
	if _, err := db.Exec(`INSERT OR IGNORE INTO executor_meta(key,value) VALUES('scope',?)`, string(owner)); err != nil {
		_ = db.Close()
		return nil, err
	}
	var existing string
	if err := db.QueryRow(`SELECT value FROM executor_meta WHERE key='scope'`).Scan(&existing); err != nil || existing != string(owner) {
		_ = db.Close()
		return nil, errors.New("executor database belongs to a different scope")
	}
	for _, p := range config.Projects {
		if p.ID == "" || !filepath.IsAbs(p.Root) {
			_ = s.Close()
			return nil, errors.New("project id and absolute root are required")
		}
		if _, exists := s.projects[p.ID]; exists {
			_ = s.Close()
			return nil, errors.New("duplicate project id")
		}
		p.Available = false
		if p.ApprovalPolicy == "" {
			p.ApprovalPolicy = "on-request"
		}
		if p.Sandbox == "" {
			p.Sandbox = "workspace-write"
		}
		s.projects[p.ID] = p
		s.projectLocked(p.ID)
	}
	if _, err := db.Exec(`UPDATE executor_requests SET valid=0`); err != nil {
		_ = s.Close()
		return nil, err
	}
	if _, err := db.Exec(`UPDATE executor_tasks SET payload=json_set(payload,'$.execution','unknown','$.waiting','') WHERE json_extract(payload,'$.execution') IN ('starting','running','stopping') OR json_extract(payload,'$.waiting')<>''`); err != nil {
		_ = s.Close()
		return nil, err
	}
	if _, err := db.Exec(`UPDATE executor_operations SET result=json_set(result,'$.stage','unknown') WHERE json_extract(result,'$.stage')='dispatched'`); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := os.Chmod(config.StatePath, 0600); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Service) StartNative(ctx context.Context) error {
	s.mu.Lock()
	if s.eventFault != nil {
		fault := s.eventFault
		s.mu.Unlock()
		return fault
	}
	if s.client != nil || s.starting {
		s.mu.Unlock()
		return errors.New("app-server already started")
	}
	s.starting = true
	s.mu.Unlock()
	childCtx, cancel := context.WithCancel(ctx)
	options := s.config.Native
	options.ExperimentalAPI = true
	if options.StartupTimeout == 0 {
		options.StartupTimeout = 15 * time.Second
	}
	client, err := Start(childCtx, options)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starting = false
	if err != nil {
		s.nativeErr = err
		cancel()
		return err
	}
	s.client, s.cancel, s.instance = client, cancel, randomID()
	s.pumpDone = make(chan struct{})
	s.nativeErr = nil
	go s.pumpNative(client)
	return nil
}

func (s *Service) Execute(ctx context.Context, op protocol.Operation) protocol.Result {
	result := protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "failed"}
	if op.Scope != s.config.Scope {
		return failResult(result, "forbidden", "operation scope does not match executor")
	}
	if op.ID == "" {
		return failResult(result, "invalid", "operation_id is required")
	}
	encoded, err := json.Marshal(op)
	if err != nil {
		return failResult(result, "invalid", err.Error())
	}
	digest := sha256.Sum256(encoded)
	s.mu.Lock()
	var oldDigest, oldResult string
	err = s.db.QueryRowContext(ctx, `SELECT digest,result FROM executor_operations WHERE operation_id=?`, op.ID).Scan(&oldDigest, &oldResult)
	if err == nil {
		s.mu.Unlock()
		if oldDigest != hex.EncodeToString(digest[:]) {
			return failResult(result, "conflict", "operation id already has different arguments")
		}
		if err := json.Unmarshal([]byte(oldResult), &result); err != nil {
			return failResult(result, "storage", err.Error())
		}
		if result.Stage == "dispatched" {
			result.Stage = "unknown"
		}
		return result
	}
	if !errors.Is(err, sql.ErrNoRows) {
		s.mu.Unlock()
		return failResult(result, "storage", err.Error())
	}
	if s.eventFault != nil && isWriteAction(op.Action) && op.Action != "task.stop" {
		fault := s.eventFault
		s.mu.Unlock()
		return failResult(result, "event_storage", fault.Error())
	}
	result.Stage = "dispatched"
	initial, _ := json.Marshal(result)
	_, err = s.db.ExecContext(ctx, `INSERT INTO executor_operations(operation_id,digest,result,action) VALUES(?,?,?,?)`, op.ID, hex.EncodeToString(digest[:]), string(initial), op.Action)
	s.mu.Unlock()
	if err != nil {
		return failResult(result, "storage", err.Error())
	}
	if op.TaskID != "" && isWriteAction(op.Action) && op.Action != "task.stop" {
		var uncertain int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM executor_operations WHERE operation_id<>? AND json_extract(result,'$.task_id')=? AND json_extract(result,'$.stage') IN ('unknown','dispatched') AND (action='' OR action IN ('task.create','task.continue','turn.steer','task.stop','goal.set','approval.respond','question.respond','files.upload'))`, op.ID, op.TaskID).Scan(&uncertain); err != nil {
			return failResult(result, "storage", err.Error())
		}
		if uncertain > 0 {
			result.Stage = "failed"
			result = failResult(result, "conflict", "task has an uncertain operation; query it before submitting another write")
			final := mustJSON(result)
			if _, err := s.db.ExecContext(ctx, `UPDATE executor_operations SET result=? WHERE operation_id=?`, string(final), op.ID); err != nil {
				result.Stage = "unknown"
				return failResult(result, "storage", err.Error())
			}
			return result
		}
	}
	result = s.execute(ctx, op)
	s.mu.Lock()
	if s.eventFault != nil && isWriteAction(op.Action) {
		result.Stage = "unknown"
		result = failResult(result, "event_storage", s.eventFault.Error())
	}
	s.mu.Unlock()
	final, _ := json.Marshal(result)
	if _, err := s.db.ExecContext(context.Background(), `UPDATE executor_operations SET result=? WHERE operation_id=?`, string(final), op.ID); err != nil {
		return failResult(protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "unknown"}, "storage", err.Error())
	}
	return result
}

func (s *Service) execute(ctx context.Context, op protocol.Operation) protocol.Result {
	r := protocol.Result{OperationID: op.ID, TaskID: op.TaskID, Stage: "failed"}
	if op.Action == "operation.read" {
		var data string
		if err := s.db.QueryRowContext(ctx, `SELECT result FROM executor_operations WHERE operation_id=?`, op.RequestID).Scan(&data); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return failResult(r, "not_found", "original operation is not recorded on this executor")
			}
			return failResult(r, "storage", err.Error())
		}
		var original protocol.Result
		if err := json.Unmarshal([]byte(data), &original); err != nil {
			return failResult(r, "storage", err.Error())
		}
		if op.TaskID != "" && original.TaskID != op.TaskID {
			return failResult(r, "forbidden", "original operation belongs to another task")
		}
		if original.Stage == "dispatched" {
			original.Stage = "unknown"
		}
		r.Stage, r.Data = "accepted", mustJSON(original)
		return r
	}
	if op.Action == "machine.status" || op.Action == "projects.list" {
		state, err := s.Snapshot(ctx)
		if err != nil {
			return failResult(r, "storage", err.Error())
		}
		r.Stage, r.Data = "accepted", mustJSON(state)
		return r
	}
	s.mu.Lock()
	p, exists := s.projectLocked(op.ProjectID)
	s.mu.Unlock()
	if !exists {
		return failResult(r, "project_not_found", "project is not registered")
	}
	if !p.Available && op.Action != "task.stop" && op.Action != "task.read" && op.Action != "task.history" && op.Action != "goal.get" {
		return failResult(r, "project_unavailable", "registered project directory is unavailable or no longer matches its authorized open handle")
	}
	if op.TaskID != "" && op.Action != "task.create" {
		s.mu.Lock()
		task, taskErr := s.loadTask(ctx, op.TaskID)
		s.mu.Unlock()
		if taskErr != nil {
			return failResult(r, "task_not_found", taskErr.Error())
		}
		if task.ProjectID != op.ProjectID {
			return failResult(r, "forbidden", "task belongs to a different authorized folder")
		}
		if err := matchingWorkingDirectory(op, task); err != nil {
			return failResult(r, "invalid_working_directory", err.Error())
		}
		if isWriteAction(op.Action) && op.Action != "task.stop" {
			s.mu.Lock()
			_, _, taskErr = s.taskRootLocked(ctx, task)
			s.mu.Unlock()
			if taskErr != nil {
				return failResult(r, "working_directory_unavailable", taskErr.Error())
			}
		}
	}
	if op.Action == "files.list" || op.Action == "files.read" || op.Action == "files.download" {
		return s.readFileOperation(ctx, op)
	}
	if op.Action == "files.upload" {
		return s.uploadFileOperation(ctx, op)
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if op.Action == "task.stop" {
		return s.stopTask(ctx, op, client)
	}
	if client == nil {
		return failResult(r, "not_ready", "app-server is not initialized")
	}
	if op.Action == "approval.respond" || op.Action == "question.respond" {
		return s.respondOperation(ctx, op, client)
	}
	if op.Action == "task.continue" || op.Action == "task.read" || op.Action == "task.history" || op.Action == "turn.steer" || op.Action == "goal.set" || op.Action == "goal.get" {
		return s.taskOperation(ctx, op, client)
	}
	if op.Action != "task.create" {
		return failResult(r, "unsupported", "action is not supported")
	}
	if op.TaskID == "" || op.Input == "" {
		return failResult(r, "invalid", "task_id and input are required")
	}
	if err := validateInputFiles(op.TaskID, op.InputFiles); err != nil {
		r.Error = err
		return r
	}
	directory, directoryErr := workingDirectory(op.WorkingDirectory)
	if directoryErr != nil {
		return failResult(r, "invalid_working_directory", directoryErr.Error())
	}
	s.mu.Lock()
	taskRoot, cwd, directoryErr := s.openTaskRootLocked(op.ProjectID, directory)
	s.mu.Unlock()
	if directoryErr != nil {
		return failResult(r, "working_directory_unavailable", directoryErr.Error())
	}
	defer func() {
		if taskRoot != nil {
			_ = taskRoot.Close()
		}
	}()
	now := time.Now().UTC()
	task := protocol.Task{ID: op.TaskID, Scope: op.Scope, Name: op.Name, Objective: op.Input, ProjectID: op.ProjectID, WorkingDirectory: directory, Version: op.TaskVersion, Execution: "starting", Verification: "pending", Delivery: "pending", Artifacts: op.Artifacts, CreatedAt: now, UpdatedAt: now}
	if task.Version == 0 {
		task.Version = 1
	}
	if err := s.insertTask(ctx, task, cwd); err != nil {
		return failResult(r, "conflict", err.Error())
	}
	s.mu.Lock()
	if s.taskRoots[task.ID] == nil {
		s.taskRoots[task.ID], taskRoot = taskRoot, nil
	}
	s.mu.Unlock()
	input, inputFailure := s.stageInputFiles(ctx, op, task)
	if inputFailure != nil {
		return *inputFailure
	}
	params := map[string]any{"cwd": cwd, "approvalPolicy": p.ApprovalPolicy, "sandbox": p.Sandbox, "approvalsReviewer": "user"}
	if p.Model != "" {
		params["model"] = p.Model
	}
	data, err := client.Call(ctx, "thread/start", params)
	if err != nil {
		return s.recordNativeFailure(ctx, r, task.ID, err)
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(data, &started); err != nil || started.Thread.ID == "" {
		r.Stage = "unknown"
		return failResult(r, "native_response", "thread/start returned no thread id")
	}
	task, err = s.recordCreatedThread(ctx, task.ID, started.Thread.ID)
	r.ThreadID = started.Thread.ID
	if err != nil {
		r.Stage = "unknown"
		return failResult(r, "storage", err.Error())
	}
	if task.Paused || task.Version != op.TaskVersion && op.TaskVersion != 0 {
		r.Data = mustJSON(task)
		return failResult(r, "paused", "task was paused or replaced before turn submission")
	}
	turnParams := map[string]any{"threadId": task.ThreadID, "input": []map[string]string{{"type": "text", "text": input}}}
	if len(op.OutputSchema) > 0 {
		turnParams["outputSchema"] = op.OutputSchema
	}
	data, err = client.Call(ctx, "turn/start", turnParams)
	if err != nil {
		return s.recordNativeFailure(ctx, r, task.ID, err)
	}
	var turn struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(data, &turn); err != nil || turn.Turn.ID == "" {
		r.Stage = "unknown"
		return failResult(r, "native_response", "turn/start returned no turn id")
	}
	task, err = s.recordAcceptedTurn(ctx, task.ID, turn.Turn.ID)
	if err != nil {
		r.Stage = "unknown"
		return failResult(r, "storage", err.Error())
	}
	if task.Paused {
		return s.applyLatePause(ctx, op, task, client)
	}
	r.Stage, r.TurnID, r.Data = "accepted", task.TurnID, mustJSON(task)
	return r
}

func (s *Service) insertTask(ctx context.Context, task protocol.Task, root string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT project_root FROM executor_tasks WHERE occupied=1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var other string
		if err := rows.Scan(&other); err != nil {
			rows.Close()
			return err
		}
		if rootsOverlap(root, other) {
			rows.Close()
			return errors.New("project is occupied by an active or uncertain task")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	_, err = s.db.ExecContext(ctx, `INSERT INTO executor_tasks(task_id,payload,project_root,occupied) VALUES(?,?,?,1)`, task.ID, string(mustJSON(task)), root)
	return err
}
func rootsOverlap(a, b string) bool { return pathWithin(a, b) || pathWithin(b, a) }
func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func (s *Service) saveTask(ctx context.Context, task protocol.Task) error {
	return saveTaskWith(ctx, s.db, task)
}

type taskStateWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func saveTaskWith(ctx context.Context, writer taskStateWriter, task protocol.Task) error {
	task.UpdatedAt = time.Now().UTC()
	result, err := writer.ExecContext(ctx, `UPDATE executor_tasks SET payload=? WHERE task_id=? AND json_extract(payload,'$.version')<=? AND (json_extract(payload,'$.paused')=0 OR ?=1 OR json_extract(payload,'$.version')<?)`, string(mustJSON(task)), task.ID, task.Version, task.Paused, task.Version)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("task changed or paused before state update")
	}
	return nil
}
func (s *Service) recordAcceptedTurn(ctx context.Context, taskID, turnID string) (protocol.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, err := s.loadTask(ctx, taskID)
	if err != nil {
		return task, err
	}
	terminal := task.TurnID == turnID && (task.Execution == "completed" || task.Execution == "interrupted" || task.Execution == "failed")
	task.TurnID = turnID
	if !terminal && !task.Paused {
		task.Execution = "running"
	}
	if s.client == nil || s.client.Err() != nil {
		task.Execution = "unknown"
	}
	return task, s.saveTask(ctx, task)
}
func (s *Service) recordCreatedThread(ctx context.Context, taskID, threadID string) (protocol.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, err := s.loadTask(ctx, taskID)
	if err != nil {
		return task, err
	}
	task.ThreadID = threadID
	if task.Paused {
		task.Execution = "stopped"
	}
	if err := s.saveTask(ctx, task); err != nil {
		return task, err
	}
	if task.Paused {
		_, err = s.db.ExecContext(ctx, `UPDATE executor_tasks SET occupied=0 WHERE task_id=?`, task.ID)
	}
	return task, err
}
func (s *Service) loadTask(ctx context.Context, id string) (protocol.Task, error) {
	var task protocol.Task
	var data string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM executor_tasks WHERE task_id=?`, id).Scan(&data); err != nil {
		return task, err
	}
	return task, json.Unmarshal([]byte(data), &task)
}
func (s *Service) Snapshot(ctx context.Context) (protocol.Snapshot, error) {
	s.mu.Lock()
	native, instance := s.client, s.instance
	s.mu.Unlock()
	state := protocol.Snapshot{InstanceID: instance, Initialized: native != nil && native.Err() == nil, Projects: []protocol.Project{}, Tasks: []protocol.Task{}}
	s.mu.Lock()
	if s.nativeErr != nil {
		state.Error = s.nativeErr.Error()
	}
	if s.eventFault != nil {
		state.Initialized = false
	}
	for id := range s.projects {
		p, _ := s.projectLocked(id)
		state.Projects = append(state.Projects, p)
	}
	s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM executor_tasks ORDER BY task_id`)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return state, err
		}
		var task protocol.Task
		if err := json.Unmarshal([]byte(text), &task); err != nil {
			return state, err
		}
		state.Tasks = append(state.Tasks, task)
	}
	return state, rows.Err()
}

// An open root stays bound to the directory first authorized by registration.
// If its path is replaced, keep the handle and report unavailability rather
// than silently authorizing the replacement directory.
func (s *Service) projectLocked(id string) (protocol.Project, bool) {
	p, exists := s.projects[id]
	if !exists {
		return p, false
	}
	p.Available = false
	root := s.roots[id]
	if root == nil {
		path, err := filepath.EvalSymlinks(p.Root)
		if err != nil {
			return p, true
		}
		root, err = os.OpenRoot(path)
		if err != nil {
			return p, true
		}
		s.roots[id] = root
	}
	opened, openErr := root.Stat(".")
	registered, pathErr := os.Stat(p.Root)
	if openErr != nil || pathErr != nil || !os.SameFile(opened, registered) {
		return p, true
	}
	p.Root, p.Available = root.Name(), true
	return p, true
}
func (s *Service) Close() error {
	var err error
	if s.client != nil {
		err = s.client.Close()
		if s.cancel != nil {
			s.cancel()
		}
		if s.pumpDone != nil {
			<-s.pumpDone
		}
	}
	s.background.Wait()
	for _, root := range s.roots {
		err = errors.Join(err, root.Close())
	}
	for _, root := range s.taskRoots {
		err = errors.Join(err, root.Close())
	}
	if s.db != nil {
		err = errors.Join(err, s.db.Close())
	}
	return err
}
func failResult(r protocol.Result, code, message string) protocol.Result {
	r.Error = &protocol.Error{Code: code, Message: message}
	return r
}
func isWriteAction(action string) bool {
	return action == "task.create" || action == "task.continue" || action == "turn.steer" || action == "task.stop" || action == "goal.set" || action == "approval.respond" || action == "question.respond" || action == "files.upload"
}
func failNativeResult(r protocol.Result, err error) protocol.Result {
	var rpc *RPCError
	r.Stage = "unknown"
	if errors.As(err, &rpc) {
		r.Stage = "failed"
	}
	return failResult(r, "native", err.Error())
}
func (s *Service) recordNativeFailure(ctx context.Context, r protocol.Result, taskID string, nativeErr error) protocol.Result {
	r = failNativeResult(r, nativeErr)
	s.mu.Lock()
	defer s.mu.Unlock()
	task, err := s.loadTask(context.Background(), taskID)
	if err == nil {
		if !task.Paused {
			if r.Stage == "failed" {
				task.Execution = "failed"
			} else {
				task.Execution = "unknown"
			}
		}
		err = s.saveTask(context.Background(), task)
		if err == nil && r.Stage == "failed" && !task.Paused {
			_, err = s.db.Exec(`UPDATE executor_tasks SET occupied=0 WHERE task_id=?`, taskID)
		}
		r.Data = mustJSON(task)
	}
	if err != nil {
		r.Stage = "unknown"
		return failResult(r, "storage", errors.Join(nativeErr, err).Error())
	}
	return r
}
func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
func randomID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}
