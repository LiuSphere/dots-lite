package executor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"dots-duet-lite/internal/executor"
)

func TestStartInitializesBeforeCalling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "handshake"},
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	result, err := client.Call(ctx, "example/echo", map[string]string{"text": "hello"})
	if err != nil {
		t.Fatalf("call after initialization: %v", err)
	}
	if string(result) != `{"text":"hello"}` {
		t.Fatalf("result = %s, want hello response", result)
	}
}

func TestConcurrentCallsReceiveTheirOwnOutOfOrderResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "concurrent"},
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	var calls sync.WaitGroup
	for _, text := range []string{"alpha", "beta"} {
		calls.Go(func() {
			result, err := client.Call(ctx, "example/echo", map[string]string{"text": text})
			if err != nil {
				t.Errorf("%s call: %v", text, err)
				return
			}
			var response struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(result, &response); err != nil {
				t.Errorf("decode %s call: %v", text, err)
			} else if response.Text != text {
				t.Errorf("%s call received %q response", text, response.Text)
			}
		})
	}
	calls.Wait()
}

func TestServerRequestsRemainSeparateFromCallResults(t *testing.T) {
	for _, scenario := range []string{"request-number", "request-string"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := executor.Start(ctx, executor.Options{
				Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", scenario},
				Stderr:  io.Discard,
			})
			if err != nil {
				t.Fatalf("start client: %v", err)
			}
			t.Cleanup(func() { _ = client.Close() })
			if err := client.Respond(ctx, json.RawMessage(`"unknown"`), map[string]string{"decision": "accept"}); err == nil {
				t.Fatal("response to unknown request was accepted")
			}
			callResult := make(chan error, 1)
			go func() {
				result, err := client.Call(ctx, "example/needsApproval", nil)
				if err == nil && string(result) != `{"status":"rejected"}` {
					err = fmt.Errorf("call result = %s", result)
				}
				callResult <- err
			}()
			var request executor.Message
			select {
			case request = <-client.Events():
			case <-ctx.Done():
				t.Fatal("did not receive server request")
			}
			if request.Method != "item/commandExecution/requestApproval" || len(request.ID) == 0 {
				t.Fatalf("server request = %+v", request)
			}
			select {
			case notification := <-client.Events():
				if notification.Method != "item/started" || len(notification.ID) != 0 {
					t.Fatalf("notification = %+v", notification)
				}
			case <-ctx.Done():
				t.Fatal("did not receive progress notification")
			}
			if err := client.Respond(ctx, request.ID, map[string]string{"decision": "decline"}); err != nil {
				t.Fatalf("answer approval: %v", err)
			}
			if err := client.Respond(ctx, request.ID, map[string]string{"decision": "accept"}); err == nil {
				t.Fatal("same approval could be answered twice")
			}
			if err := <-callResult; err != nil {
				t.Fatalf("call result: %v", err)
			}
		})
	}
}

func TestResolvedServerRequestCannotBeAnswered(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "resolved"},
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	var request executor.Message
	for _, expected := range []string{"item/commandExecution/requestApproval", "serverRequest/resolved"} {
		select {
		case event := <-client.Events():
			if event.Method != expected {
				t.Fatalf("event method = %q, want %q", event.Method, expected)
			}
			if expected == "item/commandExecution/requestApproval" {
				request = event
			}
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", expected, ctx.Err())
		}
	}
	if err := client.Respond(ctx, request.ID, map[string]string{"decision": "accept"}); err == nil {
		t.Fatal("resolved approval could still be answered")
	}
	result, err := client.Call(ctx, "example/echo", nil)
	if err != nil {
		t.Fatalf("call after expired approval: %v", err)
	}
	if string(result) != `{"text":"hello"}` {
		t.Fatalf("result = %s", result)
	}
}

