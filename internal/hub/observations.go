package hub

import (
	"context"
	"crypto/sha256"
	"dots-lite/internal/store"
	"encoding/hex"
	"encoding/json"
	"time"
)

type observation struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	TaskVersion int64     `json:"task_version"`
	Action      string    `json:"action"`
	Digest      string    `json:"sha256"`
	ObservedAt  time.Time `json:"observed_at"`
}

func (h *Hub) observedResult(ctx context.Context, b Binding, task taskRecord, id, action string, raw json.RawMessage) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil || task.ID == "" {
		return raw, nil
	}
	sum := sha256.Sum256(raw)
	obs := observation{ID: id, TaskID: task.ID, TaskVersion: task.Version, Action: action, Digest: hex.EncodeToString(sum[:]), ObservedAt: h.now().UTC()}
	if err := h.db.Update(ctx, func(tx *store.Tx) error { return tx.Put("observation", owner(b.scope()), id, obs) }); err != nil {
		return nil, err
	}
	object["_observation_id"], _ = json.Marshal(id)
	return json.Marshal(object)
}

// References bind the model's handling claim to actual query results or known
// execution receipts. They do not prove an arbitrary natural-language goal.
func checkEvidenceReferences(tx *store.Tx, b Binding, task taskRecord, f followup, raw json.RawMessage) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return failure("missing_evidence", "observed result references are required")
	}
	count := 0
	var visit func(any) error
	visit = func(value any) error {
		switch item := value.(type) {
		case []any:
			for _, child := range item {
				if err := visit(child); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, child := range item {
				if key == "observation_id" {
					id, ok := child.(string)
					if !ok {
						return failure("missing_evidence", "observation_id must identify an observed result")
					}
					var obs observation
					if err := tx.Get("observation", owner(b.scope()), id, &obs); err != nil {
						return failure("missing_evidence", "observation was not recorded for this user")
					}
					if obs.TaskID != task.ID || obs.TaskVersion != task.Version || obs.ObservedAt.Before(f.CreatedAt) {
						return failure("missing_evidence", "observation is stale or belongs to another task")
					}
					count++
				} else if key == "operation_id" {
					id, ok := child.(string)
					if !ok {
						return failure("missing_evidence", "operation_id must identify an accepted receipt")
					}
					op, err := loadOperation(tx, b, id)
					if err != nil {
						return err
					}
					if op.TaskID != task.ID || op.TaskVersion != task.Version || op.Stage != "accepted" || op.Result == nil || op.Result.Error != nil || op.UpdatedAt.Before(f.CreatedAt) {
						return failure("missing_evidence", "operation is stale, unknown, failed or belongs to another task")
					}
					count++
				} else if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(value); err != nil {
		return err
	}
	if count == 0 {
		return failure("missing_evidence", "handling requires actual observation_id or accepted operation_id references")
	}
	return nil
}
