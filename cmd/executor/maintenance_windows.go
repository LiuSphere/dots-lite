//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"dots-lite/internal/executor"
	"dots-lite/internal/protocol"
	"golang.org/x/sys/windows"
)

type maintenanceReply struct {
	Scope           protocol.Scope       `json:"scope"`
	ConfigurationID string               `json:"configuration_id"`
	State           protocol.Maintenance `json:"state"`
	Error           string               `json:"error,omitempty"`
	PID             int                  `json:"pid"`
}

func configurationID(cfg serviceConfig) string {
	raw, _ := json.Marshal(cfg)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func pipeName(configPath string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(configPath))))
	return `\\.\pipe\dots-lite-` + hex.EncodeToString(sum[:16])
}

func pipeAttributes() (*windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

func serveMaintenance(ctx context.Context, configPath string, cfg serviceConfig, service *executor.Service, stop context.CancelFunc) (func(), error) {
	name, err := windows.UTF16PtrFromString(pipeName(configPath))
	if err != nil {
		return nil, err
	}
	sa, err := pipeAttributes()
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var active windows.Handle
	var activeFile *os.File
	done := make(chan struct{})
	closed := false
	create := func() (windows.Handle, error) {
		return windows.CreateNamedPipe(name, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE|windows.FILE_FLAG_OVERLAPPED, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 4096, 4096, 0, sa)
	}
	active, err = create()
	if err != nil {
		return nil, err
	}
	ctx, cancelControl := context.WithCancel(ctx)
	go func() {
		defer close(done)
		for {
			mu.Lock()
			handle := active
			mu.Unlock()
			err := connectControlPipe(ctx, handle)
			if err != nil {
				mu.Lock()
				_ = windows.CloseHandle(handle)
				active = 0
				mu.Unlock()
				return
			}
			file := os.NewFile(uintptr(handle), "dots-lite-control")
			mu.Lock()
			activeFile = file
			mu.Unlock()
			_ = file.SetDeadline(time.Now().Add(45 * time.Second))
			identity := maintenanceReply{Scope: cfg.Scope, ConfigurationID: configurationID(cfg), PID: os.Getpid(), State: service.MaintenanceStatus()}
			identityErr := json.NewEncoder(file).Encode(identity)
			var command struct {
				Action          string         `json:"action"`
				Scope           protocol.Scope `json:"scope"`
				ConfigurationID string         `json:"configuration_id"`
			}
			decodeErr := identityErr
			if decodeErr == nil {
				decodeErr = json.NewDecoder(io.LimitReader(file, 4096)).Decode(&command)
			}
			var actionErr error
			if decodeErr != nil {
				actionErr = decodeErr
			} else if command.Scope != cfg.Scope || command.ConfigurationID != identity.ConfigurationID {
				actionErr = errors.New("control scope or installed configuration differs")
			} else if command.Action == "pause" {
				deadline, cancel := context.WithTimeout(ctx, 40*time.Second)
				actionErr = service.PauseForMaintenance(deadline)
				cancel()
			} else if command.Action != "status" {
				actionErr = errors.New("unsupported maintenance action")
			}
			reply := identity
			reply.State = service.MaintenanceStatus()
			if actionErr != nil {
				reply.Error = actionErr.Error()
			}
			writeErr := json.NewEncoder(file).Encode(reply)
			mu.Lock()
			active = 0
			activeFile = nil
			_ = file.Close()
			wasClosed := closed
			mu.Unlock()
			if command.Action == "pause" && actionErr == nil && writeErr == nil {
				stop()
				return
			}
			if wasClosed || ctx.Err() != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				return
			}
			active, err = create()
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancelControl()
			mu.Lock()
			closed = true
			if active != 0 {
				if activeFile != nil {
					_ = activeFile.Close()
				} else {
					_ = windows.CancelIoEx(active, nil)
				}
			}
			mu.Unlock()
			<-done
		})
	}, nil
}

func connectControlPipe(ctx context.Context, handle windows.Handle) error {
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(event)
	overlap := windows.Overlapped{HEvent: event}
	err = windows.ConnectNamedPipe(handle, &overlap)
	if errors.Is(err, windows.ERROR_PIPE_CONNECTED) || err == nil {
		return nil
	}
	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		return err
	}
	for {
		status, err := windows.WaitForSingleObject(event, 20)
		if err != nil {
			return err
		}
		if status == windows.WAIT_OBJECT_0 {
			var n uint32
			return windows.GetOverlappedResult(handle, &overlap, &n, false)
		}
		if ctx.Err() != nil {
			_ = windows.CancelIoEx(handle, &overlap)
			var n uint32
			_ = windows.GetOverlappedResult(handle, &overlap, &n, true)
			return ctx.Err()
		}
	}
}

