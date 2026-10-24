package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dots-lite/internal/store"
)

type recordArgs struct {
	TaskID   string          `json:"task_id"`
	Kind     string          `json:"kind"`
	Status   string          `json:"status"`
	Evidence json.RawMessage `json:"evidence"`
}

func (h *Hub) record(ctx context.Context, b Binding, inv *invocation, raw json.RawMessage) (json.RawMessage, error) {
	var a recordArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	var evidence map[string]json.RawMessage
	if err := json.Unmarshal(a.Evidence, &evidence); err != nil || len(evidence) == 0 {
		return nil, failure("missing_evidence", "a result record needs actual evidence")
	}
	var result any
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		if err := checkAPIAttempt(tx, b, inv.Source); err != nil {
			return err
		}
		task, err := loadTask(tx, b, a.TaskID)
		if err != nil {
			return err
		}
		switch a.Kind {
		case "verification":
			switch a.Status {
			case "pending", "passed", "failed":
			default:
				return failure("invalid_status", "unknown verification status")
			}
			if a.Status == "passed" {
				var checks []json.RawMessage
				if json.Unmarshal(evidence["checks"], &checks) != nil || len(checks) == 0 {
					return failure("missing_evidence", "passing verification requires observed checks")
				}
				for _, check := range checks {
					var object map[string]json.RawMessage
					if json.Unmarshal(check, &object) != nil || len(object) == 0 || !hasEvidence(check) {
						return failure("missing_evidence", "each observed check must include actual evidence")
					}
				}
			}
			task.Verification = a.Status
			task.Evidence = a.Evidence
		case "delivery":
			if a.Status == "delivered" {
				return failure("native_delivery_required", "only confirmed native outbox receipts may mark delivery complete")
			}
			switch a.Status {
			case "pending", "acquired", "delivered", "failed", "unknown":
			default:
				return failure("invalid_status", "unknown delivery status")
			}
			if a.Status == "delivered" && !hasEvidence(evidence["receipt"]) {
				return failure("missing_evidence", "delivery requires an actual native sender receipt")
			}
			task.Delivery = a.Status
		case "followup":
			if inv.Source.Kind != "api" || inv.TaskID != task.ID {
				return failure("unbound_source", "followup evidence requires its prebound API session")
			}
			var link apiLink
			if err = tx.Get("api_session", owner(b.scope()), inv.Source.ChatID, &link); err != nil {
				return err
			}
			var f followup
			if err = tx.Get("followup", owner(b.scope()), link.FollowupID, &f); err != nil {
				return err
			}
			if a.Status == "handled" {
				if !hasEvidence(evidence["actions"]) || !hasEvidence(evidence["verification"]) {
					return failure("missing_evidence", "handled followup needs actions and verification")
				}
				f.Status = "handled"
			} else if a.Status == "waiting_user" {
				f.Status = "blocked"
				task.Waiting = "user_decision"
				text := fmt.Sprintf("任务「%s」需要你的决定后继续。请在原聊天查看待处理事项。", task.Name)
				for _, key := range []string{"reason", "waiting_reason"} {
					var reason string
					if json.Unmarshal(evidence[key], &reason) == nil && strings.TrimSpace(reason) != "" {
						chars := []rune(reason)
						if len(chars) > 500 {
							chars = append(chars[:500], '…')
						}
						text += "\n" + string(chars)
						break
					}
				}
				if err = queueAttention(tx, b, task, "waiting|"+f.ID, text); err != nil {
					return err
				}
			} else {
				return failure("invalid_status", "followup status must be handled or waiting_user")
			}
			f.Evidence = a.Evidence
			if err = tx.Put("followup", owner(b.scope()), f.ID, f); err != nil {
				return err
			}
			result = f
		default:
			return failure("invalid_arguments", "unknown result record kind")
		}
		task.UpdatedAt = time.Now().UTC()
		if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
			return err
		}
		if a.Kind == "verification" && a.Status == "passed" {
			if err = queueFinalDeliveries(tx, b, task); err != nil {
				return err
			}
		}
		if result == nil {
			result = task.Task
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func hasEvidence(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return meaningful(value)
}
func meaningful(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		if len(v) == 0 {
			return false
		}
		for _, item := range v {
			if !meaningful(item) {
				return false
			}
		}
		return true
	case map[string]any:
		if len(v) == 0 {
			return false
		}
		for _, item := range v {
			if meaningful(item) {
				return true
			}
		}
		return false
	case bool:
		return v
	default:
		return true
	}
}
