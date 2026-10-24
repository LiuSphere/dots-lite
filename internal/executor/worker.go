package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dots-lite/internal/protocol"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Run keeps the authenticated transport alive independently of model readiness.
// Reconnect only resends unacknowledged events, never business operations.
func (s *Service) Run(ctx context.Context, hubURL, token string) error {
	if token == "" {
		return errors.New("executor token is required")
	}
	if !strings.HasPrefix(hubURL, "ws://") && !strings.HasPrefix(hubURL, "wss://") {
		return errors.New("hub URL must use ws or wss")
	}
	nativeCtx, cancelNative := context.WithCancel(ctx)
	nativeDone := make(chan struct{})
	go func() { defer close(nativeDone); s.superviseNative(nativeCtx) }()
	defer func() { cancelNative(); <-nativeDone }()
	delay := time.Second
	for {
		err := s.connectHub(ctx, hubURL, token)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var auth *authenticationError
		if errors.As(err, &auth) {
			return auth
		}
		s.mu.Lock()
		s.transportErr = err
		s.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < 15*time.Second {
			delay *= 2
			if delay > 15*time.Second {
				delay = 15 * time.Second
			}
		}
	}
}

const maxNativeRestarts = 3

// A transport reconnect never enters this loop or replenishes its persistent
// budget. Only a completely retired owned native process may be replaced.
func (s *Service) superviseNative(ctx context.Context) {
	if err := s.StartNative(ctx); err != nil {
		return
	}
	for {
		s.mu.Lock()
		client, pumpDone := s.client, s.pumpDone
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-client.Done():
		}
		if ctx.Err() != nil {
			return
		}
		cleanupErr := client.closeForRecovery()
		<-pumpDone
		s.background.Wait()
		s.mu.Lock()
		if cleanupErr != nil {
			s.nativeErr = errors.Join(s.nativeErr, cleanupErr)
			s.mu.Unlock()
			return
		}
		if s.eventFault != nil {
			s.mu.Unlock()
			return
		}
		cancelOld := s.cancel
		s.client, s.cancel, s.pumpDone = nil, nil, nil
		s.mu.Unlock()
		if cancelOld != nil {
			cancelOld()
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := s.reserveNativeRestart(ctx); err != nil {
			s.mu.Lock()
			s.nativeErr = errors.Join(s.nativeErr, err)
			s.mu.Unlock()
			return
		}
		// Start already closes failed initialization attempts. If it cannot
		// return an initialized Client, no cleanup proof is exposed for another
		// automatic attempt, so leave the failure visible and stop here.
		if err := s.StartNative(ctx); err != nil {
			return
		}
	}
}

func (s *Service) reserveNativeRestart(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eventFault != nil {
		return s.eventFault
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO executor_meta(key,value) VALUES('native_restart_used','0')`); err != nil {
		return err
	}
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM executor_meta WHERE key='native_restart_used'`).Scan(&stored); err != nil {
		return err
	}
	used, err := strconv.Atoi(stored)
	if err != nil || used < 0 {
		return fmt.Errorf("native restart budget is invalid: %q", stored)
	}
	if used >= maxNativeRestarts {
		return fmt.Errorf("native restart budget exhausted (%d/%d); explicit operator recovery is required", used, maxNativeRestarts)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE executor_meta SET value=? WHERE key='native_restart_used' AND value=?`, strconv.Itoa(used+1), stored)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("native restart budget changed before reservation")
	}
	return nil
}

type authenticationError struct{ message string }

func (e *authenticationError) Error() string { return e.message }

func (s *Service) connectHub(ctx context.Context, url, token string) error {
	conn, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
	if err != nil {
		if response != nil && (response.StatusCode == 401 || response.StatusCode == 403) {
			return &authenticationError{"hub rejected executor authentication"}
		}
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)
	s.mu.Lock()
	s.transportErr = nil
	s.mu.Unlock()
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writeMu sync.Mutex
	write := func(frame protocol.Frame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		frame.Version = protocol.Version
		deadline, stop := context.WithTimeout(connectionCtx, 15*time.Second)
		defer stop()
		return wsjson.Write(deadline, conn, frame)
	}
	snapshot, err := s.Snapshot(connectionCtx)
	if err != nil {
		return err
	}
	if err := write(protocol.Frame{Type: "hello", Snapshot: &snapshot}); err != nil {
		return err
	}
	writerErr := make(chan error, 1)
	reportError := func(err error) {
		select {
		case writerErr <- err:
		default:
		}
		cancel()
	}
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		var sent int64
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		var ticks int
		for {
			select {
			case <-connectionCtx.Done():
				return
			case <-ticker.C:
				events, err := s.PendingEvents(connectionCtx, sent)
				if err != nil {
					reportError(err)
					return
				}
				for _, event := range events {
					if err := write(protocol.Frame{Type: "event", Event: &event}); err != nil {
						reportError(err)
						return
					}
					sent = event.Sequence
				}
				ticks++
				if ticks%10 == 0 {
					snapshot, err := s.Snapshot(connectionCtx)
					if err != nil {
						reportError(err)
						return
					}
					if err := write(protocol.Frame{Type: "snapshot", Snapshot: &snapshot}); err != nil {
						reportError(err)
						return
					}
				}
			}
		}
	}()
	var operations sync.WaitGroup
	defer func() { cancel(); _ = conn.CloseNow(); operations.Wait(); <-senderDone }()
	for {
		var frame protocol.Frame
		if err := wsjson.Read(connectionCtx, conn, &frame); err != nil {
			cancel()
			select {
			case cause := <-writerErr:
				return cause
			default:
				return err
			}
		}
		if frame.Version != protocol.Version {
			return errors.New("unsupported hub protocol version")
		}
		switch frame.Type {
		case "ack":
			if err := s.AckEvents(connectionCtx, frame.AckSequence); err != nil {
				return err
			}
		case "operation":
			if frame.Operation == nil || frame.ID == "" {
				return errors.New("hub sent incomplete operation frame")
			}
			op, id := *frame.Operation, frame.ID
			operations.Go(func() {
				result := s.Execute(connectionCtx, op)
				if err := write(protocol.Frame{Type: "result", ID: id, Result: &result}); err != nil {
					reportError(err)
				}
			})
		default:
			return errors.New("unsupported hub frame")
		}
	}
}
