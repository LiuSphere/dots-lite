package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dots-lite/internal/protocol"
	"dots-lite/internal/store"
)

type deliveryTarget struct {
	Platform string `json:"platform"`
	ChatID   string `json:"chat_id"`
	ThreadID string `json:"thread_id,omitempty"`
}
type deliveryArtifact struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	LocalPath string `json:"local_path"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}
type delivery struct {
	ID             string            `json:"id"`
	TaskID         string            `json:"task_id"`
	TaskVersion    int64             `json:"task_version"`
	Source         protocol.Source   `json:"source"`
	Target         deliveryTarget    `json:"target"`
	Kind           string            `json:"kind"`
	Purpose        string            `json:"purpose"`
	Text           string            `json:"text,omitempty"`
	Artifact       *deliveryArtifact `json:"artifact,omitempty"`
	Status         string            `json:"status"`
	AttemptID      string            `json:"attempt_id,omitempty"`
	Attempts       int               `json:"attempts"`
	Retryable      bool              `json:"retryable"`
	NextCheckAt    time.Time         `json:"next_check_at"`
	CreatedAt      time.Time         `json:"created_at"`
	Receipt        json.RawMessage   `json:"receipt,omitempty"`
	Error          string            `json:"error,omitempty"`
	Report         string            `json:"report,omitempty"`
	AttemptHistory []json.RawMessage `json:"attempt_history,omitempty"`
}
type mediaVerification struct {
	MediaOnly   bool   `json:"media_only"`
	ArtifactID  string `json:"artifact_id"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	TargetExact bool   `json:"target_exact"`
}
type deliveryReport struct {
	AttemptID         string             `json:"attempt_id"`
	Status            string             `json:"status"`
	Receipt           json.RawMessage    `json:"receipt,omitempty"`
	Error             string             `json:"error,omitempty"`
	DefinitelyNotSent bool               `json:"definitely_not_sent,omitempty"`
	Verification      *mediaVerification `json:"verification,omitempty"`
}

func deliverySourceAllowed(b Binding, source protocol.Source) bool {
	if source.Kind != "gateway" || source.Platform == "" || source.ChatID == "" {
		return false
	}
	for _, allowed := range b.Sources {
		if allowed.Platform == source.Platform && allowed.ChatID == source.ChatID && allowed.SenderID == source.SenderID {
			return true
		}
	}
	return false
}
func queueDelivery(tx *store.Tx, b Binding, task taskRecord, key string, item delivery) error {
	if !deliverySourceAllowed(b, task.Source) {
		return failure("unbound_delivery", "task has no authorized original gateway destination")
	}
	sum := sha256.Sum256([]byte(task.ID + "|" + fmt.Sprint(task.Version) + "|" + key))
	item.ID = "delivery_" + hex.EncodeToString(sum[:])
	var old delivery
	if err := tx.Get("delivery", owner(b.scope()), item.ID, &old); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	item.TaskID, item.TaskVersion, item.Source = task.ID, task.Version, task.Source
	item.Target = deliveryTarget{Platform: task.Source.Platform, ChatID: task.Source.ChatID, ThreadID: task.Source.ThreadID}
	item.Status, item.CreatedAt = "pending", time.Now().UTC()
	return tx.Put("delivery", owner(b.scope()), item.ID, item)
}
func queueFinalDeliveries(tx *store.Tx, b Binding, task taskRecord) error {
	if task.Verification != "passed" {
		return nil
	}
	var evidence struct {
		Checks []json.RawMessage `json:"checks"`
	}
	if err := json.Unmarshal(task.Evidence, &evidence); err != nil {
		return err
	}
	text := fmt.Sprintf("任务「%s」的结果已通过 %d 项检查。", task.Name, len(evidence.Checks))
	for i, raw := range evidence.Checks {
		if i >= 3 {
			break
		}
		var check map[string]json.RawMessage
		if json.Unmarshal(raw, &check) != nil {
			continue
		}
		for _, key := range []string{"observed", "summary", "result", "command"} {
			var detail string
			if json.Unmarshal(check[key], &detail) == nil && strings.TrimSpace(detail) != "" {
				chars := []rune(detail)
				if len(chars) > 160 {
					chars = append(chars[:160], '…')
				}
				text += "\n" + string(chars)
				break
			}
		}
	}
	rows, err := tx.List("followup", owner(b.scope()))
	if err != nil {
		return err
	}
	for _, raw := range rows {
		var f followup
		if err = json.Unmarshal(raw, &f); err != nil {
			return err
		}
		if f.TaskID != task.ID || f.TaskVersion != task.Version || f.Status != "handled" {
			continue
		}
		var native struct {
			Output string `json:"output"`
		}
		if json.Unmarshal(f.NativeEvidence, &native) == nil && strings.TrimSpace(native.Output) != "" {
			output := []rune(native.Output)
			if len(output) > 600 {
				output = append(output[:600], []rune("…")...)
			}
			text += "\n" + string(output)
			break
		}
	}
	if err = queueDelivery(tx, b, task, "result|"+task.TurnID, delivery{Kind: "text", Purpose: "result", Text: text}); err != nil {
		return err
	}
	return queueAcquiredDeliveries(tx, b, task)
}
func queueAttention(tx *store.Tx, b Binding, task taskRecord, key, text string) error {
	return queueDelivery(tx, b, task, "attention|"+key, delivery{Kind: "text", Purpose: "attention", Text: text})
}