func TestRPCErrorRemainsStructuredAndDoesNotCloseClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "rpc-error"},
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_, err = client.Call(ctx, "example/fails", nil)
	var rpcErr *executor.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want structured RPC error", err)
	}
	if rpcErr.Code != -32602 || rpcErr.Message != "bad input" || string(rpcErr.Data) != `{"field":"text"}` {
		t.Fatalf("RPC error = %+v", rpcErr)
	}
	result, err := client.Call(ctx, "example/echo", nil)
	if err != nil {
		t.Fatalf("call after RPC error: %v", err)
	}
	if string(result) != `{"text":"hello"}` {
		t.Fatalf("result = %s", result)
	}
}

func TestTimedOutCallDoesNotRetryOrConsumeTheNextCallsResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "timeout"},
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	callCtx, cancelCall := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelCall()
	_, err = client.Call(callCtx, "example/slow", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow call error = %v, want deadline exceeded", err)
	}
	result, err := client.Call(ctx, "example/afterTimeout", nil)
	if err != nil {
		t.Fatalf("call after timeout: %v", err)
	}
	if string(result) != `{"status":"next"}` {
		t.Fatalf("next result = %s", result)
	}
}

func TestCallDeadlineAlsoInterruptsBlockedPipeWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "blocked-write"},
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	callCtx, cancelCall := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelCall()
	result := make(chan error, 1)
	go func() {
		_, err := client.Call(callCtx, "example/large", map[string]string{"text": strings.Repeat("x", 1<<20)})
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked write error = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call deadline did not interrupt blocked pipe write")
	}
	closed := make(chan struct{})
	go func() { _ = client.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked after timed-out pipe write")
	}
}

func TestStartReturnsInitializationRejection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "init-error"},
		Stderr:  io.Discard,
	})
	if client != nil {
		_ = client.Close()
	}
	var rpcErr *executor.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32600 || rpcErr.Message != "unsupported initialization" {
		t.Fatalf("initialization error = %v, want RPC rejection", err)
	}
}

func TestStartPreservesInitializationAndCleanupFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "init-error-unresponsive"},
		Stderr:  io.Discard,
	})
	if client != nil {
		_ = client.Close()
	}
	var rpcErr *executor.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32600 {
		t.Fatalf("initialization error was lost: %v", err)
	}
	if !strings.Contains(err.Error(), "forced termination required") {
		t.Fatalf("initialization cleanup failure was lost: %v", err)
	}
}

func TestCanceledQueuedResponseLeavesRequestAvailable(t *testing.T) {
	ctx, client, request, control, largeResult := startBlockedApprovalPeer(t)
	answerCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := client.Respond(answerCtx, request.ID, map[string]string{"decision": "decline"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued response cancellation = %v", err)
	}
	if _, err := control.Write([]byte("u")); err != nil {
		t.Fatal(err)
	}
	if err := <-largeResult; err != nil {
		t.Fatal(err)
	}
	if err := client.Respond(ctx, request.ID, map[string]string{"decision": "decline"}); err != nil {
		t.Fatalf("unsent canceled response consumed the pending request: %v", err)
	}
	if _, err := client.Call(ctx, "example/check", nil); err != nil {
		t.Fatalf("peer did not receive the valid response: %v", err)
	}
}

func TestResolvedQueuedResponseIsNotSent(t *testing.T) {
	ctx, client, request, control, largeResult := startBlockedApprovalPeer(t)
	answerCtx := &observedCancellationContext{Context: ctx, observed: make(chan struct{})}
	answerResult := make(chan error, 1)
	go func() {
		answerResult <- client.Respond(answerCtx, request.ID, map[string]string{"decision": "accept"})
	}()
	select {
	case <-answerCtx.observed:
	case <-ctx.Done():
		t.Fatal("response did not begin waiting with cancellation support")
	}
	if _, err := control.Write([]byte("r")); err != nil {
		t.Fatal(err)
	}
	nextPeerEvent(t, ctx, client, "serverRequest/resolved")
	if _, err := control.Write([]byte("u")); err != nil {
		t.Fatal(err)
	}
	if err := <-largeResult; err != nil {
		t.Fatal(err)
	}
	if err := <-answerResult; err == nil {
		t.Error("queued response succeeded after the server resolved its request")
	}
	// The peer expects this call next. Any stale approval frame makes it fail.
	if _, err := client.Call(ctx, "example/check", nil); err != nil {
		t.Fatalf("peer observed a stale response after resolution: %v", err)
	}
}

type observedCancellationContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedCancellationContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func startBlockedApprovalPeer(t *testing.T) (context.Context, *executor.Client, executor.Message, net.Conn, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = listener.SetDeadline(time.Now().Add(5 * time.Second))
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "queued-response", listener.Addr().String()},
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	control, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close() })
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	request := nextPeerEvent(t, ctx, client, "item/commandExecution/requestApproval")
	largeResult := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, "example/large", map[string]string{"text": strings.Repeat("x", 8<<20)})
		largeResult <- err
	}()
	// The peer confirms that it read the first byte, then waits on control.
	// The remaining frame cannot fit in the process pipe while it waits.
	nextPeerEvent(t, ctx, client, "example/writeStarted")
	return ctx, client, request, control, largeResult
}

