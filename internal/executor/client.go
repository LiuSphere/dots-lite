package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options selects the app-server process. An empty Command uses Codex on PATH.
type Options struct {
	Command         []string
	Stderr          io.Writer
	ExperimentalAPI bool
	StartupTimeout  time.Duration
}

// Message is an app-server notification or a request awaiting an explicit response.
type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// RPCError preserves the server's structured error response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("app-server RPC error %d: %s", e.Code, e.Message)
}

// Client owns one app-server process and its stdio connection.
type Client struct {
	cmd        *exec.Cmd
	process    *processControl
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	decoder    *json.Decoder
	wait       chan error
	done       chan struct{}
	readDone   chan struct{}
	writeDone  chan struct{}
	mu         sync.Mutex
	writes     chan writeRequest
	pending    map[string]chan response
	requests   map[string]struct{}
	events     chan Message
	err        error
	nextID     int64
	close      sync.Once
	closeErr   error
	cleanupErr error
}

type response struct {
	result json.RawMessage
	err    error
}

type writeRequest struct {
	ctx         context.Context
	data        []byte
	responseKey string
	beforeWrite func() error
	result      chan error
}

type wireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// Start initializes a new app-server instance. ctx bounds its lifetime.
func Start(ctx context.Context, opts Options) (*Client, error) {
	command := opts.Command
	if len(command) == 0 {
		command = []string{"codex", "app-server", "--listen", "stdio://"}
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stderr = opts.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("app-server stdout: %w", err)
	}
	control, err := prepareProcess(cmd)
	if err != nil {
		return nil, errors.Join(err, closePipe(stdin), closePipe(stdout))
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start app-server: %w", errors.Join(err, control.close(), closePipe(stdin), closePipe(stdout)))
	}
	if err := control.started(cmd); err != nil {
		killErr := cmd.Process.Kill()
		if errors.Is(killErr, os.ErrProcessDone) {
			killErr = nil
		}
		return nil, errors.Join(err, killErr, control.close(), closePipe(stdin), closePipe(stdout), cmd.Wait())
	}
	c := &Client{cmd: cmd, process: control, stdin: stdin, stdout: stdout, decoder: json.NewDecoder(stdout), wait: make(chan error, 1), done: make(chan struct{}), readDone: make(chan struct{}), writeDone: make(chan struct{}), writes: make(chan writeRequest), pending: make(map[string]chan response), requests: make(map[string]struct{}), events: make(chan Message, 128)}
	go c.readResponses()
	go c.writeMessages()
	go func() {
		<-c.readDone
		c.wait <- cmd.Wait()
	}()
	go func() {
		select {
		case <-ctx.Done():
			c.abort(ctx.Err())
		case <-c.done:
		}
	}()
	initializeCtx := ctx
	if opts.StartupTimeout > 0 {
		var cancel context.CancelFunc
		initializeCtx, cancel = context.WithTimeout(ctx, opts.StartupTimeout)
		defer cancel()
	}
	initializeParams := map[string]any{"clientInfo": map[string]string{"name": "dots_lite", "version": "0.1.0"}}
	if opts.ExperimentalAPI {
		initializeParams["capabilities"] = map[string]bool{"experimentalApi": true}
	}
	_, err = c.Call(initializeCtx, "initialize", initializeParams)
	if err == nil {
		err = c.write(initializeCtx, map[string]any{"method": "initialized", "params": map[string]any{}}, "")
	}
	if err != nil {
		return nil, fmt.Errorf("initialize app-server: %w", errors.Join(err, c.Close()))
	}
	return c, nil
}

// Call sends a request without retrying it.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	key := "n:" + strconv.FormatInt(id, 10)
	result := make(chan response, 1)
	c.pending[key] = result
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()
	err := c.write(ctx, map[string]any{"id": id, "method": method, "params": params}, "")
	if err != nil {
		return nil, fmt.Errorf("write app-server request: %w", err)
	}
	select {
	case response := <-result:
		return response.result, response.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		select {
		case response := <-result:
			return response.result, response.err
		default:
			return nil, c.Err()
		}
	}
}

func (c *Client) readResponses() {
	defer close(c.readDone)
	defer close(c.events)
	for {
		var message wireMessage
		if err := c.decoder.Decode(&message); err != nil {
			c.fail(fmt.Errorf("read app-server response: %w", err))
			return
		}
		if message.Method != "" {
			if message.Method == "serverRequest/resolved" {
				var resolved struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				if err := json.Unmarshal(message.Params, &resolved); err != nil {
					c.fail(fmt.Errorf("decode resolved app-server request: %w", err))
					return
				}
				key, err := requestKey(resolved.RequestID)
				if err != nil {
					c.fail(err)
					return
				}
				c.mu.Lock()
				delete(c.requests, key)
				c.mu.Unlock()
			}
			if len(message.ID) > 0 {
				key, err := requestKey(message.ID)
				if err != nil {
					c.fail(err)
					return
				}
				c.mu.Lock()
				c.requests[key] = struct{}{}
				c.mu.Unlock()
			}
			select {
			case c.events <- Message{ID: message.ID, Method: message.Method, Params: message.Params}:
				continue
			default:
				c.fail(errors.New("app-server event queue is full"))
				_ = c.stdin.Close()
				return
			}
		}
		key, err := requestKey(message.ID)
		if err != nil {
			c.fail(err)
			return
		}
		c.mu.Lock()
		result := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if result != nil {
			var responseErr error
			if message.Error != nil {
				responseErr = message.Error
			}
			result <- response{result: message.Result, err: responseErr}
		}
	}
}

