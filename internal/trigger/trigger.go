// Package trigger lets events start activity in Engram (milestone M8; design,
// "Triggers"): webhooks from services such as GitHub, and results of jobs and
// automations that already run elsewhere. Each event is observed as a system
// episode with its source as provenance, wired into the graph like any memory,
// and dispatched to subscribed agents whose context it is salient to, together
// with the related memories (tickets, decisions) those agents may read.
//
// Guardrails: triggers are created by the user through the CLI, authenticate
// every request, are rate-limited so a burst of events cannot become a burst of
// notifications, can be paused, and pass the same secret filter as memories.
package trigger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// RateLimit is the most events a trigger accepts per minute.
var RateLimit = 60

// FailureUrgency is added to the salience of failed events.
const FailureUrgency = 0.25

// Event is one thing that happened.
type Event struct {
	Trigger string
	Title   string // one line, such as "Job payouts failed (exit 1) in 3.2s"
	Body    string // details, truncated
	Status  string // ok or failed
	Tags    []string
	Key     string // stable id for idempotence (a delivery id); derived from the content if empty
}

// Result says what happened to an event.
type Result struct {
	MemoryID string               `json:"memoryId"`
	Created  bool                 `json:"created"`
	Notified []store.Notification `json:"notified"`
}

// ErrRateLimited is returned when a trigger exceeds RateLimit.
var ErrRateLimited = errors.New("rate limited")

// ErrPaused is returned when a trigger is paused.
var ErrPaused = errors.New("trigger is paused")

const maxBody = 1500

// Record observes an event and notifies subscribers.
func Record(st *store.Store, ev Event) (Result, error) {
	t, err := st.GetTrigger(ev.Trigger)
	if err != nil {
		return Result{}, err
	}
	if t.Paused {
		return Result{}, ErrPaused
	}
	n, err := st.TriggerEventsSince(t.Name, store.Now().Add(-time.Minute))
	if err != nil {
		return Result{}, err
	}
	if n >= RateLimit {
		_ = st.RecordTriggerEvent(store.TriggerEvent{Trigger: t.Name, Status: "rate-limited", Summary: ev.Title})
		return Result{}, ErrRateLimited
	}
	if ev.Status != "failed" {
		ev.Status = "ok"
	}
	content := strings.TrimSpace(ev.Title)
	if body := strings.TrimSpace(ev.Body); body != "" {
		if len(body) > maxBody {
			body = "…" + body[len(body)-maxBody:]
		}
		content += "\n" + body
	}
	if ev.Key == "" {
		h := sha256.Sum256([]byte(t.Name + "\x00" + content + "\x00" + store.Now().Format(time.RFC3339)))
		ev.Key = hex.EncodeToString(h[:12])
	}
	tags := append([]string{"trigger", t.Kind, ev.Status}, ev.Tags...)
	id, created, err := st.InsertSystem(store.SystemMemory{
		Key: "trigger:" + t.Name + ":" + ev.Key, Kind: "episode", Namespace: t.Namespace,
		Service: "trigger:" + t.Name, Content: content, Tags: tags, Strength: 0.4,
	})
	var e *ecp.Error
	if errors.As(err, &e) && e.Code == ecp.ContentRejected {
		_ = st.RecordTriggerEvent(store.TriggerEvent{Trigger: t.Name, Status: "rejected", Summary: ev.Title})
		return Result{}, err
	}
	if err != nil && id == "" {
		return Result{}, err
	}
	res := Result{MemoryID: id, Created: created, Notified: []store.Notification{}}
	if !created {
		return res, nil // a redelivery of an event already recorded
	}
	urgency := 0.0
	if ev.Status == "failed" {
		urgency = FailureUrgency
	}
	notes, err := st.Dispatch(id, urgency)
	if err != nil {
		return res, fmt.Errorf("recorded %s but could not notify: %w", id, err)
	}
	res.Notified = notes
	return res, st.RecordTriggerEvent(store.TriggerEvent{Trigger: t.Name, MemoryID: id, Status: ev.Status,
		Notified: len(notes), Summary: ev.Title})
}