func nextPeerEvent(t *testing.T, ctx context.Context, client *executor.Client, method string) executor.Message {
	t.Helper()
	select {
	case event := <-client.Events():
		if event.Method != method {
			t.Fatalf("event = %q, want %q", event.Method, method)
		}
		return event
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", method, ctx.Err())
		return executor.Message{}
	}
}

func TestProcessExitFailsOutstandingCallAndSignalsDone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "exit-with-pending"},
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_, err = client.Call(ctx, "example/pending", nil)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call error = %v, want process failure before deadline", err)
	}
	select {
	case <-client.Done():
		if client.Err() == nil {
			t.Fatal("process exit did not retain an error")
		}
	case <-ctx.Done():
		t.Fatal("process exit did not signal Done")
	}
}

func TestCloseReapsUnresponsiveProcessAndRejectsFurtherCalls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "blocked-write"},
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	closed := make(chan error, 1)
	go func() {
		first, second := client.Close(), client.Close()
		if first == nil || second == nil {
			closed <- fmt.Errorf("forced process termination was hidden: first=%v, second=%v", first, second)
			return
		}
		if first.Error() != second.Error() {
			closed <- fmt.Errorf("repeated Close changed result: first=%v, second=%v", first, second)
			return
		}
		closed <- nil
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not reap unresponsive process")
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("Close returned before Done was signaled")
	}
	if _, err := client.Call(ctx, "example/echo", nil); err == nil {
		t.Fatal("call after Close was accepted")
	}
}

func TestUnconsumedEventsFailExplicitlyInsteadOfDisappearing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestPeerProcess$", "--", "event-overflow"},
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_, err = client.Call(ctx, "example/flood", nil)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unconsumed event overflow error = %v, want explicit connection failure", err)
	}
	select {
	case <-client.Done():
		if client.Err() == nil {
			t.Fatal("event overflow did not retain an error")
		}
	case <-ctx.Done():
		t.Fatal("event overflow did not close client")
	}
}

// TestPeerProcess speaks the external app-server protocol over real process pipes.
func TestPeerProcess(t *testing.T) {
	var scenario string
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			scenario = os.Args[i+1]
			break
		}
	}
	if scenario == "" {
		return
	}
	if err := runPeer(scenario); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

type peerFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