func queueRecoveryBudgetNotice(tx *store.Tx, b Binding, task taskRecord, latest followup) error {
	text := fmt.Sprintf("任务「%s」(%s) 的自动跟进已达到累计预算，已停止自动跟进。\n当前业务快照：执行=%s，核验=%s，交付=%s。\n已尝试原生跟进 %d 次、受理请求 %d 次；最近原生状态=%s。", task.Name, task.ID, task.Execution, task.Verification, task.Delivery, task.RecoveryUsed, task.RecoveryAttempts, latest.NativeStatus)
	bounded := func(value string) string {
		chars := []rune(value)
		if len(chars) > 500 {
			return string(chars[:500]) + "…"
		}
		return value
	}
	var native struct {
		Error  string `json:"error"`
		Output string `json:"output"`
	}
	if len(latest.NativeEvidence) > 0 {
		if err := json.Unmarshal(latest.NativeEvidence, &native); err != nil {
			return err
		}
	}
	if strings.TrimSpace(native.Error) != "" {
		text += "\n最近错误：" + bounded(native.Error)
	} else if strings.TrimSpace(native.Output) != "" {
		text += "\n最近原生报告（未获业务确认）：" + bounded(native.Output)
	} else {
		text += "\n尚未取得可核实的修复结果。"
	}
	var evidence struct {
		Actions json.RawMessage `json:"actions"`
	}
	if len(latest.Evidence) > 0 {
		if err := json.Unmarshal(latest.Evidence, &evidence); err != nil {
			return err
		}
	}
	if hasEvidence(evidence.Actions) {
		text += "\n已记录动作：" + bounded(string(evidence.Actions))
	}
	text += "\n请选择：暂停该任务，或保留当前执行状态并由你处理上述阻塞。你希望如何处理？"
	return queueAttention(tx, b, task, "recovery_budget", text)
}

func queueRequestNotice(tx *store.Tx, b Binding, task taskRecord, event protocol.Event, requestID string) error {
	if requestID == "" {
		return nil
	}
	kind := ""
	switch event.Type {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "execCommandApproval", "applyPatchApproval":
		kind = "审批"
	}
	if kind == "" {
		return nil
	}
	return queueAttention(tx, b, task, "request|"+requestID, fmt.Sprintf("任务「%s」需要你的%s。请在原聊天查看请求 %s 后确认；此通知不会批准或解决原生请求。", task.Name, kind, requestID))
}

