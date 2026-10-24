package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dots-lite/internal/hub"
	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestHubRejectsExecutorThatCannotHonorWorkingDirectories(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h, err := hub.New(hub.Config{Bindings: []hub.Binding{{
		UID: 101, UserID: "user", ProfileID: "profile", ExecutorID: "computer", Token: "executor-token",
	}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	server := httptest.NewServer(h.WebsocketHandler())
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/executor", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer executor-token"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	// Version 1 executors ignore the working_directory field and would execute
	// a subfolder task in the authorization root instead.
	if err := wsjson.Write(ctx, conn, protocol.Frame{Version: 1, Type: "hello", Snapshot: &protocol.Snapshot{InstanceID: "legacy-executor"}}); err != nil {
		t.Fatal(err)
	}
	var reply protocol.Frame
	err = wsjson.Read(ctx, conn, &reply)
	if err == nil {
		t.Fatalf("legacy executor accepted without working-directory support: %+v", reply)
	}
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("expected a protocol rejection, got %v", err)
	}
}
