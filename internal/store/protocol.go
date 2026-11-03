package store

import (
	"container/heap"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
)

// Support for native ECP (milestone M11): an event log that lets every serving
// process push conflict and consolidation notifications whoever caused them,
// access tokens for HTTP clients, and path queries.

// ---------------------------------------------------------------- events

// Event is one entry in the event log.
type Event struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"` // conflict, consolidated
	Payload json.RawMessage `json:"payload"`
	At      string          `json:"at"`
}

// AddEvent appends to the event log.
func (s *Store) AddEvent(kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO events(kind, payload, at) VALUES (?,?,?)`, kind, string(b), ts(Now()))
	return err
}

// LatestEventID returns the newest event id, or 0.
func (s *Store) LatestEventID() (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRow(`SELECT max(id) FROM events`).Scan(&id)
	return id.Int64, err
}

// EventsAfter returns events newer than id, oldest first.
func (s *Store) EventsAfter(id int64, limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, kind, payload, at FROM events WHERE id > ? ORDER BY id LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var p string
		if err := rows.Scan(&e.ID, &e.Kind, &p, &e.At); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(p)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- tokens

// TokenInfo describes an access token (never its secret).
type TokenInfo struct {
	Prefix    string `json:"prefix"`
	Client    string `json:"client"`
	Service   bool   `json:"service"`
	CreatedAt string `json:"createdAt"`
	LastUsed  string `json:"lastUsedAt,omitempty"`
	Revoked   bool   `json:"revoked"`
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// CreateToken issues a bearer token for a client. Service tokens identify headless
// programs (cron jobs, CI, webhook bridges), whose writes default to author
// "system". The token is returned once; only its hash is stored. Like pairing,
// tokens are issued through the CLI, never over the protocol.
func (s *Store) CreateToken(client string, service bool) (string, error) {
	if strings.TrimSpace(client) == "" {
		return "", fmt.Errorf("client name is required")
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := "ecp_" + hex.EncodeToString(b)
	_, err := s.db.Exec(`INSERT INTO tokens(hash, prefix, client, service, created_at) VALUES (?,?,?,?,?)`,
		hashToken(tok), tok[:12], client, service, ts(Now()))
	if err != nil {
		return "", err
	}
	_ = audit(s.db, "engram-cli", "token-create", "", client)
	return tok, nil
}

// ErrBadToken is returned for unknown or revoked tokens.
var ErrBadToken = errors.New("unknown or revoked token")

// AuthToken resolves a bearer token to its client.
func (s *Store) AuthToken(tok string) (client string, service bool, err error) {
	if !strings.HasPrefix(tok, "ecp_") {
		return "", false, ErrBadToken
	}
	h := hashToken(tok)
	var revoked sql.NullString
	err = s.db.QueryRow(`SELECT client, service, revoked_at FROM tokens WHERE hash = ?`, h).Scan(&client, &service, &revoked)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && revoked.Valid) {
		return "", false, ErrBadToken
	}
	if err != nil {
		return "", false, err
	}
	_, _ = s.db.Exec(`UPDATE tokens SET last_used_at = ? WHERE hash = ?`, ts(Now()), h)
	return client, service, nil
}

// Tokens lists issued tokens.
func (s *Store) Tokens() ([]TokenInfo, error) {
	rows, err := s.db.Query(`SELECT prefix, client, service, created_at, COALESCE(last_used_at,''), revoked_at IS NOT NULL
		FROM tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.Prefix, &t.Client, &t.Service, &t.CreatedAt, &t.LastUsed, &t.Revoked); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeTokens revokes every token of a client, or the one with the given prefix.
func (s *Store) RevokeTokens(clientOrPrefix string) (int64, error) {
	res, err := s.db.Exec(`UPDATE tokens SET revoked_at = ? WHERE revoked_at IS NULL AND (client = ? OR prefix = ?)`,
		ts(Now()), clientOrPrefix, clientOrPrefix)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	_ = audit(s.db, "engram-cli", "token-revoke", "", clientOrPrefix)
	return n, nil
}

// ---------------------------------------------------------------- paths

// PathResult is the answer to a path query.
type PathResult struct {
	Path        []string `json:"path"`
	WeakestLink *Edge    `json:"weakestLink,omitempty"`
}