func queueAcquiredDeliveries(tx *store.Tx, b Binding, task taskRecord) error {
	latest, err := latestDeliveryArtifacts(tx, b, task)
	if err != nil {
		return err
	}
	for _, path := range task.Artifacts {
		a, ok := latest[path]
		if !ok || a.Status != "acquired" {
			continue
		}
		var file filePayload
		if err = json.Unmarshal(a.File, &file); err != nil {
			return err
		}
		item := delivery{Kind: "artifact", Purpose: "result", Artifact: &deliveryArtifact{ID: a.ID, Path: file.Path, LocalPath: a.LocalPath, SHA256: file.SHA256, Size: file.Size}}
		if err = queueDelivery(tx, b, task, "artifact|"+a.ID, item); err != nil {
			return err
		}
	}
	return nil
}

func latestDeliveryArtifacts(tx *store.Tx, b Binding, task taskRecord) (map[string]artifact, error) {
	rows, err := tx.List("artifact", owner(b.scope()))
	if err != nil {
		return nil, err
	}
	latest := map[string]artifact{}
	for _, raw := range rows {
		var a artifact
		if err = json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		if a.TaskID != task.ID || a.TaskVersion != task.Version {
			continue
		}
		var file filePayload
		if err = json.Unmarshal(a.File, &file); err != nil {
			return nil, err
		}
		if old, ok := latest[file.Path]; !ok || a.Sequence > old.Sequence {
			latest[file.Path] = a
		}
	}
	return latest, nil
}

