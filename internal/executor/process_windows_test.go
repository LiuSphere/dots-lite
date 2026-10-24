//go:build windows

package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"dots-lite/internal/executor"
	"golang.org/x/sys/windows"
)

func TestContextCancellationTerminatesOwnedProcessWithoutCompetingKills(t *testing.T) {
	for attempt := 0; attempt < 8; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		client, err := executor.Start(ctx, executor.Options{
			Command: []string{os.Args[0], "-test.run=^TestOwnedProcessPeer$", "--", "parent"},
			Stderr:  io.Discard,
		})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		select {
		case <-client.Events():
		case <-time.After(3 * time.Second):
			cancel()
			_ = client.Close()
			t.Fatal("peer did not start its owned child")
		}
		cancel()
		if err := client.Close(); errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("context cancellation raced another process termination: %v", err)
		}
	}
}

func TestCloseTerminatesOwnedDescendantProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{os.Args[0], "-test.run=^TestOwnedProcessPeer$", "--", "parent"},
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	var message executor.Message
	select {
	case message = <-client.Events():
	case <-ctx.Done():
		t.Fatal("peer did not identify its owned descendant")
	}
	var child struct {
		PID uint32 `json:"pid"`
	}
	if err := json.Unmarshal(message.Params, &child); err != nil || child.PID == 0 {
		t.Fatalf("invalid descendant identity: %s, %v", message.Params, err)
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, child.PID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		state, err := windows.WaitForSingleObject(handle, 0)
		if err != nil {
			t.Error(err)
		}
		if state == uint32(windows.WAIT_TIMEOUT) {
			if err := windows.TerminateProcess(handle, 1); err != nil {
				t.Error(err)
			}
		}
		if err := windows.CloseHandle(handle); err != nil {
			t.Error(err)
		}
	})
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("unresponsive peer termination was reported as graceful")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close remained blocked by the owned descendant after killing its parent")
	}
	state, err := windows.WaitForSingleObject(handle, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if state != windows.WAIT_OBJECT_0 {
		t.Fatal("owned descendant survived app-server shutdown")
	}
}

func TestOwnedProcessPeer(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "child":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "parent":
		decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
		var initialize struct {
			ID json.RawMessage `json:"id"`
		}
		if err := decoder.Decode(&initialize); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := encoder.Encode(map[string]any{"id": initialize.ID, "result": map[string]string{"userAgent": "owned-process-fixture"}}); err != nil {
			os.Exit(2)
		}
		var initialized map[string]any
		if err := decoder.Decode(&initialized); err != nil {
			os.Exit(2)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestOwnedProcessPeer$", "--", "child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := encoder.Encode(map[string]any{"method": "fixture/descendant", "params": map[string]int{"pid": child.Process.Pid}}); err != nil {
			os.Exit(2)
		}
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
}
