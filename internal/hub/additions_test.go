package hub

import (
	"context"
	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func additionHub(t *testing.T) (*Hub, *store.DB, Binding, taskRecord) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	b := Binding{UID: 1001, UserID: "user", ProfileID: "profile", ExecutorID: "pc", Token: "fixture-token"}
	h, err := New(Config{Bindings: []Binding{b}}, db)
	if err != nil {
		t.Fatal(err)
	}
	h.sessions[b.ExecutorID] = &session{snapshot: protocol.Snapshot{Maintenance: protocol.Maintenance{Phase: "running"}}}
	task := taskRecord{Task: protocol.Task{ID: "task", Scope: b.scope(), Version: 1, Verification: "pending", Delivery: "pending", CreatedAt: time.Now().Add(-72 * time.Hour)}, RecoveryUsed: 100, RecoveryAttempts: 1000, RecoveryStartedAt: time.Now().Add(-72 * time.Hour)}
	if err := db.Update(context.Background(), func(tx *store.Tx) error { return tx.Put("task", owner(b.scope()), task.ID, task) }); err != nil {
		t.Fatal(err)
	}
	return h, db, b, task
}

func offerEvent(t *testing.T, h *Hub, db *store.DB, b Binding, task taskRecord, event protocol.Event) {
	t.Helper()
	if err := db.Update(context.Background(), func(tx *store.Tx) error {
		if err := tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
			return err
		}
		return h.createFollowup(tx, b, task, event)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNormalStagesAcrossOldTaskAgeAndMoreThanEightFollowupsHaveNoLifetimeLimit(t *testing.T) {
	h, db, b, task := additionHub(t)
	ctx := context.Background()
	simulated := time.Now().UTC()
	start := simulated
	h.now = func() time.Time { return simulated }
	for i := 1; i <= 12; i++ {
		simulated = start.Add(time.Duration(i) * 6 * time.Hour)
		task.TurnID = fmt.Sprintf("turn-%d", i)
		event := protocol.Event{Sequence: int64(i), Type: "task/quiescent", Data: json.RawMessage(`{"goal_status":"none","native_status":"idle"}`)}
		offerEvent(t, h, db, b, task, event)
		raw, err := h.NextFollowup(ctx, b.UID)
		if err != nil {
			t.Fatal(err)
		}
		var f followup
		if json.Unmarshal(raw, &f) != nil || f.ID == "" {
			t.Fatalf("normal stage %d was blocked by legacy age/count: %s", i, raw)
		}
		if _, err := h.SaveFollowupRun(ctx, b.UID, f.ID, mustRaw(map[string]string{"run_id": fmt.Sprintf("run-%d", i), "session_id": f.SessionID, "status": "completed"})); err != nil {
			t.Fatal(err)
		}
		inv := &invocation{Source: protocol.Source{Kind: "api", ChatID: f.SessionID}, TaskID: task.ID}
		observed := "observed-" + f.ID
		if err := db.Update(ctx, func(tx *store.Tx) error {
			return tx.Put("observation", owner(b.scope()), observed, observation{ID: observed, TaskID: task.ID, TaskVersion: task.Version, Action: "task.read", Digest: "fixture", ObservedAt: h.now().UTC()})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.record(ctx, b, inv, mustRaw(map[string]any{"task_id": task.ID, "kind": "followup", "status": "handled", "evidence": map[string]any{"actions": []any{map[string]string{"observation_id": observed}}, "verification": map[string]string{"observation_id": observed, "observed": "stage complete"}}})); err != nil {
			t.Fatal(err)
		}
		if err := db.Update(ctx, func(tx *store.Tx) error { var err error; task, err = loadTask(tx, b, task.ID); return err }); err != nil {
			t.Fatal(err)
		}
		// A second observation of the same stage has no new obligation.
		event.Sequence += 100
		offerEvent(t, h, db, b, task, event)
		if raw, err := h.NextFollowup(ctx, b.UID); err != nil || string(raw) != "null" {
			t.Fatalf("same stage woke model again: %s %v", raw, err)
		}
	}
	if task.RunsAccepted != 12 || task.RunsCompleted != 12 {
		t.Fatalf("native counts differ from admission and completion: %+v", task)
	}
}

func TestTransportUnavailableAndUserPauseDoNotCreateModelWork(t *testing.T) {
	h, db, b, task := additionHub(t)
	for i, reason := range []string{"state_unavailable", "goal_unavailable", "no_progress"} {
		offerEvent(t, h, db, b, task, protocol.Event{Sequence: int64(i + 1), Type: "task/reconcile", Data: mustRaw(map[string]string{"reason": reason})})
	}
	if raw, err := h.NextFollowup(context.Background(), b.UID); err != nil || string(raw) != "null" {
		t.Fatalf("poll-only outage started a model: %s %v", raw, err)
	}
	task.Paused = true
	offerEvent(t, h, db, b, task, protocol.Event{Sequence: 9, Type: "error", Data: json.RawMessage(`{}`)})
	if raw, err := h.NextFollowup(context.Background(), b.UID); err != nil || string(raw) != "null" {
		t.Fatalf("pause started a model: %s %v", raw, err)
	}
}

func TestWaitingNeedsProgramPredicateAndTerminalRunBeforeNewFollowup(t *testing.T) {
	h, db, b, task := additionHub(t)
	ctx := context.Background()
	offerEvent(t, h, db, b, task, protocol.Event{Sequence: 1, Type: "task/quiescent", Data: json.RawMessage(`{"goal_status":"none","native_status":"idle"}`)})
	raw, err := h.NextFollowup(ctx, b.UID)
	if err != nil {
		t.Fatal(err)
	}
	var f followup
	_ = json.Unmarshal(raw, &f)
	if _, err := h.SaveFollowupRun(ctx, b.UID, f.ID, mustRaw(map[string]string{"run_id": "run", "session_id": f.SessionID, "status": "running"})); err != nil {
		t.Fatal(err)
	}
	inv := &invocation{Source: protocol.Source{Kind: "api", ChatID: f.SessionID}, TaskID: task.ID}
	_, err = h.record(ctx, b, inv, mustRaw(map[string]any{"task_id": task.ID, "kind": "followup", "status": "waiting", "evidence": map[string]any{"waiting": map[string]any{"kind": "native_state", "check_at": time.Now().Add(time.Hour), "native_status": "idle", "goal_status": "none"}}}))
	if err != nil {
		t.Fatal(err)
	}
	wake := func(native string, terminal bool) {
		t.Helper()
		if err := db.Update(ctx, func(tx *store.Tx) error {
			var latest followup
			if err := tx.Get("followup", owner(b.scope()), f.ID, &latest); err != nil {
				return err
			}
			latest.Wait.CheckAt = time.Now().Add(-time.Hour)
			if terminal {
				latest.NativeStatus = "completed"
			}
			if err := tx.Put("followup", owner(b.scope()), f.ID, latest); err != nil {
				return err
			}
			return h.wakeWaiting(tx, b, task, native, "none", time.Now())
		}); err != nil {
			t.Fatal(err)
		}
	}
	wake("idle", false)
	wake("active", true)
	if raw, err := h.NextFollowup(ctx, b.UID); err != nil || string(raw) != "null" {
		t.Fatalf("false predicate or active prior run woke a model: %s %v", raw, err)
	}
	wake("idle", true)
	if raw, err := h.NextFollowup(ctx, b.UID); err != nil || string(raw) == "null" {
		t.Fatalf("verified wait never woke: %s %v", raw, err)
	}
}

func mustRaw(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestFaultIdentitySurvivesTurnChangesAndOnlyVerifiedRecoveryStartsANewCycle(t *testing.T) {
	h, db, b, task := additionHub(t)
	ctx := context.Background()
	var first, second string
	for i := 1; i <= 2; i++ {
		task.TurnID = fmt.Sprintf("turn-%d", i)
		offerEvent(t, h, db, b, task, protocol.Event{Sequence: int64(i), Type: "error", Data: mustRaw(map[string]string{"message": fmt.Sprintf("same failure with different wording %d", i)})})
	}
	if err := db.Update(ctx, func(tx *store.Tx) error {
		var f1, f2 followup
		if err := tx.Get("followup", owner(b.scope()), "event_1", &f1); err != nil {
			return err
		}
		if err := tx.Get("followup", owner(b.scope()), "event_2", &f2); err != nil {
			return err
		}
		first, second = f1.FaultID, f2.FaultID
		return closeFault(tx, b, first, "fixture actual recovery check")
	}); err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatalf("turn/prose changes reset failure scope: %s / %s", first, second)
	}
	task.TurnID = "turn-3"
	offerEvent(t, h, db, b, task, protocol.Event{Sequence: 3, Type: "error", Data: json.RawMessage(`{}`)})
	if err := db.Update(ctx, func(tx *store.Tx) error {
		var f followup
		if err := tx.Get("followup", owner(b.scope()), "event_3", &f); err != nil {
			return err
		}
		if f.FaultID == first {
			t.Fatal("verified recovery did not retain and advance the fault generation")
		}
		var history faultBudget
		if err := tx.Get("fault", owner(b.scope()), first, &history); err != nil {
			return err
		}
		if history.ClosedAt.IsZero() {
			t.Fatal("fault history was erased")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceEpochAndUserPausePersistAtHubAndOldFactsCannotReopenWrites(t *testing.T) {
	h, db, b, task := additionHub(t)
	ctx := context.Background()
	task.Paused = true
	task.Version++
	snapshot := protocol.Snapshot{InstanceID: "instance", Maintenance: protocol.Maintenance{Epoch: 1, Phase: "quiescing"}, Tasks: []protocol.Task{task.Task}}
	if err := h.saveEvent(ctx, b, protocol.Event{Sequence: 1, Type: "executor/maintenance", Data: mustRaw(snapshot)}); err != nil {
		t.Fatal(err)
	}
	if epoch, phase := h.maintenanceState(b); epoch != 1 || phase != "quiescing" {
		t.Fatalf("maintenance barrier lost: %d %s", epoch, phase)
	}
	if raw, err := h.NextFollowup(ctx, b.UID); err != nil || string(raw) != "null" {
		t.Fatalf("maintenance admitted a model run: %s %v", raw, err)
	}
	h2, err := New(Config{Bindings: []Binding{b}}, db)
	if err != nil {
		t.Fatal(err)
	}
	if epoch, phase := h2.maintenanceState(b); epoch != 1 || phase != "quiescing" {
		t.Fatalf("restart dropped maintenance: %d %s", epoch, phase)
	}
	snapshot.Maintenance = protocol.Maintenance{Epoch: 0, Phase: "running"}
	if err := h2.applySnapshot(ctx, b, snapshot); err == nil {
		t.Fatal("old hello reopened maintenance")
	}
	snapshot.Maintenance = protocol.Maintenance{Epoch: 2, Phase: "resuming"}
	if err := h2.applySnapshot(ctx, b, snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Maintenance.Phase = "running"
	if err := h2.applySnapshot(ctx, b, snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Maintenance = protocol.Maintenance{Epoch: 1, Phase: "stopping"}
	if err := h2.saveEvent(ctx, b, protocol.Event{Sequence: 2, Type: "executor/maintenance", Data: mustRaw(snapshot)}); err != nil {
		t.Fatal(err)
	}
	if epoch, phase := h2.maintenanceState(b); epoch != 2 || phase != "running" {
		t.Fatalf("old replay reverted resumed state: %d %s", epoch, phase)
	}
}

func TestHandlingRejectsInventedAndCrossTaskResultReferences(t *testing.T) {
	_, db, b, task := additionHub(t)
	ctx := context.Background()
	f := followup{CreatedAt: time.Now().Add(-time.Second)}
	if err := db.Update(ctx, func(tx *store.Tx) error {
		if err := checkEvidenceReferences(tx, b, task, f, json.RawMessage(`{"observation_id":"invented"}`)); err == nil {
			t.Fatal("invented proof was accepted")
		}
		obs := observation{ID: "observed", TaskID: "other", TaskVersion: task.Version, ObservedAt: time.Now()}
		if err := tx.Put("observation", owner(b.scope()), obs.ID, obs); err != nil {
			return err
		}
		if err := checkEvidenceReferences(tx, b, task, f, json.RawMessage(`{"observation_id":"observed"}`)); err == nil {
			t.Fatal("another task's proof was accepted")
		}
		obs.TaskID = task.ID
		if err := tx.Put("observation", owner(b.scope()), obs.ID, obs); err != nil {
			return err
		}
		return checkEvidenceReferences(tx, b, task, f, json.RawMessage(`{"observation_id":"observed"}`))
	}); err != nil {
		t.Fatal(err)
	}
}