func (h *Hub) NextDelivery(ctx context.Context, uid int) (json.RawMessage, error) {
	b, err := h.binding(uid)
	if err != nil {
		return nil, err
	}
	var next *delivery
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := expireDeliveryAttempts(tx, b); err != nil {
			return err
		}
		rows, err := tx.List("delivery", owner(b.scope()))
		if err != nil {
			return err
		}
		var items []delivery
		for _, raw := range rows {
			var item delivery
			if err = json.Unmarshal(raw, &item); err != nil {
				return err
			}
			items = append(items, item)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
		for _, item := range items {
			if (item.Status != "pending" && !(item.Status == "failed" && item.Retryable)) || time.Now().Before(item.NextCheckAt) {
				continue
			}
			task, err := loadTask(tx, b, item.TaskID)
			if err != nil {
				return err
			}
			if task.Paused || task.Version != item.TaskVersion {
				continue
			}
			if item.Source != task.Source || !deliverySourceAllowed(b, task.Source) || !allowedFolder(b, task.FolderID) {
				item.Status = "blocked"
				item.Error = "original task delivery authorization changed"
				if err = tx.Put("delivery", owner(b.scope()), item.ID, item); err != nil {
					return err
				}
				continue
			}
			if item.Attempts >= 3 {
				item.Status = "blocked"
				item.Retryable = false
				item.Error = "cumulative delivery retry budget exhausted"
				if item.Purpose == "result" {
					task.Delivery = "failed"
					if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
						return err
					}
				}
				if err = tx.Put("delivery", owner(b.scope()), item.ID, item); err != nil {
					return err
				}
				continue
			}
			if item.Kind == "artifact" {
				latest, err := latestDeliveryArtifacts(tx, b, task)
				if err != nil {
					return err
				}
				if item.Artifact == nil {
					return failure("invalid_delivery", "artifact delivery has no bound file")
				}
				a := latest[item.Artifact.Path]
				if a.ID != item.Artifact.ID || a.Status != "acquired" || a.LocalPath != item.Artifact.LocalPath {
					item.Status = "superseded"
					item.Error = "a newer or unavailable artifact replaced this send unit"
					if err = tx.Put("delivery", owner(b.scope()), item.ID, item); err != nil {
						return err
					}
					continue
				}
			}
			item.AttemptID, err = randomID("send_")
			if err != nil {
				return err
			}
			item.Attempts++
			item.Status = "dispatching"
			// The native helper has a 120-second deadline. Missing receipts
			// become uncertain after a grace period, never eligible for replay.
			item.NextCheckAt = time.Now().UTC().Add(150 * time.Second)
			item.Retryable = false
			item.Report = ""
			item.Receipt = nil
			if err = tx.Put("delivery", owner(b.scope()), item.ID, item); err != nil {
				return err
			}
			if err = tx.Put("delivery_attempt", owner(b.scope()), item.AttemptID, item); err != nil {
				return err
			}
			next = &item
			break
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(next)
}

func expireDeliveryAttempts(tx *store.Tx, b Binding) error {
	rows, err := tx.List("delivery", owner(b.scope()))
	if err != nil {
		return err
	}
	for _, raw := range rows {
		var item delivery
		if err = json.Unmarshal(raw, &item); err != nil {
			return err
		}
		if item.Status != "dispatching" || item.NextCheckAt.IsZero() || time.Now().Before(item.NextCheckAt) {
			continue
		}
		item.Status, item.Retryable = "unknown", false
		item.Error = "native send receipt deadline expired; automatic replay is disabled"
		if err = tx.Put("delivery", owner(b.scope()), item.ID, item); err != nil {
			return err
		}
		if err = tx.Put("delivery_attempt", owner(b.scope()), item.AttemptID, item); err != nil {
			return err
		}
		if item.Purpose == "result" {
			task, err := loadTask(tx, b, item.TaskID)
			if err != nil {
				return err
			}
			if task.Version == item.TaskVersion {
				task.Delivery = "unknown"
				if err = tx.Put("task", owner(b.scope()), task.ID, task); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func validateDeliveryReceipt(item delivery, report deliveryReport) error {
	var receipt struct {
		Success        bool            `json:"success"`
		MessageID      string          `json:"message_id"`
		Platform       string          `json:"platform"`
		ChatID         string          `json:"chat_id"`
		ThreadID       string          `json:"thread_id"`
		Partial        json.RawMessage `json:"partial_success"`
		Dropped        json.RawMessage `json:"media_dropped"`
		Warnings       json.RawMessage `json:"warnings"`
		MediaDelivered json.RawMessage `json:"media_delivered"`
	}
	if json.Unmarshal(report.Receipt, &receipt) != nil || !receipt.Success || strings.TrimSpace(receipt.MessageID) == "" || hasEvidence(receipt.Partial) || hasEvidence(receipt.Dropped) || hasEvidence(receipt.Warnings) {
		return failure("invalid_receipt", "delivery needs a complete native success receipt and message ID")
	}
	if (receipt.Platform != "" && receipt.Platform != item.Target.Platform) || (receipt.ChatID != "" && receipt.ChatID != item.Target.ChatID) || (receipt.ThreadID != "" && receipt.ThreadID != item.Target.ThreadID) {
		return failure("binding_conflict", "native receipt destination differs from the original task")
	}
	if item.Kind == "artifact" {
		v := report.Verification
		if item.Artifact == nil || item.Target.Platform != "weixin" || v == nil || !v.MediaOnly || !v.TargetExact || v.ArtifactID != item.Artifact.ID || v.SHA256 != item.Artifact.SHA256 || v.Size != item.Artifact.Size {
			return failure("invalid_receipt", "artifact delivery needs verified single-media native sending for the bound file")
		}
		if len(receipt.MediaDelivered) > 0 {
			var delivered bool
			if json.Unmarshal(receipt.MediaDelivered, &delivered) != nil || !delivered {
				return failure("invalid_receipt", "native sender did not confirm media delivery")
			}
		}
	}
	return nil
}
func (h *Hub) ReportDelivery(ctx context.Context, uid int, id string, raw json.RawMessage) (json.RawMessage, error) {
	b, err := h.binding(uid)
	if err != nil {
		return nil, err
	}
	var report deliveryReport
	if err = decodeArgs(raw, &report); err != nil {
		return nil, err
	}
	fingerprint, err := canonical(raw)
	if err != nil {
		return nil, err
	}
	var item delivery
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.Get("delivery", owner(b.scope()), id, &item); err != nil {
			return err
		}
		if report.AttemptID == "" || report.AttemptID != item.AttemptID {
			return failure("binding_conflict", "delivery receipt does not match its persisted send attempt")
		}
		if item.Report == fingerprint {
			return nil
		}
		if item.Status != "dispatching" && item.Status != "unknown" {
			return failure("binding_conflict", "delivery attempt already has a different outcome")
		}
		switch report.Status {
		case "delivered":
			if err := validateDeliveryReceipt(item, report); err != nil {
				return err
			}
			item.Status = "delivered"
			item.Receipt = report.Receipt
			item.Error = ""
		case "failed", "unknown":
			if strings.TrimSpace(report.Error) == "" {
				return failure("invalid_arguments", "failed or unknown delivery needs observed error facts")
			}
			item.Status = "unknown"
			item.Error = report.Error
			item.Receipt = report.Receipt
			item.Retryable = false
			if report.Status == "failed" && report.DefinitelyNotSent {
				if len(report.Receipt) > 0 {
					var native struct {
						Success   bool            `json:"success"`
						MessageID string          `json:"message_id"`
						Partial   json.RawMessage `json:"partial_success"`
						Dropped   json.RawMessage `json:"media_dropped"`
						Warnings  json.RawMessage `json:"warnings"`
						Media     json.RawMessage `json:"media_delivered"`
					}
					if json.Unmarshal(report.Receipt, &native) != nil || native.Success || strings.TrimSpace(native.MessageID) != "" || hasEvidence(native.Partial) || hasEvidence(native.Dropped) || hasEvidence(native.Warnings) || hasEvidence(native.Media) {
						return failure("invalid_receipt", "a send or partial native receipt cannot be classified as definitely not sent")
					}
				}
				item.Status = "failed"
				item.Retryable = true
				item.NextCheckAt = time.Now().UTC()
			}
		default:
			return failure("invalid_arguments", "unknown delivery outcome")
		}
		item.Report = fingerprint
		if err = tx.Put("delivery", owner(b.scope()), item.ID, item); err != nil {
			return err
		}
		if err = tx.Put("delivery_attempt", owner(b.scope()), item.AttemptID, item); err != nil {
			return err
		}
		task, err := loadTask(tx, b, item.TaskID)
		if err != nil {
			return err
		}
		if task.Version == item.TaskVersion && item.Purpose == "result" {
			if item.Status == "unknown" {
				task.Delivery = "unknown"
			} else if item.Status == "failed" {
				task.Delivery = "failed"
			} else {
				complete, err := allResultDeliveriesSent(tx, b, task)
				if err != nil {
					return err
				}
				if complete {
					task.Delivery = "delivered"
				}
			}
			return tx.Put("task", owner(b.scope()), task.ID, task)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(item)
}
func allResultDeliveriesSent(tx *store.Tx, b Binding, task taskRecord) (bool, error) {
	rows, err := tx.List("delivery", owner(b.scope()))
	if err != nil {
		return false, err
	}
	found := false
	finalText := false
	sentArtifacts := map[string]bool{}
	for _, raw := range rows {
		var item delivery
		if err = json.Unmarshal(raw, &item); err != nil {
			return false, err
		}
		if item.TaskID == task.ID && item.TaskVersion == task.Version && item.Purpose == "result" {
			if item.Status == "superseded" {
				continue
			}
			found = true
			if item.Status != "delivered" {
				return false, nil
			}
			if item.Kind == "text" {
				finalText = true
			} else if item.Artifact != nil {
				sentArtifacts[item.Artifact.ID] = true
			}
		}
	}
	latest, err := latestDeliveryArtifacts(tx, b, task)
	if err != nil {
		return false, err
	}
	for _, path := range task.Artifacts {
		a := latest[path]
		if a.Status != "acquired" || !sentArtifacts[a.ID] {
			return false, nil
		}
	}
	return found && finalText, nil
}
func (h *Hub) deliveryList(ctx context.Context, b Binding, task taskRecord) (json.RawMessage, error) {
	items := []delivery{}
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		attemptRows, err := tx.List("delivery_attempt", owner(b.scope()))
		if err != nil {
			return err
		}
		history := map[string][]json.RawMessage{}
		for _, raw := range attemptRows {
			var attempt delivery
			if err = json.Unmarshal(raw, &attempt); err != nil {
				return err
			}
			if attempt.TaskID == task.ID {
				history[attempt.ID] = append(history[attempt.ID], raw)
			}
		}
		rows, err := tx.List("delivery", owner(b.scope()))
		if err != nil {
			return err
		}
		for _, raw := range rows {
			var item delivery
			if err = json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if item.TaskID == task.ID {
				item.AttemptHistory = history[item.ID]
				items = append(items, item)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(items)
}