func runPeer(scenario string) error {
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	var frame peerFrame
	if err := decoder.Decode(&frame); err != nil {
		return err
	}
	if frame.Method != "initialize" || len(frame.ID) == 0 {
		return fmt.Errorf("first message must be initialize request")
	}
	if scenario == "init-error" || scenario == "init-error-unresponsive" {
		if err := encoder.Encode(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32600, "message": "unsupported initialization"}}); err != nil {
			return err
		}
		if scenario == "init-error-unresponsive" {
			time.Sleep(30 * time.Second)
		}
		return nil
	}
	if err := encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]string{"userAgent": "test-peer/1"}}); err != nil {
		return err
	}
	frame = peerFrame{}
	if err := decoder.Decode(&frame); err != nil {
		return err
	}
	if frame.Method != "initialized" || len(frame.ID) != 0 {
		return fmt.Errorf("initialize response must be followed by initialized notification")
	}
	if scenario == "queued-response" {
		return runQueuedApprovalPeer(decoder, encoder)
	}
	if scenario == "blocked-write" {
		time.Sleep(30 * time.Second)
		return nil
	}
	if scenario == "exit-with-pending" {
		var pending peerFrame
		if err := decoder.Decode(&pending); err != nil {
			return err
		}
		return fmt.Errorf("test peer exited with request pending")
	}
	if scenario == "event-overflow" {
		var call peerFrame
		if err := decoder.Decode(&call); err != nil {
			return err
		}
		if call.Method != "example/flood" {
			return fmt.Errorf("unexpected method %q", call.Method)
		}
		for i := 0; i < 4096; i++ {
			if err := encoder.Encode(map[string]any{"method": "item/started", "params": map[string]int{"sequence": i}}); err != nil {
				return err
			}
		}
		if err := encoder.Encode(map[string]any{"id": call.ID, "result": json.RawMessage(`{"status":"flooded"}`)}); err != nil {
			return err
		}
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	}
	if scenario == "resolved" {
		if err := encoder.Encode(map[string]any{"id": "expired-1", "method": "item/commandExecution/requestApproval", "params": map[string]string{"threadId": "thread-1", "turnId": "turn-1"}}); err != nil {
			return err
		}
		if err := encoder.Encode(map[string]any{"method": "serverRequest/resolved", "params": json.RawMessage(`{"threadId":"thread-1","requestId":"expir\u0065d-1"}`)}); err != nil {
			return err
		}
		scenario = "handshake"
	}
	if scenario == "rpc-error" {
		var call peerFrame
		if err := decoder.Decode(&call); err != nil {
			return err
		}
		if call.Method != "example/fails" {
			return fmt.Errorf("expected failed call, got %q", call.Method)
		}
		if err := encoder.Encode(map[string]any{"id": call.ID, "error": map[string]any{"code": -32602, "message": "bad input", "data": map[string]string{"field": "text"}}}); err != nil {
			return err
		}
		scenario = "handshake"
	}
	if scenario == "timeout" {
		var slow, next peerFrame
		if err := decoder.Decode(&slow); err != nil {
			return err
		}
		if err := decoder.Decode(&next); err != nil {
			return err
		}
		if slow.Method != "example/slow" || next.Method != "example/afterTimeout" || string(slow.ID) == string(next.ID) {
			return fmt.Errorf("timed-out request retried or next request not distinct")
		}
		if err := encoder.Encode(map[string]any{"id": slow.ID, "result": json.RawMessage(`{"status":"late"}`)}); err != nil {
			return err
		}
		if err := encoder.Encode(map[string]any{"id": next.ID, "result": json.RawMessage(`{"status":"next"}`)}); err != nil {
			return err
		}
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	}
	if scenario == "concurrent" {
		var first, second peerFrame
		if err := decoder.Decode(&first); err != nil {
			return err
		}
		if err := decoder.Decode(&second); err != nil {
			return err
		}
		for _, request := range []peerFrame{second, first} {
			if err := encoder.Encode(map[string]any{"id": request.ID, "result": request.Params}); err != nil {
				return err
			}
		}
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	}
	if scenario == "request-number" || scenario == "request-string" {
		var call peerFrame
		if err := decoder.Decode(&call); err != nil {
			return err
		}
		if call.Method != "example/needsApproval" {
			return fmt.Errorf("unexpected call or unsolicited response: %+v", call)
		}
		requestID := call.ID
		if scenario == "request-string" {
			requestID = json.RawMessage(`"approval-1"`)
		}
		if err := encoder.Encode(map[string]any{"id": requestID, "method": "item/commandExecution/requestApproval", "params": map[string]string{"threadId": "thread-1", "turnId": "turn-1"}}); err != nil {
			return err
		}
		if err := encoder.Encode(map[string]any{"method": "item/started", "params": map[string]string{"threadId": "thread-1"}}); err != nil {
			return err
		}
		var response peerFrame
		if err := decoder.Decode(&response); err != nil {
			return err
		}
		if string(response.ID) != string(requestID) || string(response.Result) != `{"decision":"decline"}` || response.Method != "" {
			return fmt.Errorf("unexpected approval response: %+v", response)
		}
		if err := encoder.Encode(map[string]any{"id": call.ID, "result": json.RawMessage(`{"status":"rejected"}`)}); err != nil {
			return err
		}
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	}
	if scenario != "handshake" {
		return fmt.Errorf("unknown peer scenario %q", scenario)
	}
	frame = peerFrame{}
	if err := decoder.Decode(&frame); err != nil {
		return err
	}
	if frame.Method != "example/echo" {
		return fmt.Errorf("expected example/echo after initialization, got %q", frame.Method)
	}
	if err := encoder.Encode(map[string]any{"id": frame.ID, "result": json.RawMessage(`{"text":"hello"}`)}); err != nil {
		return err
	}
	_, err := io.Copy(io.Discard, os.Stdin)
	return err
}

