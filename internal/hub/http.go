package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dots-duet-lite/internal/protocol"
)

type peerKey struct{}

// APIHandler accepts only identities attached by the Unix listener, never HTTP headers.
func (h *Hub) APIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, ok := r.Context().Value(peerKey{}).(int)
		if !ok {
			writeFailure(w, failure("forbidden", "verified Unix peer credentials required"))
			return
		}
		if _, err := h.binding(uid); err != nil {
			writeFailure(w, err)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
		var result any
		var err error
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/invocations":
			var in protocol.Invocation
			if err = readJSON(r, &in); err == nil {
				var ref string
				ref, err = h.RegisterInvocation(r.Context(), uid, in)
				result = map[string]string{"context_ref": ref}
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tools/call":
			var in protocol.ToolRequest
			if err = readJSON(r, &in); err == nil {
				result, err = h.Call(r.Context(), uid, in)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/approvals/confirm":
			var in Confirmation
			if err = readJSON(r, &in); err == nil {
				result, err = h.ConfirmApproval(r.Context(), uid, in)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/followups/next":
			result, err = h.NextFollowup(r.Context(), uid)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/artifacts/next":
			result, err = h.NextArtifact(r.Context(), uid)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/deliveries/next":
			result, err = h.NextDelivery(r.Context(), uid)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/deliveries/"):
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) != 5 || parts[4] != "report" {
				err = failure("not_found", "unknown delivery route")
				break
			}
			var body json.RawMessage
			if err = readJSON(r, &body); err == nil {
				result, err = h.ReportDelivery(r.Context(), uid, parts[3], body)
			}
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/artifacts/"):
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) != 5 || parts[4] != "acquired" {
				err = failure("not_found", "unknown artifact route")
				break
			}
			var body json.RawMessage
			if err = readJSON(r, &body); err == nil {
				result, err = h.RecordArtifactAcquisition(r.Context(), uid, parts[3], body)
			}
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/followups/"):
			parts := strings.Split(r.URL.Path, "/")
			if len(parts) != 5 {
				err = failure("not_found", "unknown followup route")
				break
			}
			var body json.RawMessage
			if err = readJSON(r, &body); err != nil {
				break
			}
			switch parts[4] {
			case "run":
				result, err = h.SaveFollowupRun(r.Context(), uid, parts[3], body)
			case "report":
				result, err = h.ReportFollowup(r.Context(), uid, parts[3], body)
			default:
				err = failure("not_found", "unknown followup route")
			}
		default:
			err = failure("not_found", "unknown API route")
		}
		if err != nil {
			writeFailure(w, err)
			return
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			writeFailure(w, err)
			return
		}
		if len(encoded) > 16<<20 {
			writeFailure(w, failure("response_too_large", "API response exceeds 16 MiB"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err = w.Write(append(encoded, '\n')); err != nil {
			return
		}
	})
}
func readJSON(r *http.Request, dst any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return failure("invalid_arguments", err.Error())
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return failure("invalid_arguments", "request must contain exactly one JSON value")
	}
	return nil
}
func writeFailure(w http.ResponseWriter, err error) {
	var p *protocol.Error
	if !errors.As(err, &p) {
		p = &protocol.Error{Code: "internal", Message: err.Error()}
	}
	status := http.StatusBadRequest
	switch p.Code {
	case "forbidden", "invalid_context", "unbound_source":
		status = http.StatusForbidden
	case "not_found":
		status = http.StatusNotFound
	case "offline":
		status = http.StatusServiceUnavailable
	case "internal":
		status = http.StatusInternalServerError
	case "response_too_large":
		status = http.StatusRequestEntityTooLarge
	case "unknown_conflict", "stale_version", "paused":
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": p})
}
func (h *Hub) ServeUnix(ctx context.Context, path string) error {
	if !supportsPeerCredentials() {
		return errors.New("Unix peer UID authentication is supported only on Linux")
	}
	if os.Geteuid() == 0 {
		return errors.New("hub must run as an unprivileged service account")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	listener, err := listenUnix(path)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(path, 0660); err != nil {
		return err
	}
	server := &http.Server{Handler: h.APIHandler(), ReadHeaderTimeout: 5 * time.Second, ConnContext: func(ctx context.Context, c net.Conn) context.Context {
		uid, err := peerUID(c)
		if err != nil {
			return ctx
		}
		return context.WithValue(ctx, peerKey{}, uid)
	}}
	stopped := make(chan struct{})
	shutdownResult := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			shutdownResult <- server.Shutdown(shutdown)
		case <-stopped:
			shutdownResult <- nil
		}
	}()
	err = server.Serve(listener)
	close(stopped)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, <-shutdownResult)
}
