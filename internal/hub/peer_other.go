//go:build !linux

package hub

import (
	"errors"
	"net"
)

func supportsPeerCredentials() bool                { return false }
func listenUnix(path string) (net.Listener, error) { return net.Listen("unix", path) }
func peerUID(net.Conn) (int, error) {
	return 0, errors.New("Unix peer UID authentication is unsupported")
}