func dialControl(ctx context.Context, path, action string) (maintenanceReply, windows.Handle, error) {
	var reply maintenanceReply
	name, err := windows.UTF16PtrFromString(pipeName(path))
	if err != nil {
		return reply, 0, err
	}
	var handle windows.Handle
	connectDeadline := time.Now().Add(time.Second)
	for {
		handle, err = windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return reply, 0, err
		}
		if time.Now().After(connectDeadline) {
			return reply, 0, err
		}
		select {
		case <-ctx.Done():
			return reply, 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	file := os.NewFile(uintptr(handle), "dots-lite-control")
	defer file.Close()
	var pid uint32
	if err = windows.GetNamedPipeServerProcessId(handle, &pid); err != nil {
		return reply, 0, err
	}
	process, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return reply, 0, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = windows.CloseHandle(process)
		}
	}()
	buf := make([]uint16, 32768)
	length := uint32(len(buf))
	if err = windows.QueryFullProcessImageName(process, 0, &buf[0], &length); err != nil {
		return reply, 0, err
	}
	self, err := os.Executable()
	if err != nil {
		return reply, 0, err
	}
	if !strings.EqualFold(filepath.Clean(windows.UTF16ToString(buf[:length])), filepath.Clean(self)) {
		return reply, 0, errors.New("control pipe belongs to a different executable")
	}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			_ = file.Close()
		case <-finished:
		}
	}()
	cfg, err := readServiceConfig(path)
	if err != nil {
		return reply, 0, err
	}
	if err := json.NewDecoder(io.LimitReader(file, 16384)).Decode(&reply); err != nil {
		return reply, 0, err
	}
	if reply.PID != int(pid) || reply.Scope != cfg.Scope || reply.ConfigurationID != configurationID(cfg) {
		return reply, 0, errors.New("control handshake differs from the intended scope or configuration")
	}
	if err = json.NewEncoder(file).Encode(map[string]any{"action": action, "scope": cfg.Scope, "configuration_id": configurationID(cfg)}); err != nil {
		return reply, 0, err
	}
	if err = json.NewDecoder(io.LimitReader(file, 16384)).Decode(&reply); err != nil {
		return reply, 0, err
	}
	if reply.PID != int(pid) {
		return reply, 0, errors.New("control server identity changed")
	}
	if reply.Error != "" {
		return reply, 0, errors.New(reply.Error)
	}
	keep = true
	return reply, process, nil
}

