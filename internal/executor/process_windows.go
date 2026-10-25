//go:build windows

package executor

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The child is suspended until it belongs to this job. Assigning a running
// shim would leave a race in which it can spawn an unowned app-server first.
type processControl struct {
	job  windows.Handle
	once sync.Once
	err  error
}

func prepareProcess(cmd *exec.Cmd) (*processControl, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create app-server job: %w", err)
	}
	control := &processControl{job: job}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, errors.Join(fmt.Errorf("configure app-server job: %w", err), control.close())
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	cmd.SysProcAttr.HideWindow = true
	// CommandContext and Client.abort share the same idempotent job cleanup.
	// A separate Process.Kill can race job termination and report access denied.
	cmd.Cancel = func() error { return control.kill(cmd) }
	return control, nil
}

func (p *processControl) started(cmd *exec.Cmd) (err error) {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("open suspended app-server: %w", err)
	}
	defer func() { err = errors.Join(err, windows.CloseHandle(process)) }()
	if err := windows.AssignProcessToJobObject(p.job, process); err != nil {
		return fmt.Errorf("contain suspended app-server: %w", err)
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("find suspended app-server thread: %w", err)
	}
	defer func() { err = errors.Join(err, windows.CloseHandle(snapshot)) }()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	scanErr := windows.Thread32First(snapshot, &entry)
	for ; scanErr == nil; scanErr = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(cmd.Process.Pid) {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return fmt.Errorf("open suspended app-server thread: %w", openErr)
		}
		previous, resumeErr := windows.ResumeThread(thread)
		closeErr := windows.CloseHandle(thread)
		if resumeErr != nil {
			return errors.Join(fmt.Errorf("resume contained app-server: %w", resumeErr), closeErr)
		}
		if previous != 1 {
			return errors.Join(errors.New("app-server initial thread had unexpected suspension state"), closeErr)
		}
		return closeErr
	}
	if !errors.Is(scanErr, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("enumerate suspended app-server threads: %w", scanErr)
	}
	return errors.New("suspended app-server initial thread was not found")
}

// Keep the job handle until accounting confirms that its descendants exited.
func (p *processControl) close() error {
	p.once.Do(func() {
		defer func() { p.err = errors.Join(p.err, windows.CloseHandle(p.job)) }()
		type accounting struct {
			TotalUserTime, TotalKernelTime, PeriodUserTime, PeriodKernelTime int64
			PageFaults, TotalProcesses, ActiveProcesses, TerminatedProcesses uint32
		}
		query := func() (uint32, error) {
			var info accounting
			err := windows.QueryInformationJobObject(p.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
			return info.ActiveProcesses, err
		}
		active, err := query()
		if err != nil {
			p.err = err
			return
		}
		if active == 0 {
			return
		}
		if err := windows.TerminateJobObject(p.job, 1); err != nil {
			p.err = err
			return
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			active, err = query()
			if err != nil {
				p.err = err
				return
			}
			if active == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		p.err = errors.New("owned native descendants have not all exited")
	})
	return p.err
}

func (p *processControl) kill(cmd *exec.Cmd) error { return p.close() }