// pathKinds are the edges a chain of association may follow.
var pathKinds = []string{"similar", "coactive", "about", "derived", "supersedes"}

const maxPathNodes = 20000

type pathItem struct {
	id   string
	cost float64 // maximin: −bottleneck; most_probable: Σ −log w
}
type pathQueue []pathItem

func (q pathQueue) Len() int           { return len(q) }
func (q pathQueue) Less(i, j int) bool { return q[i].cost < q[j].cost }
func (q pathQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *pathQueue) Push(x any)        { *q = append(*q, x.(pathItem)) }
func (q *pathQueue) Pop() any {
	old := *q
	x := old[len(old)-1]
	*q = old[:len(old)-1]
	return x
}

// FindPath returns the best chain of associations between two memories the client may
// read. Rule "maximin" (default) maximizes the weakest link, since a chain of
// reasoning is only as strong as its weakest step; "most_probable" minimizes the
// sum of −log(weight). Every node on the path must be current and readable.
func (s *Store) FindPath(client, from, to, rule string) (PathResult, error) {
	if rule == "" {
		rule = "maximin"
	}
	if rule != "maximin" && rule != "most_probable" {
		return PathResult{}, ecp.Errorf(ecp.InvalidParams, "rule must be maximin or most_probable")
	}
	if _, err := s.Get(client, []string{from, to}); err != nil {
		return PathResult{}, err
	}
	readable, all, err := s.readable(client)
	if err != nil {
		return PathResult{}, err
	}
	canRead := func(ns string) bool { return all || contains(readable, ns) }
	res := PathResult{Path: []string{}}
	if from == to {
		res.Path = []string{from}
		return res, nil
	}
	best := map[string]float64{from: math.Inf(-1)}
	if rule == "most_probable" {
		best[from] = 0
	}
	prev := map[string]string{}
	prevEdge := map[string]Edge{}
	q := &pathQueue{{from, best[from]}}
	done := map[string]bool{}
	kinds := placeholders(len(pathKinds))
	for q.Len() > 0 && len(done) < maxPathNodes {
		cur := heap.Pop(q).(pathItem)
		if done[cur.id] {
			continue
		}
		done[cur.id] = true
		if cur.id == to {
			break
		}
		rows, err := s.db.Query(`SELECT e.src, e.dst, e.kind, e.weight, m.id, m.namespace FROM edges e
			JOIN memories m ON m.id = CASE WHEN e.src = ? THEN e.dst ELSE e.src END
			WHERE (e.src = ? OR e.dst = ?) AND e.kind IN (`+kinds+`) AND e.weight > 0 AND m.valid_to IS NULL`,
			append([]any{cur.id, cur.id, cur.id}, toAny(pathKinds)...)...)
		if err != nil {
			return PathResult{}, err
		}
		type step struct {
			e      Edge
			id, ns string
		}
		var steps []step
		for rows.Next() {
			var st step
			if err := rows.Scan(&st.e.From, &st.e.To, &st.e.Kind, &st.e.Weight, &st.id, &st.ns); err != nil {
				rows.Close()
				return PathResult{}, err
			}
			steps = append(steps, st)
		}
		rows.Close()
		for _, st := range steps {
			if done[st.id] || !canRead(st.ns) {
				continue
			}
			w := math.Min(1, st.e.Weight)
			var c float64
			if rule == "maximin" {
				bottleneck := -cur.cost // the weakest link so far
				if cur.id == from {
					bottleneck = math.Inf(1)
				}
				c = -math.Min(bottleneck, w)
			} else {
				c = cur.cost - math.Log(w)
			}
			if old, ok := best[st.id]; !ok || c < old {
				best[st.id] = c
				prev[st.id] = cur.id
				prevEdge[st.id] = st.e
				heap.Push(q, pathItem{st.id, c})
			}
		}
	}
	if _, ok := prev[to]; !ok {
		return res, nil // no chain of readable, current memories connects them
	}
	for n := to; ; n = prev[n] {
		res.Path = append([]string{n}, res.Path...)
		e := prevEdge[n]
		if res.WeakestLink == nil || e.Weight < res.WeakestLink.Weight {
			ee := e
			res.WeakestLink = &ee
		}
		if prev[n] == from {
			res.Path = append([]string{from}, res.Path...)
			break
		}
	}
	return res, nil
}
