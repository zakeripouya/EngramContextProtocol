package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
)

// Triggers let events start activity in Engram (milestone M8; design, "Triggers").
// A trigger is created by the user through the CLI and owns one namespace; its
// events are written as system episodes there, then dispatched to subscribers.

// Trigger kinds.
const (
	TriggerWebhook = "webhook" // generic JSON, bearer-token authenticated
	TriggerGitHub  = "github"  // GitHub webhooks, HMAC-SHA256 signed
	TriggerJob     = "job"     // results of scheduled jobs and automations
)

var triggerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Trigger is one event source.
type Trigger struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Namespace   string `json:"namespace"`
	Secret      string `json:"-"`
	Paused      bool   `json:"paused"`
	CreatedAt   string `json:"createdAt"`
	LastFiredAt string `json:"lastFiredAt,omitempty"`
	Fired       int    `json:"fired"`
	Failed      int    `json:"failed"`
}

// TriggerEvent is one recorded event.
type TriggerEvent struct {
	Trigger  string `json:"trigger"`
	At       string `json:"at"`
	MemoryID string `json:"memoryId,omitempty"`
	Status   string `json:"status"` // ok, failed, rejected, rate-limited
	Notified int    `json:"notified"`
	Summary  string `json:"summary"`
}

// AddTrigger creates a trigger and returns it with a fresh secret.
func (s *Store) AddTrigger(name, kind, namespace string) (Trigger, error) {
	if !triggerNameRE.MatchString(name) {
		return Trigger{}, fmt.Errorf("trigger names use lowercase letters, digits, dots, dashes and underscores")
	}
	if kind != TriggerWebhook && kind != TriggerGitHub && kind != TriggerJob {
		return Trigger{}, fmt.Errorf("kind must be webhook, github or job")
	}
	if namespace == "" {
		namespace = "flow/" + name
	}
	if !namespaceRE.MatchString(namespace) {
		return Trigger{}, fmt.Errorf("invalid namespace %q", namespace)
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return Trigger{}, err
	}
	t := Trigger{Name: name, Kind: kind, Namespace: namespace, Secret: hex.EncodeToString(b), CreatedAt: ts(Now())}
	_, err := s.db.Exec(`INSERT INTO triggers(name, kind, namespace, secret, paused, created_at) VALUES (?,?,?,?,0,?)`,
		t.Name, t.Kind, t.Namespace, t.Secret, t.CreatedAt)
	if err != nil {
		return Trigger{}, fmt.Errorf("trigger %q already exists", name)
	}
	_ = audit(s.db, "engram-cli", "trigger-add", namespace, name)
	return t, nil
}

// GetTrigger returns a trigger with its secret.
func (s *Store) GetTrigger(name string) (Trigger, error) {
	var t Trigger
	var paused int
	err := s.db.QueryRow(`SELECT t.name, t.kind, t.namespace, t.secret, t.paused, t.created_at,
		COALESCE((SELECT max(at) FROM trigger_events e WHERE e.trigger = t.name), ''),
		(SELECT count(*) FROM trigger_events e WHERE e.trigger = t.name AND e.status IN ('ok','failed')),
		(SELECT count(*) FROM trigger_events e WHERE e.trigger = t.name AND e.status = 'failed')
		FROM triggers t WHERE t.name = ?`, name).
		Scan(&t.Name, &t.Kind, &t.Namespace, &t.Secret, &paused, &t.CreatedAt, &t.LastFiredAt, &t.Fired, &t.Failed)
	if errors.Is(err, sql.ErrNoRows) {
		return Trigger{}, ecp.Errorf(ecp.MemoryNotFound, "no trigger %q", name)
	}
	t.Paused = paused == 1
	return t, err
}

// Triggers lists every trigger.
func (s *Store) Triggers() ([]Trigger, error) {
	rows, err := s.db.Query(`SELECT name FROM triggers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	out := make([]Trigger, 0, len(names))
	for _, n := range names {
		t, err := s.GetTrigger(n)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// RemoveTrigger deletes a trigger. Memories it recorded stay.
func (s *Store) RemoveTrigger(name string) error {
	res, err := s.db.Exec(`DELETE FROM triggers WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no trigger %q", name)
	}
	_ = audit(s.db, "engram-cli", "trigger-remove", "", name)
	return nil
}

// PauseTrigger pauses or resumes a trigger.
func (s *Store) PauseTrigger(name string, paused bool) error {
	v := 0
	if paused {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE triggers SET paused = ? WHERE name = ?`, v, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no trigger %q", name)
	}
	return nil
}

// RecordTriggerEvent logs an event for rate limiting and the dashboard.
func (s *Store) RecordTriggerEvent(e TriggerEvent) error {
	if e.At == "" {
		e.At = ts(Now())
	}
	_, err := s.db.Exec(`INSERT INTO trigger_events(trigger, at, memory_id, status, notified, summary) VALUES (?,?,?,?,?,?)`,
		e.Trigger, e.At, nullable(e.MemoryID), e.Status, e.Notified, Short(e.Summary))
	return err
}

// TriggerEventsSince counts accepted events for a trigger since a time.
func (s *Store) TriggerEventsSince(name string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM trigger_events WHERE trigger = ? AND at >= ? AND status IN ('ok','failed')`,
		name, ts(since)).Scan(&n)
	return n, err
}

// TriggerEvents returns recent events, newest first; name "" means all triggers.
func (s *Store) TriggerEvents(name string, limit int) ([]TriggerEvent, error) {
	q, args := `SELECT trigger, at, COALESCE(memory_id,''), status, notified, summary FROM trigger_events`, []any{}
	if name != "" {
		q += ` WHERE trigger = ?`
		args = append(args, name)
	}
	rows, err := s.db.Query(q+` ORDER BY at DESC, rowid DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TriggerEvent{}
	for rows.Next() {
		var e TriggerEvent
		if err := rows.Scan(&e.Trigger, &e.At, &e.MemoryID, &e.Status, &e.Notified, &e.Summary); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
