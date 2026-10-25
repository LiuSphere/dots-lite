//go:build !windows

package executor

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func lockState(path string) (func() error, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return func() error { return f.Close() }, nil
}