// Events returns server requests and notifications in receive order.
// Callers must drain it; overflow terminates the connection with an error.
func (c *Client) Events() <-chan Message { return c.events }

// Respond answers one pending server request. It never decides approvals itself.
func (c *Client) Respond(ctx context.Context, id json.RawMessage, result any) error {
	return c.respondIf(ctx, id, result, nil)
}
func (c *Client) respondIf(ctx context.Context, id json.RawMessage, result any, beforeWrite func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode app-server response: %w", err)
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	key, err := requestKey(id)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if _, ok := c.requests[key]; !ok {
		c.mu.Unlock()
		return errors.New("app-server request is unknown or already answered")
	}
	c.mu.Unlock()
	err = c.writeChecked(ctx, map[string]any{"id": id, "result": json.RawMessage(encoded)}, key, beforeWrite)
	if err != nil {
		return fmt.Errorf("write app-server response: %w", err)
	}
	return nil
}

func (c *Client) write(ctx context.Context, message any, responseKey string) error {
	return c.writeChecked(ctx, message, responseKey, nil)
}
func (c *Client) writeChecked(ctx context.Context, message any, responseKey string, beforeWrite func() error) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	request := writeRequest{ctx: ctx, data: append(data, '\n'), responseKey: responseKey, beforeWrite: beforeWrite, result: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	case c.writes <- request:
	}
	select {
	case err := <-request.result:
		return err
	case <-ctx.Done():
		// A partial frame may already be on the pipe. End this connection rather
		// than send a later request into an incomplete JSON message.
		c.abort(ctx.Err())
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}

func (c *Client) writeMessages() {
	defer close(c.writeDone)
	for {
		select {
		case <-c.done:
			return
		case request := <-c.writes:
			if err := request.ctx.Err(); err != nil {
				request.result <- err
				continue
			}
			if request.beforeWrite != nil {
				if err := request.beforeWrite(); err != nil {
					request.result <- err
					continue
				}
			}
			if request.responseKey != "" {
				c.mu.Lock()
				_, pending := c.requests[request.responseKey]
				if pending {
					delete(c.requests, request.responseKey)
				}
				c.mu.Unlock()
				if !pending {
					request.result <- errors.New("app-server request is unknown or already answered")
					continue
				}
			}
			_, err := c.stdin.Write(request.data)
			request.result <- err
			if err != nil {
				c.fail(fmt.Errorf("write app-server message: %w", err))
				return
			}
		}
	}
}

func (c *Client) abort(err error) {
	c.fail(err)
	cleanupErr := errors.Join(c.process.kill(c.cmd), closePipe(c.stdin), closePipe(c.stdout))
	if cleanupErr != nil {
		c.mu.Lock()
		c.err = errors.Join(c.err, cleanupErr)
		c.mu.Unlock()
	}
}

func closePipe(pipe io.Closer) error {
	err := pipe.Close()
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

func requestKey(id json.RawMessage) (string, error) {
	id = json.RawMessage(strings.TrimSpace(string(id)))
	if len(id) == 0 || string(id) == "null" {
		return "", errors.New("app-server request ID must be a string or integer")
	}
	if id[0] == '"' {
		var value string
		if err := json.Unmarshal(id, &value); err != nil {
			return "", errors.New("invalid app-server string request ID")
		}
		return "s:" + value, nil
	}
	var value int64
	if err := json.Unmarshal(id, &value); err != nil {
		return "", errors.New("app-server request ID must be a string or integer")
	}
	return "n:" + strconv.FormatInt(value, 10), nil
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
}

// Done closes when the stdio connection is no longer usable.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the stdio connection ended.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close closes stdin and waits briefly before terminating the owned process.
func (c *Client) Close() error {
	c.close.Do(func() {
		c.fail(errors.New("app-server client closed"))
		stdinErr := closePipe(c.stdin)
		select {
		case c.closeErr = <-c.wait:
		case <-time.After(time.Second):
			c.cleanupErr = errors.Join(errors.New("app-server did not exit after stdin closed; forced termination required"), c.process.kill(c.cmd), closePipe(c.stdout))
			c.closeErr = <-c.wait
		}
		<-c.writeDone
		c.cleanupErr = errors.Join(c.cleanupErr, stdinErr, c.process.close())
		c.closeErr = errors.Join(c.closeErr, c.cleanupErr)
	})
	return c.closeErr
}

// A failed process may be replaced only after its owned resources were closed.
// Its exit status remains observable through Close and Err.
func (c *Client) closeForRecovery() error {
	_ = c.Close()
	if c.cmd.ProcessState == nil || !c.cmd.ProcessState.Exited() {
		return errors.Join(c.cleanupErr, errors.New("app-server exit was not confirmed"))
	}
	return c.cleanupErr
}
