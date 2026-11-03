package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/text"
)

// Push (milestone M8; spec, "Subscriptions and notifications"). An agent subscribes
// with a context. When an event (a trigger, a job result) is recorded, every live
// subscription it is salient to gets a notification carrying the event and the
// related memories the subscriber may read. Notifications are stored, so the
// process that records an event (a webhook listener, a cron job) and the process
// that serves the agent can be different; each serve process watches the table
// and pushes an MCP resource update.

// SubscriptionTTL is how long a subscription lives without a heartbeat from its server.
var SubscriptionTTL = 2 * time.Minute

// Subscription is one agent's standing interest.
type Subscription struct {
	ID         string   `json:"subscriptionId"`
	Client     string   `json:"client"`
	Context    string   `json:"context"`
	Tags       []string `json:"tags,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
	Threshold  float64  `json:"threshold"`
	CreatedAt  string   `json:"createdAt"`
}

// SubscribeInput is the input of Subscribe.
type SubscribeInput struct {
	Context    string
	Tags       []string
	Namespaces []string
	Threshold  float64 // 0..1, default 0.6
}

// Notification is one push to a subscriber.
type Notification struct {
	ID             int64    `json:"id"`
	SubscriptionID string   `json:"subscriptionId"`
	EventID        string   `json:"eventId"`
	Reason         string   `json:"reason"`
	Salience       float64  `json:"salience"`
	Memories       []Memory `json:"memories"`
	CreatedAt      string   `json:"createdAt"`
}

// Subscribe registers a client's interest in a context.
func (s *Store) Subscribe(client string, in SubscribeInput) (Subscription, error) {
	in.Context = strings.TrimSpace(in.Context)
	if in.Context == "" {
		return Subscription{}, ecp.Errorf(ecp.InvalidParams, "context is required")
	}
	if in.Threshold == 0 {
		in.Threshold = 0.6
	}
	if in.Threshold < 0 || in.Threshold > 1 {
		return Subscription{}, ecp.Errorf(ecp.InvalidParams, "threshold must be between 0 and 1")
	}
	readable, all, err := s.readable(client)
	if err != nil {
		return Subscription{}, err
	}
	for _, ns := range in.Namespaces {
		if !all && !contains(readable, ns) {
			return Subscription{}, ecp.Errorf(ecp.ScopeDenied, "client %q cannot read namespace %q", client, ns)
		}
	}
	sub := Subscription{ID: "sub_" + newID(), Client: client, Context: in.Context, Tags: dedupeSorted(in.Tags),
		Namespaces: dedupeSorted(in.Namespaces), Threshold: in.Threshold, CreatedAt: ts(Now())}
	tags, _ := json.Marshal(sub.Tags)
	nss, _ := json.Marshal(sub.Namespaces)
	_, err = s.db.Exec(`INSERT INTO subscriptions(id, client, context, tags, namespaces, threshold, created_at, heartbeat_at)
		VALUES (?,?,?,?,?,?,?,?)`, sub.ID, client, sub.Context, string(tags), string(nss), sub.Threshold, sub.CreatedAt, sub.CreatedAt)
	if err != nil {
		return Subscription{}, err
	}
	_ = audit(s.db, client, "subscribe", "", Short(sub.Context))
	return sub, nil
}

// Unsubscribe removes a client's subscription.
func (s *Store) Unsubscribe(client, id string) error {
	res, err := s.db.Exec(`DELETE FROM subscriptions WHERE id = ? AND client = ?`, id, client)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ecp.Errorf(ecp.InvalidParams, "no subscription %q for this client", id)
	}
	_, _ = s.db.Exec(`DELETE FROM subscription_sent WHERE subscription_id = ?`, id)
	return nil
}

// TouchSubscriptions renews the heartbeat of live subscriptions.
func (s *Store) TouchSubscriptions(ids []string) error {
	now := ts(Now())
	for _, id := range ids {
		if _, err := s.db.Exec(`UPDATE subscriptions SET heartbeat_at = ? WHERE id = ?`, now, id); err != nil {
			return err
		}
	}
	return nil
}

// DropSubscriptions removes subscriptions when their connection ends.
func (s *Store) DropSubscriptions(ids []string) {
	for _, id := range ids {
		_, _ = s.db.Exec(`DELETE FROM subscriptions WHERE id = ?`, id)
		_, _ = s.db.Exec(`DELETE FROM subscription_sent WHERE subscription_id = ?`, id)
	}
}

// Subscriptions lists live subscriptions.
func (s *Store) Subscriptions() ([]Subscription, error) {
	cutoff := ts(Now().Add(-SubscriptionTTL))
	rows, err := s.db.Query(`SELECT id, client, context, tags, namespaces, threshold, created_at FROM subscriptions
		WHERE heartbeat_at >= ? ORDER BY created_at`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		var tags, nss string
		if err := rows.Scan(&sub.ID, &sub.Client, &sub.Context, &tags, &nss, &sub.Threshold, &sub.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &sub.Tags)
		_ = json.Unmarshal([]byte(nss), &sub.Namespaces)
		out = append(out, sub)
	}
	return out, rows.Err()
}

func overlap(query, doc string) float64 {
	q := text.Tokenize(query)
	if len(q) == 0 {
		return 0
	}
	d := map[string]bool{}
	for _, t := range text.Tokenize(doc) {
		d[t] = true
	}
	set := map[string]bool{}
	hit := 0
	for _, t := range q {
		if set[t] {
			continue
		}
		set[t] = true
		if d[t] {
			hit++
		}
	}
	return float64(hit) / float64(len(set))
}

// Dispatch notifies every live subscription an event is salient to. urgency (0..1)
// raises salience, as for a failed job. It returns the notifications created.
func (s *Store) Dispatch(eventID string, urgency float64) ([]Notification, error) {
	var ev Memory
	mems, err := s.memoriesByID([]string{eventID})
	if err != nil {
		return nil, err
	}
	if len(mems) != 1 {
		return nil, ecp.Errorf(ecp.MemoryNotFound, "no memory %q", eventID)
	}
	ev = mems[0]
	subs, err := s.Subscriptions()
	if err != nil {
		return nil, err
	}
	var evVec []float32
	var out []Notification
	for _, sub := range subs {
		if ok, err := s.can(sub.Client, ev.Namespace, false); err != nil || !ok {
			continue // never notify about what the subscriber cannot read
		}
		if len(sub.Namespaces) > 0 && !contains(sub.Namespaces, ev.Namespace) {
			continue
		}
		rel := overlap(sub.Context, ev.Content+" "+strings.Join(ev.Tags, " "))
		if s.emb != nil {
			if evVec == nil {
				if v, err := s.emb.Embed(bgCtx(), []string{ev.Content}, embed.Document); err == nil && len(v) == 1 {
					evVec = v[0]
				}
			}
			if evVec != nil {
				if v, err := s.emb.Embed(context.Background(), []string{sub.Context}, embed.Query); err == nil && len(v) == 1 {
					rel = math.Max(rel, float64(embed.Cosine(evVec, v[0])))
				}
			}
		}
		why := []string{fmt.Sprintf("%.0f%% related to %q", rel*100, Short(sub.Context))}
		tagBonus := 0.0
		for _, t := range sub.Tags {
			if contains(ev.Tags, t) {
				tagBonus = 0.3
				why = append(why, "tag "+t)
				break
			}
		}
		if urgency > 0 {
			why = append(why, "urgent")
		}
		sal := math.Min(1, rel+tagBonus+urgency)
		if sal < sub.Threshold {
			continue
		}
		ids := []string{ev.ID}
		related, err := s.RecallQuiet(sub.Client, RecallInput{Query: ev.Content + " " + sub.Context, K: 6, Hops: 2})
		if err != nil {
			return nil, err
		}
		for _, r := range related {
			if r.Memory.ID != ev.ID && r.Memory.Provenance.Service != ev.Provenance.Service {
				ids = append(ids, r.Memory.ID)
			}
			if len(ids) == 6 {
				break
			}
		}
		fresh := ids[:0:0]
		for _, id := range ids {
			var n int
			if err := s.db.QueryRow(`SELECT count(*) FROM subscription_sent WHERE subscription_id = ? AND memory_id = ?`,
				sub.ID, id).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				fresh = append(fresh, id)
			}
		}
		if len(fresh) == 0 || fresh[0] != ev.ID {
			continue // the event itself was already sent for this subscription
		}
		reason := "Event " + Short(ev.Content) + " (" + strings.Join(why, ", ") + ")"
		idsJSON, _ := json.Marshal(fresh)
		now := ts(Now())
		tx, err := s.db.Begin()
		if err != nil {
			return nil, err
		}
		r, err := tx.Exec(`INSERT INTO notifications(subscription_id, client, event_id, memory_ids, reason, salience, created_at)
			VALUES (?,?,?,?,?,?,?)`, sub.ID, sub.Client, ev.ID, string(idsJSON), reason, round4(sal), now)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		for _, id := range fresh {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO subscription_sent(subscription_id, memory_id) VALUES (?,?)`, sub.ID, id); err != nil {
				tx.Rollback()
				return nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		nid, _ := r.LastInsertId()
		out = append(out, Notification{ID: nid, SubscriptionID: sub.ID, EventID: ev.ID, Reason: reason, Salience: round4(sal), CreatedAt: now})
	}
	return out, nil
}

