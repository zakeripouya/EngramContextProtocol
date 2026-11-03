package store

import (
	"database/sql"
	"time"
)

// Permission requests seen by the permission assist (milestone M9), with what
// Engram predicted and what the user actually decided.

// PermitRequest is one logged permission request.
type PermitRequest struct {
	ID          int64   `json:"id"`
	At          string  `json:"at"`
	Client      string  `json:"client"`
	SessionID   string  `json:"sessionId"`
	Tool        string  `json:"tool"`
	InputHash   string  `json:"-"`
	Summary     string  `json:"summary"`
	Category    string  `json:"category"`
	Tier        string  `json:"tier"`      // eligible, never or unknown
	Predicted   string  `json:"predicted"` // allow or ask
	Confidence  float64 `json:"confidence"`
	Precedents  int     `json:"precedents"`
	Reason      string  `json:"reason"`
	Outcome     string  `json:"outcome"`             // pending, approved or denied
	Objective   string  `json:"objective,omitempty"` // ok or failed, once the tool ran
	ResolvedAt  string  `json:"resolvedAt,omitempty"`
	ResolvedHow string  `json:"resolvedHow,omitempty"` // tool-ran, session-moved-on, expired
}

// LogPermit stores a new pending request.
func (s *Store) LogPermit(r PermitRequest) (int64, error) {
	if r.At == "" {
		r.At = ts(Now())
	}
	res, err := s.db.Exec(`INSERT INTO permits(at, client, session_id, tool, input_hash, summary, category, tier, predicted,
		confidence, precedents, reason, outcome) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,'pending')`,
		r.At, r.Client, r.SessionID, r.Tool, r.InputHash, Short(r.Summary), r.Category, r.Tier, r.Predicted,
		r.Confidence, r.Precedents, r.Reason)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ResolvePermitRan marks the newest matching request approved because the tool ran.
// A request earlier inferred as denied is corrected. It reports whether one matched.
func (s *Store) ResolvePermitRan(client, session, tool, inputHash, objective string) (bool, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM permits WHERE client = ? AND session_id = ? AND tool = ? AND input_hash = ?
		AND (outcome = 'pending' OR resolved_how IN ('session-moved-on', 'expired')) AND at >= ?
		ORDER BY id DESC LIMIT 1`, client, session, tool, inputHash, ts(Now().Add(-24*time.Hour))).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	} else if err != nil {
		return false, err
	}
	_, err = s.db.Exec(`UPDATE permits SET outcome = 'approved', objective = ?, resolved_at = ?, resolved_how = 'tool-ran'
		WHERE id = ?`, nullable(objective), ts(Now()), id)
	return err == nil, err
}

// InferPermitDenials resolves pending requests the user did not approve: Claude Code
// fires no hook on a manual deny, so a request is denied when its session moved on
// (a later request or tool run) at least minAge after it, or when it is older than maxAge.
func (s *Store) InferPermitDenials(minAge, maxAge time.Duration) (int, error) {
	now := Now()
	res, err := s.db.Exec(`UPDATE permits SET outcome = 'denied', resolved_at = ?, resolved_how = 'session-moved-on'
		WHERE outcome = 'pending' AND at <= ? AND EXISTS (
		  SELECT 1 FROM permit_activity a WHERE a.client = permits.client AND a.session_id = permits.session_id AND a.at > permits.at
		    AND a.at >= strftime('%Y-%m-%dT%H:%M:%SZ', permits.at, '+' || ? || ' seconds'))`,
		ts(now), ts(now.Add(-minAge)), int(minAge.Seconds()))
	if err != nil {
		return 0, err
	}
	n1, _ := res.RowsAffected()
	res, err = s.db.Exec(`UPDATE permits SET outcome = 'denied', resolved_at = ?, resolved_how = 'expired'
		WHERE outcome = 'pending' AND at <= ?`, ts(now), ts(now.Add(-maxAge)))
	if err != nil {
		return int(n1), err
	}
	n2, _ := res.RowsAffected()
	return int(n1 + n2), nil
}

// NotePermitActivity records that a session did something (for denial inference).
func (s *Store) NotePermitActivity(client, session string) error {
	_, err := s.db.Exec(`INSERT INTO permit_activity(client, session_id, at) VALUES (?,?,?)`, client, session, ts(Now()))
	if err == nil {
		_, _ = s.db.Exec(`DELETE FROM permit_activity WHERE at < ?`, ts(Now().Add(-48*time.Hour)))
	}
	return err
}

// PermitPrecedents counts resolved requests in a category for a client.
func (s *Store) PermitPrecedents(client, category string) (approved, denied, failed int, err error) {
	err = s.db.QueryRow(`SELECT
		COALESCE(sum(outcome = 'approved'), 0), COALESCE(sum(outcome = 'denied'), 0),
		COALESCE(sum(outcome = 'approved' AND objective = 'failed'), 0)
		FROM permits WHERE client = ? AND category = ? AND outcome != 'pending'`, client, category).
		Scan(&approved, &denied, &failed)
	return
}

// Permits returns logged requests, newest first.
func (s *Store) Permits(limit int) ([]PermitRequest, error) {
	rows, err := s.db.Query(`SELECT id, at, client, session_id, tool, summary, category, tier, predicted, confidence,
		precedents, reason, outcome, COALESCE(objective,''), COALESCE(resolved_at,''), COALESCE(resolved_how,'')
		FROM permits ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PermitRequest{}
	for rows.Next() {
		var r PermitRequest
		if err := rows.Scan(&r.ID, &r.At, &r.Client, &r.SessionID, &r.Tool, &r.Summary, &r.Category, &r.Tier,
			&r.Predicted, &r.Confidence, &r.Precedents, &r.Reason, &r.Outcome, &r.Objective, &r.ResolvedAt,
			&r.ResolvedHow); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
