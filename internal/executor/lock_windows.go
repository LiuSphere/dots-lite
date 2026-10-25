//go:build windows

package executor

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func lockState(path string) (func() error, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	o := new(windows.Overlapped)
	if err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, o); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return func() error { return errors.Join(windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, o), f.Close()) }, nil
}