// LatestNotification returns the newest notification id for the given subscriptions, or 0.
func (s *Store) LatestNotification(subIDs []string) (int64, error) {
	if len(subIDs) == 0 {
		return 0, nil
	}
	args := make([]any, len(subIDs))
	for i, id := range subIDs {
		args[i] = id
	}
	var id sql.NullInt64
	err := s.db.QueryRow(`SELECT max(id) FROM notifications WHERE read_at IS NULL AND subscription_id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...).Scan(&id)
	return id.Int64, err
}

// PendingNotifications returns a client's unread notifications with the memories it
// may still read, oldest first. markRead marks them delivered.
func (s *Store) PendingNotifications(client string, markRead bool) ([]Notification, error) {
	rows, err := s.db.Query(`SELECT id, subscription_id, event_id, memory_ids, reason, salience, created_at
		FROM notifications WHERE client = ? AND read_at IS NULL ORDER BY id LIMIT 50`, client)
	if err != nil {
		return nil, err
	}
	var out []Notification
	var raw [][]string
	for rows.Next() {
		var n Notification
		var ids string
		if err := rows.Scan(&n.ID, &n.SubscriptionID, &n.EventID, &ids, &n.Reason, &n.Salience, &n.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		var list []string
		_ = json.Unmarshal([]byte(ids), &list)
		out = append(out, n)
		raw = append(raw, list)
	}
	rows.Close()
	if len(out) == 0 {
		return []Notification{}, nil
	}
	readable, all, err := s.readable(client)
	if err != nil {
		return nil, err
	}
	for i := range out {
		mems, err := s.memoriesByID(raw[i])
		if err != nil {
			return nil, err
		}
		out[i].Memories = []Memory{}
		for _, m := range mems {
			if all || contains(readable, m.Namespace) { // grants may have changed since dispatch
				out[i].Memories = append(out[i].Memories, m)
			}
		}
	}
	if markRead {
		now := ts(Now())
		for _, n := range out {
			if _, err := s.db.Exec(`UPDATE notifications SET read_at = ? WHERE id = ?`, now, n.ID); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// memoriesByID loads memories without grant checks, skipping ids that no longer exist.
func (s *Store) memoriesByID(ids []string) ([]Memory, error) {
	out := make([]Memory, 0, len(ids))
	for _, id := range ids {
		m, err := scanMemory(s.db.QueryRow(`SELECT `+memoryColumns+` FROM memories WHERE id = ?`, id))
		if err == sql.ErrNoRows {
			continue
		} else if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