// Values travel through the child environment, never through PowerShell code.
func scheduledTask(ctx context.Context, cfg serviceConfig, action string) error {
	if cfg.Maintenance.TaskName == "" || cfg.Maintenance.TaskExecutable == "" {
		return errors.New("maintenance requires the exact scheduled task name, executable and arguments in configuration")
	}
	script := `$ErrorActionPreference='Stop'
$full=$env:DOTS_CONTROL_TASK
$parent=Split-Path $full -Parent
if (!$parent) {$parent='\'} elseif (!$parent.EndsWith('\')) {$parent+='\'}
$task=Get-ScheduledTask -TaskName (Split-Path $full -Leaf) -TaskPath $parent
# Named export reads the registered SID; piping the task summary can serialize
# its unqualified display name instead, which needs account environment to map.
$registered=[xml](Export-ScheduledTask -TaskName $task.TaskName -TaskPath $task.TaskPath)
$sid=[string]$registered.Task.Principals.Principal.UserId
if (!$sid.StartsWith('S-1-')) {$sid=([System.Security.Principal.NTAccount]::new($sid)).Translate([System.Security.Principal.SecurityIdentifier]).Value}
if ($sid -ne [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value) {throw 'Scheduled task owner differs'}
if (@($task.Actions).Count -ne 1 -or $task.Actions[0].Execute -ne $env:DOTS_CONTROL_EXE -or $task.Actions[0].Arguments -cne $env:DOTS_CONTROL_ARGS) {throw 'Scheduled task launch command differs'}
switch ($env:DOTS_CONTROL_ACTION) {
 'disable' {$null=$task|Disable-ScheduledTask; if ((Get-ScheduledTask -TaskName $task.TaskName -TaskPath $task.TaskPath).Settings.Enabled) {throw 'Automatic restart remains enabled'}}
 'start' {$null=$task|Enable-ScheduledTask; $task|Start-ScheduledTask}
 'stopped' {if ($task.State -eq 'Running') {throw 'Scheduled task launcher is still running'}}
 'offline' {if ($task.State -eq 'Running' -or $task.Settings.Enabled) {throw 'Disable and stop the old scheduled task before migration'}}
 default {throw 'Invalid scheduler action'}
}`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Env = append(os.Environ(), "DOTS_CONTROL_TASK="+cfg.Maintenance.TaskName, "DOTS_CONTROL_EXE="+cfg.Maintenance.TaskExecutable, "DOTS_CONTROL_ARGS="+cfg.Maintenance.TaskArguments, "DOTS_CONTROL_ACTION="+action)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("scheduled task %s: %w: %s", action, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func runMaintenance(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("maintenance requires pause, resume or status")
	}
	action := args[0]
	flags := flag.NewFlagSet("maintenance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "installed executor configuration")
	offline := flags.Bool("offline-confirmed", false, "old executor and its owned native processes have exited")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return errors.New("maintenance requires -config <installed JSON>")
	}
	full, err := filepath.Abs(*path)
	if err != nil {
		return err
	}
	cfg, err := readServiceConfig(full)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	unlock, err := executor.LockMaintenanceCommand(cfg.StatePath)
	if err != nil {
		return err
	}
	defer unlock()
	if action == "migrate" {
		if !*offline {
			return errors.New("migration requires -offline-confirmed after the old executor and owned native processes exited")
		}
		if err := scheduledTask(ctx, cfg, "offline"); err != nil {
			return err
		}
		backup, err := executor.MigrateV3(ctx, executor.ServiceConfig{Scope: cfg.Scope, Folders: cfg.Folders, DirectoryMode: cfg.DirectoryMode, StatePath: cfg.StatePath})
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(map[string]string{"phase": "stopped", "backup": backup, "notice": "Do not restore the old snapshot after new business or control writes"})
	}
	if action == "status" {
		reply, process, err := dialControl(ctx, full, "status")
		if err != nil {
			state, offlineErr := executor.ReadOfflineMaintenance(ctx, cfg.StatePath)
			if offlineErr != nil {
				return errors.Join(err, offlineErr)
			}
			return json.NewEncoder(stdout).Encode(map[string]any{"state": state, "executor_process": "offline"})
		}
		defer windows.CloseHandle(process)
		return json.NewEncoder(stdout).Encode(reply)
	}
	if action != "pause" && action != "resume" {
		return errors.New("maintenance requires pause, resume or status")
	}
	if action == "pause" {
		if err := scheduledTask(ctx, cfg, "disable"); err != nil {
			return err
		}
		reply, process, err := dialControl(ctx, full, "pause")
		if err != nil {
			if scheduledTask(ctx, cfg, "stopped") == nil {
				if state, offlineErr := executor.CompleteOfflineMaintenance(ctx, cfg.StatePath, false); offlineErr == nil {
					return json.NewEncoder(stdout).Encode(state)
				}
			}
			return fmt.Errorf("automatic restart disabled; pause remains unconfirmed: %w", err)
		}
		defer windows.CloseHandle(process)
		if reply.State.Phase != "stopping" {
			return errors.New("executor did not confirm shutdown eligibility")
		}
		for {
			status, err := windows.WaitForSingleObject(process, 100)
			if err != nil {
				return err
			}
			if status == windows.WAIT_OBJECT_0 {
				break
			}
			if ctx.Err() != nil {
				return errors.New("executor main process has not exited before the deadline")
			}
		}
		var exitCode uint32
		if err := windows.GetExitCodeProcess(process, &exitCode); err != nil || exitCode != 0 {
			return fmt.Errorf("executor cleanup failed (exit %d): %v", exitCode, err)
		}
		for {
			err = scheduledTask(ctx, cfg, "stopped")
			if err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return err
			case <-time.After(200 * time.Millisecond):
			}
		}
		state, err := executor.CompleteOfflineMaintenance(ctx, cfg.StatePath, false)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(state)
	}
	if err := scheduledTask(ctx, cfg, "stopped"); err != nil {
		return err
	}
	if _, err := executor.CompleteOfflineMaintenance(ctx, cfg.StatePath, true); err != nil {
		return err
	}
	if err := scheduledTask(ctx, cfg, "start"); err != nil {
		return err
	}
	for {
		reply, process, err := dialControl(ctx, full, "status")
		if process != 0 {
			_ = windows.CloseHandle(process)
		}
		if err == nil && reply.State.Phase == "running" {
			return json.NewEncoder(stdout).Encode(reply)
		}
		if ctx.Err() != nil {
			stopErr := scheduledTask(context.WithoutCancel(ctx), cfg, "disable")
			return errors.Join(errors.New("resume did not reach running; maintenance remains unresolved"), err, stopErr)
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}
