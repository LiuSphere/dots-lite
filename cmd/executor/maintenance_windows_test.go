//go:build windows

package main

import (
	"context"
	"dots-lite/internal/executor"
	"dots-lite/internal/protocol"
	"encoding/json"
	"golang.org/x/sys/windows"
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
