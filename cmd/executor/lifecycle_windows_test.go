//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"dots-lite/internal/executor"
	"dots-lite/internal/hub"
	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
	"golang.org/x/sys/windows"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "fixture-descendant" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "app-server" {
		for i, arg := range os.Args {
			if arg == "--out" && i+1 < len(os.Args) {
				root := filepath.Join(os.Args[i+1], "v2")
				if err := os.MkdirAll(root, 0700); err != nil {
					os.Exit(3)
				}
				if err := os.WriteFile(filepath.Join(root, "ThreadListParams.json"), []byte(`{"definitions":{"ThreadSourceKind":{"enum":["cli","vscode","exec","appServer","subAgent","subAgentReview","subAgentCompact","subAgentThreadSpawn","subAgentOther","unknown"]}}}`), 0600); err != nil {
					os.Exit(3)
				}
				os.Exit(0)
			}
		}
		if os.Getenv("DOTS_LITE_LIFECYCLE_CONFIG") == "" {
			os.Exit(3)
		}
		child := exec.Command(os.Args[0], "fixture-descendant")
		child.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(os.Getenv("DOTS_LITE_LIFECYCLE_CONFIG")+".child-pid", []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			os.Exit(3)
		}
		dec, enc := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
		for {
			var frame struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := dec.Decode(&frame); err != nil {
				if err == io.EOF {
					os.Exit(0)
				}
				os.Exit(3)
			}
			if frame.Method == "initialized" {
				continue
			}
			if frame.Method != "initialize" {
				os.Exit(3)
			}
			if err := enc.Encode(map[string]any{"id": frame.ID, "result": map[string]string{"userAgent": "lifecycle-fixture"}}); err != nil {
				os.Exit(3)
			}
		}
	}
	os.Exit(m.Run())
}

func TestLifecycleServicePeer(t *testing.T) {
	path := os.Getenv("DOTS_LITE_LIFECYCLE_CONFIG")
	if path == "" {
		return
	}
	if err := runService(context.Background(), path, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	os.Exit(0)
}

func TestMaintenanceActuallyExitsExecutorAndOwnedDescendantBeforeOfflineStopped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	binding := hub.Binding{UID: 1001, UserID: "fixture", ProfileID: "fixture", ExecutorID: "fixture", Token: "fixture-token", Sources: []hub.AllowedSource{{Platform: "fixture", SenderID: "sender", ChatID: "chat"}}}
	h, err := hub.New(hub.Config{Bindings: []hub.Binding{binding}}, state)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	server := httptest.NewServer(h.WebsocketHandler())
	defer server.Close()
	path := filepath.Join(root, "executor.json")
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte(binding.Token), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := serviceConfig{Scope: protocol.Scope{UserID: "fixture", ProfileID: "fixture", ExecutorID: "fixture"}, StatePath: filepath.Join(root, "executor.db"), HubURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/executor", TokenFile: token, Codex: os.Args[0], DirectoryMode: "task"}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleServicePeer$")
	command.Env = append(os.Environ(), "DOTS_LITE_LIFECYCLE_CONFIG="+path)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	ready := false
	for ctx.Err() == nil {
		args := json.RawMessage(`{"query":"machine"}`)
		ref, err := h.RegisterInvocation(ctx, 1001, protocol.Invocation{Tool: "dots_lite_query", Arguments: args, Source: protocol.Source{Kind: "gateway", Platform: "fixture", SenderID: "sender", ChatID: "chat", SessionID: "test"}})
		if err != nil {
			t.Fatal(err)
		}
		result, err := h.Call(ctx, 1001, protocol.ToolRequest{Tool: "dots_lite_query", Arguments: args, ContextRef: ref})
		if err != nil {
			t.Fatal(err)
		}
		var machine struct {
			Snapshot protocol.Snapshot `json:"snapshot"`
		}
		_ = json.Unmarshal(result, &machine)
		if machine.Snapshot.Initialized {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("fixture executor never initialized")
	}
	pidRaw, err := os.ReadFile(path + ".child-pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidRaw))
	if err != nil {
		t.Fatal(err)
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(child)
	reply, process, err := dialControl(ctx, path, "pause")
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process)
	if reply.State.Phase != "stopping" {
		t.Fatalf("pause reply claims wrong phase: %+v", reply)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	waited = true
	if result, err := windows.WaitForSingleObject(process, 0); err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("executor main process remains alive: %d %v", result, err)
	}
	if result, err := windows.WaitForSingleObject(child, 0); err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("owned descendant remains alive: %d %v", result, err)
	}
	stopped, err := executor.CompleteOfflineMaintenance(ctx, cfg.StatePath, false)
	if err != nil || stopped.Phase != "stopped" {
		t.Fatalf("complete stop: %+v %v", stopped, err)
	}
}
