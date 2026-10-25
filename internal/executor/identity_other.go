//go:build !windows

package executor

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func directoryIdentity(root *os.Root) (string, error) {
	f, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer f.Close()
	var info unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x", info.Dev, info.Ino), nil
}