func runQueuedApprovalPeer(decoder *json.Decoder, encoder *json.Encoder) error {
	control, err := net.DialTimeout("tcp", os.Args[len(os.Args)-1], 5*time.Second)
	if err != nil {
		return err
	}
	defer control.Close()
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	if err := encoder.Encode(map[string]any{"id": "queued-1", "method": "item/commandExecution/requestApproval", "params": map[string]string{"threadId": "thread-1", "turnId": "turn-1"}}); err != nil {
		return err
	}
	remaining := io.MultiReader(decoder.Buffered(), os.Stdin)
	var first [1]byte
	for {
		if _, err := io.ReadFull(remaining, first[:]); err != nil {
			return err
		}
		if !strings.ContainsRune(" \r\n\t", rune(first[0])) {
			break
		}
	}
	if first[0] != '{' {
		return fmt.Errorf("expected first byte of large request")
	}
	if err := encoder.Encode(map[string]any{"method": "example/writeStarted", "params": map[string]any{}}); err != nil {
		return err
	}
	var signal [1]byte
	if _, err := io.ReadFull(control, signal[:]); err != nil {
		return err
	}
	resolved := signal[0] == 'r'
	if resolved {
		if err := encoder.Encode(map[string]any{"method": "serverRequest/resolved", "params": map[string]string{"threadId": "thread-1", "requestId": "queued-1"}}); err != nil {
			return err
		}
		if _, err := io.ReadFull(control, signal[:]); err != nil {
			return err
		}
	}
	if signal[0] != 'u' {
		return fmt.Errorf("expected explicit pipe-read release")
	}
	decoder = json.NewDecoder(io.MultiReader(bytes.NewReader(first[:]), remaining))
	var large peerFrame
	if err := decoder.Decode(&large); err != nil {
		return err
	}
	if large.Method != "example/large" {
		return fmt.Errorf("unexpected large-frame method %q", large.Method)
	}
	if err := encoder.Encode(map[string]any{"id": large.ID, "result": map[string]string{"status": "received"}}); err != nil {
		return err
	}
	if !resolved {
		var answer peerFrame
		if err := decoder.Decode(&answer); err != nil {
			return err
		}
		if string(answer.ID) != `"queued-1"` || answer.Method != "" || string(answer.Result) != `{"decision":"decline"}` {
			return fmt.Errorf("missing or incorrect explicit approval response")
		}
	}
	var check peerFrame
	if err := decoder.Decode(&check); err != nil {
		return err
	}
	if check.Method != "example/check" {
		return fmt.Errorf("received stale response or unexpected frame after resolution")
	}
	if err := encoder.Encode(map[string]any{"id": check.ID, "result": map[string]bool{"ok": true}}); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, os.Stdin)
	return err
}
