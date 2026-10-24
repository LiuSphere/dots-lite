package hub

import (
	"errors"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func supportsPeerCredentials() bool { return true }

func listenUnix(path string) (net.Listener, error) {
	listener, err := net.Listen("unix", path)
	if err == nil || !errors.Is(err, syscall.EADDRINUSE) {
		return listener, err
	}
	original := err
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, errors.Join(original, err)
	}
	if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errors.Join(original, errors.New("existing socket belongs to another user"))
	}
	probe, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		return nil, errors.Join(original, probe.Close())
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return nil, errors.Join(original, err)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.Join(original, err, errors.New("socket changed during restart check"))
	}
	if err := os.Remove(path); err != nil {
		return nil, errors.Join(original, err)
	}
	return net.Listen("unix", path)
}
func peerUID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("peer is not a Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var peerErr error
	if err = raw.Control(func(fd uintptr) {
		cred, peerErr = unix.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if peerErr != nil {
		return 0, peerErr
	}
	return int(cred.Uid), nil
}
