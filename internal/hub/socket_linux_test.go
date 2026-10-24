//go:build linux

package hub_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dots-duet-lite/internal/hub"
	"dots-duet-lite/internal/store"
)

func TestUnixListenerRecoversAnOwnedStaleSocket(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("hub must run without root privileges")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := hub.New(testConfig(), db)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.ServeUnix(ctx, path) }()
	for {
		conn, err := net.DialTimeout("unix", path, 20*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("stale socket prevented service restart: %v", err)
		case <-ctx.Done():
			t.Fatal("restarted service never became available")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnixListenerDoesNotReplaceAnActiveSocketOrRegularFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("hub must run without root privileges")
	}
	for _, active := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "hub.sock")
		var listener *net.UnixListener
		if active {
			var err error
			listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
		} else if err := os.WriteFile(path, []byte("keep this file"), 0600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(filepath.Join(dir, "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		h, err := hub.New(testConfig(), db)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = h.ServeUnix(ctx, path)
		cancel()
		db.Close()
		if err == nil {
			t.Fatal("existing live endpoint or regular file was replaced")
		}
		after, err := os.Lstat(path)
		if err != nil || !os.SameFile(before, after) {
			t.Fatalf("existing endpoint changed: %v", err)
		}
		if active {
			conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
			if err != nil {
				t.Fatal("existing listener was damaged:", err)
			}
			conn.Close()
		} else if data, err := os.ReadFile(path); err != nil || string(data) != "keep this file" {
			t.Fatalf("regular file changed: %q, %v", data, err)
		}
	}
}
