//go:build windows

package main

import (
	"context"
	"dots-lite/internal/executor"
	"dots-lite/internal/protocol"
	"encoding/json"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestControlPipeReturnsStateAndRequestsExitOnlyAfterQuiescence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "executor.json")
	service, err := executor.NewService(executor.ServiceConfig{Scope: protocol.Scope{UserID: "test", ProfileID: "test", ExecutorID: "pipe-test"}, StatePath: filepath.Join(t.TempDir(), "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	stopped := make(chan struct{})
	cfg := serviceConfig{Scope: protocol.Scope{UserID: "test", ProfileID: "test", ExecutorID: "pipe-test"}}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	closeControl, err := serveMaintenance(ctx, path, cfg, service, func() { close(stopped) })
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	status, process, err := dialControl(ctx, path, "status")
	if err != nil {
		t.Fatal(err)
	}
	_ = windows.CloseHandle(process)
	if status.State.Phase != "running" {
		t.Fatalf("status: %+v", status)
	}
	reply, process, err := dialControl(ctx, path, "pause")
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process)
	if reply.State.Phase != "stopping" || reply.State.Epoch != 1 {
		t.Fatalf("pause prematurely reported complete shutdown: %+v", reply)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("successful pause did not request main exit")
	}
}

func TestControlPipeClosesWhileWaitingForConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := executor.NewService(executor.ServiceConfig{Scope: protocol.Scope{UserID: "test", ProfileID: "test", ExecutorID: "pipe-close"}, StatePath: filepath.Join(t.TempDir(), "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	closeControl, err := serveMaintenance(ctx, filepath.Join(t.TempDir(), "executor.json"), serviceConfig{}, service, func() {})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { closeControl(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closing control blocked in a native pipe accept")
	}
}

func TestControlHandshakeRejectsConfigurationDriftBeforePausing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	path := filepath.Join(root, "executor.json")
	scope := protocol.Scope{UserID: "test", ProfileID: "test", ExecutorID: "config-drift"}
	service, err := executor.NewService(executor.ServiceConfig{Scope: scope, StatePath: filepath.Join(root, "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	cfg := serviceConfig{Scope: scope}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	closeControl, err := serveMaintenance(ctx, path, cfg, service, func() { t.Error("wrong configuration requested service exit") })
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	changed := cfg
	changed.Scope.ProfileID = "other-profile"
	changedRaw, _ := json.Marshal(changed)
	if err := os.WriteFile(path, changedRaw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, process, err := dialControl(ctx, path, "pause"); err == nil {
		_ = windows.CloseHandle(process)
		t.Fatal("configuration drift reached pause")
	}
	if service.MaintenanceStatus().Phase != "running" {
		t.Fatal("mismatched control paused the service before identity validation")
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, process, err := dialControl(ctx, path, "status"); err != nil {
		t.Fatal(err)
	} else {
		_ = windows.CloseHandle(process)
	}
}

func TestInstalledScheduledTaskUsesRegisteredIdentityWithoutAccountEnvironment(t *testing.T) {
	path := os.Getenv("DOTS_LITE_WINDOWS_TASK_AUDIT")
	if path == "" {
		t.Skip("set DOTS_LITE_WINDOWS_TASK_AUDIT to a disabled, stopped installation configuration for a read-only scheduler check")
	}
	cfg, err := readServiceConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"COMPUTERNAME", "USERDOMAIN", "USERNAME"} {
		t.Setenv(name, "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := scheduledTask(ctx, cfg, "offline"); err != nil {
		t.Fatalf("registered owner could not be verified without account environment: %v", err)
	}
	cfg.Maintenance.TaskExecutable += ".wrong"
	if err := scheduledTask(ctx, cfg, "offline"); err == nil {
		t.Fatal("scheduler accepted a different registered launch command")
	}
}

func TestControlPipeWaitsForReplyReceiptAndSurvivesDelayedClientClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	root := t.TempDir()
	path := filepath.Join(root, "executor.json")
	cfg := serviceConfig{Scope: protocol.Scope{UserID: "test", ProfileID: "test", ExecutorID: "slow-controller"}}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	service, err := executor.NewService(executor.ServiceConfig{Scope: cfg.Scope, StatePath: filepath.Join(root, "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	closeControl, err := serveMaintenance(ctx, path, cfg, service, func() { t.Error("status requested shutdown") })
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	name, _ := windows.UTF16PtrFromString(pipeName(path))
	for i := 0; i < 10; i++ {
		var handle windows.Handle
		for {
			handle, err = windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				t.Fatalf("listener did not return after client %d: %v", i, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		file := os.NewFile(uintptr(handle), "slow-controller")
		if i < 2 {
			// An abandoned connection must not retire the service's only
			// control listener before the next controller arrives.
			file.Close()
			continue
		}
		_ = file.SetDeadline(time.Now().Add(time.Second))
		dec := json.NewDecoder(io.LimitReader(file, 16384))
		enc := json.NewEncoder(file)
		var reply maintenanceReply
		if err := dec.Decode(&reply); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := enc.Encode(map[string]any{"action": "status", "scope": cfg.Scope, "configuration_id": configurationID(cfg)}); err != nil {
			file.Close()
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
		if err := dec.Decode(&reply); err != nil {
			file.Close()
			t.Fatalf("reply was discarded before controller read: %v", err)
		}
		if err := enc.Encode(map[string]bool{"received": true}); err != nil {
			file.Close()
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
		file.Close()
	}
	if _, process, err := dialControl(ctx, path, "status"); err != nil {
		t.Fatalf("listener did not survive repeated delayed controllers: %v", err)
	} else {
		_ = windows.CloseHandle(process)
	}
}
