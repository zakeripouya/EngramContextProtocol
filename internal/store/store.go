// Package store is Engram's persistence and access-control layer (milestone M1).
//
// One SQLite file holds memories, edges, client grants and an audit log.
// Every read and write is checked against the calling client's namespace grants,
// and every memory carries provenance and bitemporal validity
// (docs/design.md, "Memory model", "Time validity", "Namespaces and isolation").
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/text"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS memories (
  id                TEXT PRIMARY KEY,
  kind              TEXT NOT NULL,
  content           TEXT NOT NULL,
  tags              TEXT NOT NULL DEFAULT '[]',
  scope             TEXT NOT NULL,
  namespace         TEXT NOT NULL,
  strength          REAL NOT NULL DEFAULT 0.5,
  confidence        REAL,
  decision          TEXT,
  agent             TEXT NOT NULL,
  client            TEXT NOT NULL,
  service           TEXT,
  session_id        TEXT,
  author            TEXT NOT NULL,
  created_at        TEXT NOT NULL,
  valid_from        TEXT NOT NULL,
  valid_to          TEXT,
  last_activated_at TEXT,
  activations       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS memories_ns ON memories(namespace, valid_to);

CREATE TABLE IF NOT EXISTS edges (
  src    TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  dst    TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  kind   TEXT NOT NULL,
  weight REAL NOT NULL,
  PRIMARY KEY (src, dst, kind)
);

CREATE TABLE IF NOT EXISTS grants (
  client     TEXT NOT NULL,
  namespace  TEXT NOT NULL,
  can_read   INTEGER NOT NULL,
  can_write  INTEGER NOT NULL,
  expires_at TEXT,
  created_at TEXT NOT NULL,
  PRIMARY KEY (client, namespace)
);

CREATE TABLE IF NOT EXISTS audit (
  at        TEXT NOT NULL,
  client    TEXT NOT NULL,
  action    TEXT NOT NULL,
  namespace TEXT,
  detail    TEXT
);
`

// migrations upgrade a database step by step; PRAGMA user_version records the last one applied.
var migrations = []string{
	// 1 (M2): vectors, Hebbian bookkeeping and edge timestamps.
	`ALTER TABLE edges ADD COLUMN updated_at TEXT;
	CREATE INDEX IF NOT EXISTS edges_dst ON edges(dst, kind);
	CREATE TABLE IF NOT EXISTS vectors (
	  memory_id TEXT PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
	  model     TEXT NOT NULL,
	  dims      INTEGER NOT NULL,
	  vec       BLOB NOT NULL
	);
	CREATE TABLE IF NOT EXISTS coactivations (
	  a      TEXT NOT NULL,
	  b      TEXT NOT NULL,
	  client TEXT NOT NULL,
	  at     TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS coactivations_pair ON coactivations(a, b, client, at);`,

	// 2 (M3): keys for idempotent imports.
	`CREATE TABLE IF NOT EXISTS import_keys (
	  key       TEXT PRIMARY KEY,
	  memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE
	);`,

	// 3 (M4): feedback events, kept as training data for ranking and decision prediction.
	`CREATE TABLE IF NOT EXISTS feedback (
	  at        TEXT NOT NULL,
	  client    TEXT NOT NULL,
	  memory_id TEXT NOT NULL,
	  outcome   TEXT NOT NULL,
	  objective TEXT,
	  query     TEXT
	);
	CREATE INDEX IF NOT EXISTS feedback_memory ON feedback(memory_id);`,

	// 4 (M5): settings, sleep runs and an undo log of every change a sleep cycle makes.
	`CREATE TABLE IF NOT EXISTS settings (
	  key   TEXT PRIMARY KEY,
	  value TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS sleep_runs (
	  id          INTEGER PRIMARY KEY AUTOINCREMENT,
	  started_at  TEXT NOT NULL,
	  finished_at TEXT,
	  light       INTEGER NOT NULL DEFAULT 0,
	  snapshot    TEXT,
	  report      TEXT
	);
	CREATE TABLE IF NOT EXISTS sleep_changes (
	  id        INTEGER PRIMARY KEY AUTOINCREMENT,
	  run_id    INTEGER NOT NULL,
	  kind      TEXT NOT NULL,
	  memory_id TEXT NOT NULL,
	  other_id  TEXT,
	  detail    TEXT,
	  undone_at TEXT
	);`,

	// 5 (M6): nightly health metrics and the user's golden recall suite.
	`CREATE TABLE IF NOT EXISTS health (
	  at      TEXT NOT NULL,
	  metrics TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS golden (
	  id        INTEGER PRIMARY KEY AUTOINCREMENT,
	  query     TEXT NOT NULL,
	  expect_id TEXT NOT NULL,
	  namespace TEXT NOT NULL,
	  added_at  TEXT NOT NULL
	);`,
	// 6 (M7): sessions and handoff.
	`CREATE TABLE IF NOT EXISTS sessions (
	  id           TEXT PRIMARY KEY,
	  namespace    TEXT NOT NULL,
	  status       TEXT NOT NULL,
	  parent_id    TEXT,
	  delegated_to TEXT,
	  goal         TEXT NOT NULL,
	  state        TEXT NOT NULL,
	  created_at   TEXT NOT NULL,
	  updated_at   TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS sessions_parent ON sessions(parent_id);
	CREATE INDEX IF NOT EXISTS sessions_ns ON sessions(namespace, status);
	CREATE INDEX IF NOT EXISTS memories_session ON memories(session_id);`,
	// 7 (M8): attend conflicts, subscriptions, notifications and triggers.
	`CREATE TABLE IF NOT EXISTS subscriptions (
	  id           TEXT PRIMARY KEY,
	  client       TEXT NOT NULL,
	  context      TEXT NOT NULL,
	  tags         TEXT NOT NULL,
	  namespaces   TEXT NOT NULL,
	  threshold    REAL NOT NULL,
	  created_at   TEXT NOT NULL,
	  heartbeat_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS subscription_sent (
	  subscription_id TEXT NOT NULL,
	  memory_id       TEXT NOT NULL,
	  PRIMARY KEY (subscription_id, memory_id)
	);
	CREATE TABLE IF NOT EXISTS notifications (
	  id              INTEGER PRIMARY KEY AUTOINCREMENT,
	  subscription_id TEXT NOT NULL,
	  client          TEXT NOT NULL,
	  event_id        TEXT NOT NULL,
	  memory_ids      TEXT NOT NULL,
	  reason          TEXT NOT NULL,
	  salience        REAL NOT NULL,
	  created_at      TEXT NOT NULL,
	  read_at         TEXT
	);
	CREATE INDEX IF NOT EXISTS notifications_client ON notifications(client, read_at);
	CREATE TABLE IF NOT EXISTS triggers (
	  name       TEXT PRIMARY KEY,
	  kind       TEXT NOT NULL,
	  namespace  TEXT NOT NULL,
	  secret     TEXT NOT NULL,
	  paused     INTEGER NOT NULL DEFAULT 0,
	  created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS trigger_events (
	  trigger   TEXT NOT NULL,
	  at        TEXT NOT NULL,
	  memory_id TEXT,
	  status    TEXT NOT NULL,
	  notified  INTEGER NOT NULL DEFAULT 0,
	  summary   TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS trigger_events_by ON trigger_events(trigger, at);`,
}

// Accepted values (spec, "Data types").
var (
	ObservableKinds = map[string]bool{"episode": true, "decision": true, "suggestion": true, "fact": true}
	Authors         = map[string]bool{"user": true, "agent": true, "system": true}
	Scopes          = map[string]bool{"private": true, "team": true, "public": true}
	namespaceRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)
)

// maxCandidates bounds how many memories M1 ranks per recall. Embedding search in M2 removes this limit.
const maxCandidates = 5000

// Now is the clock; tests may replace it.
var Now = func() time.Time { return time.Now().UTC() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ParseTime accepts an RFC 3339 time or a YYYY-MM-DD date and returns it in UTC.
func ParseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, ecp.Errorf(ecp.InvalidParams, "not an ISO 8601 date or time: %q", s)
}

// Decision is the structured part of a decision memory.
type Decision struct {
	Situation string   `json:"situation"`
	Options   []string `json:"options,omitempty"`
	Choice    string   `json:"choice"`
	Rationale string   `json:"rationale,omitempty"`
}

// Provenance records who wrote a memory and from where.
type Provenance struct {
	Agent     string `json:"agent"`
	Client    string `json:"client"`
	Service   string `json:"service,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Author    string `json:"author"`
	At        string `json:"at"`
}

// Memory is one node in the graph, as returned to clients.
type Memory struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`
	Content         string     `json:"content"`
	Tags            []string   `json:"tags"`
	Scope           string     `json:"scope"`
	Namespace       string     `json:"namespace"`
	Strength        float64    `json:"strength"`
	Confidence      *float64   `json:"confidence,omitempty"`
	Decision        *Decision  `json:"decision,omitempty"`
	Provenance      Provenance `json:"provenance"`
	CreatedAt       string     `json:"createdAt"`
	ValidFrom       string     `json:"validFrom"`
	ValidTo         string     `json:"validTo,omitempty"`
	LastActivatedAt string     `json:"lastActivatedAt,omitempty"`
}

// Scored is a recall result. Path is set when the memory was reached through a pathway.
type Scored struct {
	Memory     Memory   `json:"memory"`
	Score      float64  `json:"score"`
	Confidence float64  `json:"confidence"`
	Reason     string   `json:"reason"`
	Path       []string `json:"path,omitempty"`
}

// Grant is one client's access to one namespace.
type Grant struct {
	Client    string
	Namespace string
	CanRead   bool
	CanWrite  bool
	ExpiresAt string
}

// ObserveInput is a write request.
type ObserveInput struct {
	Kind       string
	Content    string
	Namespace  string
	Tags       []string
	Scope      string
	Author     string
	Agent      string
	Decision   *Decision
	Service    string
	SessionID  string
	ValidFrom  string
	Supersedes string
}

// RecallInput is a read request.
type RecallInput struct {
	Query      string
	K          int
	Kinds      []string
	Tags       []string
	Namespaces []string
	AsOf       string
	Hops       int // spreading-activation hops, 0 to 3: 0 = direct matches only (the server defaults to 2)
}

// Store is one Engram instance.
type Store struct {
	db    *sql.DB
	emb   embed.Embedder
	learn Learning
	path  string
}

// Open opens (creating if needed) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		dsn = "file:mem_" + newID() + "?mode=memory&cache=shared"
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	st := &Store{db: db, learn: DefaultLearning}
	if path != ":memory:" {
		st.path = path
	}
	return st, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			return err
		}
	}
	return nil
}

// SetLearning replaces the learning parameters (for tuning and tests).
func (s *Store) SetLearning(l Learning) { s.learn = l }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validNamespace(ns string) bool { return ns == "*" || namespaceRE.MatchString(ns) }

// ---------------------------------------------------------------- grants

// Pair grants a client access. It is called by the user through the CLI, never over the protocol,
// so that no agent can grant itself access (spec, "Pairing").
func (s *Store) Pair(client string, read, write []string, expiresAt string) error {
	if client == "" {
		return fmt.Errorf("client name is required")
	}
	all := map[string]bool{}
	for _, ns := range append(append([]string{}, read...), write...) {
		if !validNamespace(ns) {
			return fmt.Errorf("invalid namespace %q", ns)
		}
		all[ns] = true
	}
	if expiresAt != "" {
		t, err := ParseTime(expiresAt)
		if err != nil {
			return err
		}
		expiresAt = ts(t)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for ns := range all {
		_, err := tx.Exec(`INSERT INTO grants(client, namespace, can_read, can_write, expires_at, created_at)
			VALUES (?,?,?,?,?,?) ON CONFLICT(client, namespace) DO UPDATE SET
			can_read=excluded.can_read, can_write=excluded.can_write, expires_at=excluded.expires_at`,
			client, ns, contains(read, ns), contains(write, ns), nullable(expiresAt), ts(Now()))
		if err != nil {
			return err
		}
	}
	detail, _ := json.Marshal(map[string]any{"read": read, "write": write, "expires": expiresAt})
	if err := audit(tx, client, "pair", "", string(detail)); err != nil {
		return err
	}
	return tx.Commit()
}

// Revoke removes all of a client's grants and returns how many were removed.
func (s *Store) Revoke(client string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM grants WHERE client = ?`, client)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	_ = audit(s.db, client, "revoke", "", "")
	return n, nil
}

// Grants lists grants, for one client or (client == "") for all.
func (s *Store) Grants(client string) ([]Grant, error) {
	q, args := `SELECT client, namespace, can_read, can_write, COALESCE(expires_at,'') FROM grants`, []any{}
	if client != "" {
		q += ` WHERE client = ?`
		args = append(args, client)
	}
	rows, err := s.db.Query(q+` ORDER BY client, namespace`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.Client, &g.Namespace, &g.CanRead, &g.CanWrite, &g.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) activeGrants(client string) ([]Grant, error) {
	all, err := s.Grants(client)
	if err != nil {
		return nil, err
	}
	now := ts(Now())
	var out []Grant
	for _, g := range all {
		if g.ExpiresAt == "" || g.ExpiresAt > now {
			out = append(out, g)
		}
	}
	if client == "" || len(out) == 0 {
		return nil, ecp.Errorf(ecp.Unauthorized,
			"client %q is not paired. The user can approve it from a terminal: engram pair %s --read me --write me",
			client, client)
	}
	return out, nil
}

func (s *Store) can(client, namespace string, write bool) (bool, error) {
	grants, err := s.activeGrants(client)
	if err != nil {
		return false, err
	}
	for _, g := range grants {
		if g.Namespace == "*" || g.Namespace == namespace {
			if (write && g.CanWrite) || (!write && g.CanRead) {
				return true, nil
			}
		}
	}
	return false, nil
}

// readable returns the namespaces a client may read; nil with all=true means every namespace.
func (s *Store) readable(client string) (nss []string, all bool, err error) {
	grants, err := s.activeGrants(client)
	if err != nil {
		return nil, false, err
	}
	set := map[string]bool{}
	for _, g := range grants {
		if !g.CanRead {
			continue
		}
		if g.Namespace == "*" {
			return nil, true, nil
		}
		set[g.Namespace] = true
	}
	for ns := range set {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	return nss, false, nil
}

// ---------------------------------------------------------------- writes

// ObserveResult is returned by Observe.
type ObserveResult struct {
	ID       string `json:"id"`
	Linked   int    `json:"linked"`
	Embedded bool   `json:"embedded"`
}

// Observe stores a memory after validating it, checking grants and filtering secrets.
func (s *Store) Observe(client string, in ObserveInput) (ObserveResult, error) {
	if in.Namespace == "" {
		in.Namespace = "me"
	}
	if in.Scope == "" {
		in.Scope = "private"
	}
	if in.Author == "" {
		in.Author = "agent"
	}
	in.Content = strings.TrimSpace(in.Content)
	switch {
	case !ObservableKinds[in.Kind]:
		return ObserveResult{}, ecp.Errorf(ecp.InvalidParams, "kind must be episode, fact, suggestion or decision")
	case !Authors[in.Author]:
		return ObserveResult{}, ecp.Errorf(ecp.InvalidParams, "author must be user, agent or system")
	case !Scopes[in.Scope]:
		return ObserveResult{}, ecp.Errorf(ecp.InvalidParams, "scope must be private, team or public")
	case in.Namespace == "*" || !validNamespace(in.Namespace):
		return ObserveResult{}, ecp.Errorf(ecp.InvalidParams, "invalid namespace %q", in.Namespace)
	case in.Content == "":
		return ObserveResult{}, ecp.Errorf(ecp.InvalidParams, "content is empty")
	}
	if in.Kind == "decision" {
		if in.Author != "user" {
			return ObserveResult{}, ecp.Errorf(ecp.InvalidProvenance,
				"a decision must have author \"user\"; record agent proposals as kind \"suggestion\"")
		}
		if in.Decision == nil || in.Decision.Situation == "" || in.Decision.Choice == "" {
			return ObserveResult{}, ecp.Errorf(ecp.InvalidParams, "a decision needs decision.situation and decision.choice")
		}
	}
	ok, err := s.can(client, in.Namespace, true)
	if err != nil {
		return ObserveResult{}, err
	}
	if !ok {
		return ObserveResult{}, ecp.Errorf(ecp.ScopeDenied, "client %q cannot write to namespace %q", client, in.Namespace)
	}
	decisionJSON := ""
	if in.Decision != nil {
		b, _ := json.Marshal(in.Decision)
		decisionJSON = string(b)
	}
	if hit := FindSensitive(strings.Join([]string{in.Content, decisionJSON, strings.Join(in.Tags, " ")}, " ")); hit != "" {
		_ = audit(s.db, client, "reject", in.Namespace, hit)
		return ObserveResult{}, ecp.Errorf(ecp.ContentRejected, "content looks like it contains a %s; nothing was stored", hit)
	}

	now := Now()
	validFrom := now
	if in.ValidFrom != "" {
		if validFrom, err = ParseTime(in.ValidFrom); err != nil {
			return ObserveResult{}, err
		}
	}
	tags := dedupeSorted(in.Tags)
	tagsJSON, _ := json.Marshal(tags)
	strength := 0.5
	if in.Kind == "decision" {
		strength = 0.8
	}
	agent := in.Agent
	if agent == "" {
		agent = client
	}
	id := "mem_" + newID()

	// Checks that read the database happen before the write transaction starts:
	// an in-memory store has a single connection, so reading inside the
	// transaction would wait on itself.
	if in.Supersedes != "" {
		var ns string
		err := s.db.QueryRow(`SELECT namespace FROM memories WHERE id = ?`, in.Supersedes).Scan(&ns)
		if err == sql.ErrNoRows {
			return ObserveResult{}, ecp.Errorf(ecp.MemoryNotFound, "no memory %q", in.Supersedes)
		} else if err != nil {
			return ObserveResult{}, err
		}
		if ok, err := s.can(client, ns, true); err != nil || !ok {
			return ObserveResult{}, ecp.Errorf(ecp.ScopeDenied, "cannot supersede a memory in namespace %q", ns)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return ObserveResult{}, err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO memories(id, kind, content, tags, scope, namespace, strength, decision,
		agent, client, service, session_id, author, created_at, valid_from) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, in.Kind, in.Content, string(tagsJSON), in.Scope, in.Namespace, strength, nullable(decisionJSON),
		agent, client, nullable(in.Service), nullable(in.SessionID), in.Author, ts(now), ts(validFrom))
	if err != nil {
		return ObserveResult{}, err
	}
	linked := 0
	if in.Supersedes != "" {
		if _, err := tx.Exec(`UPDATE memories SET valid_to = ? WHERE id = ? AND valid_to IS NULL`, ts(validFrom), in.Supersedes); err != nil {
			return ObserveResult{}, err
		}
		if _, err := tx.Exec(`INSERT INTO edges(src, dst, kind, weight) VALUES (?,?,'supersedes',1.0)`, id, in.Supersedes); err != nil {
			return ObserveResult{}, err
		}
		linked++
	}
	if err := audit(tx, client, "observe", in.Namespace, id); err != nil {
		return ObserveResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ObserveResult{}, err
	}
	n, embedded, err := s.embedAndLink(context.Background(), id, in.Namespace, in.Content)
	if err != nil {
		return ObserveResult{}, err
	}
	return ObserveResult{ID: id, Linked: linked + n, Embedded: embedded}, nil
}

// AddEdge inserts or updates an edge. Used by later milestones and tests.
func (s *Store) AddEdge(src, dst, kind string, weight float64) error {
	weight = max(0, min(1, weight))
	_, err := s.db.Exec(`INSERT INTO edges(src, dst, kind, weight) VALUES (?,?,?,?)
		ON CONFLICT(src, dst, kind) DO UPDATE SET weight = excluded.weight`, src, dst, kind, weight)
	return err
}

// Forget deletes memories; with cascade it also deletes nodes derived only from forgotten ones.
func (s *Store) Forget(client string, ids []string, cascade bool) (int, error) {
	for _, id := range ids {
		var ns string
		err := s.db.QueryRow(`SELECT namespace FROM memories WHERE id = ?`, id).Scan(&ns)
		if err == sql.ErrNoRows {
			return 0, ecp.Errorf(ecp.MemoryNotFound, "no memory %q", id)
		} else if err != nil {
			return 0, err
		}
		if ok, err := s.can(client, ns, true); err != nil {
			return 0, err
		} else if !ok {
			return 0, ecp.Errorf(ecp.ScopeDenied, "cannot forget a memory in namespace %q", ns)
		}
	}
	doomed := map[string]bool{}
	for _, id := range ids {
		doomed[id] = true
	}
	if cascade {
		parents := map[string][]string{}
		rows, err := s.db.Query(`SELECT src, dst FROM edges WHERE kind = 'derived'`)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var src, dst string
			if err := rows.Scan(&src, &dst); err != nil {
				rows.Close()
				return 0, err
			}
			parents[src] = append(parents[src], dst)
		}
		rows.Close()
		for changed := true; changed; {
			changed = false
			for child, ps := range parents {
				if doomed[child] {
					continue
				}
				all := true
				for _, p := range ps {
					if !doomed[p] {
						all = false
						break
					}
				}
				if all {
					doomed[child] = true
					changed = true
				}
			}
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	removed := 0
	list := make([]string, 0, len(doomed))
	for id := range doomed {
		res, err := tx.Exec(`DELETE FROM memories WHERE id = ?`, id)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		removed += int(n)
		list = append(list, id)
	}
	sort.Strings(list)
	detail, _ := json.Marshal(list)
	if err := audit(tx, client, "forget", "", string(detail)); err != nil {
		return 0, err
	}
	return removed, tx.Commit()
}

// ---------------------------------------------------------------- reads

const memoryColumns = `id, kind, content, tags, scope, namespace, strength, confidence, COALESCE(decision,''),
	agent, client, COALESCE(service,''), COALESCE(session_id,''), author, created_at, valid_from,
	COALESCE(valid_to,''), COALESCE(last_activated_at,'')`

// memoryColumnsM is memoryColumns qualified with the alias m, for joins.
const memoryColumnsM = `m.id, m.kind, m.content, m.tags, m.scope, m.namespace, m.strength, m.confidence,
	COALESCE(m.decision,''), m.agent, m.client, COALESCE(m.service,''), COALESCE(m.session_id,''), m.author,
	m.created_at, m.valid_from, COALESCE(m.valid_to,''), COALESCE(m.last_activated_at,'')`

type scanner interface{ Scan(dest ...any) error }

func scanMemory(r scanner) (Memory, error) { return scanMemoryWith(r) }

// scanMemoryWith scans the memory columns followed by any extra destinations.
func scanMemoryWith(r scanner, extra ...any) (Memory, error) {
	var m Memory
	var tags, decision string
	var conf sql.NullFloat64
	dest := []any{&m.ID, &m.Kind, &m.Content, &tags, &m.Scope, &m.Namespace, &m.Strength, &conf, &decision,
		&m.Provenance.Agent, &m.Provenance.Client, &m.Provenance.Service, &m.Provenance.SessionID,
		&m.Provenance.Author, &m.CreatedAt, &m.ValidFrom, &m.ValidTo, &m.LastActivatedAt}
	err := r.Scan(append(dest, extra...)...)
	if err != nil {
		return m, err
	}
	m.Provenance.At = m.CreatedAt
	_ = json.Unmarshal([]byte(tags), &m.Tags)
	if m.Tags == nil {
		m.Tags = []string{}
	}
	if conf.Valid {
		m.Confidence = &conf.Float64
	}
	if decision != "" {
		m.Decision = &Decision{}
		_ = json.Unmarshal([]byte(decision), m.Decision)
	}
	return m, nil
}

// Get fetches memories by id, checking read access for each.
func (s *Store) Get(client string, ids []string) ([]Memory, error) {
	out := make([]Memory, 0, len(ids))
	for _, id := range ids {
		m, err := scanMemory(s.db.QueryRow(`SELECT `+memoryColumns+` FROM memories WHERE id = ?`, id))
		if err == sql.ErrNoRows {
			return nil, ecp.Errorf(ecp.MemoryNotFound, "no memory %q", id)
		} else if err != nil {
			return nil, err
		}
		if ok, err := s.can(client, m.Namespace, false); err != nil {
			return nil, err
		} else if !ok {
			return nil, ecp.Errorf(ecp.ScopeDenied, "client %q cannot read namespace %q", client, m.Namespace)
		}
		out = append(out, m)
	}
	detail, _ := json.Marshal(ids)
	_ = audit(s.db, client, "get", "", string(detail))
	return out, nil
}

// Recall returns the memories most related to the query that the client may read.
func (s *Store) Recall(client string, in RecallInput) ([]Scored, error) {
	return s.recall(client, in, true, true)
}

// RecallSystem searches every namespace (or in.Namespaces) without grants and without
// side effects: no activation counts, no Hebbian updates, no audit entry. The sleep
// cycle uses it to test itself without changing what it is testing.
func (s *Store) RecallSystem(in RecallInput) ([]Scored, error) {
	return s.recall("engram-sleep", in, false, false)
}

// RecallQuiet checks the client's grants like Recall but has no side effects. Attend
// and push notifications use it, so that being shown something does not train pathways.
func (s *Store) RecallQuiet(client string, in RecallInput) ([]Scored, error) {
	return s.recall(client, in, true, false)
}

func (s *Store) recall(client string, in RecallInput, grants, effects bool) ([]Scored, error) {
	if in.K <= 0 {
		in.K = 5
	}
	in.K = min(in.K, 50)
	var readable []string
	all := true
	if grants {
		var err error
		if readable, all, err = s.readable(client); err != nil {
			return nil, err
		}
	}
	allowed := readable
	if len(in.Namespaces) > 0 {
		for _, ns := range in.Namespaces {
			if !all && !contains(readable, ns) {
				return nil, ecp.Errorf(ecp.ScopeDenied, "client %q cannot read namespace %q", client, ns)
			}
		}
		allowed, all = in.Namespaces, false
	}
	query := text.Tokenize(in.Query)
	var qvec []float32
	if s.emb != nil {
		if v, err := s.emb.Embed(context.Background(), []string{in.Query}, embed.Query); err == nil && len(v) == 1 {
			qvec = v[0]
		}
	}
	if len(query) == 0 && qvec == nil {
		return nil, ecp.Errorf(ecp.InvalidParams, "query has no searchable words")
	}
	if !all && len(allowed) == 0 {
		return []Scored{}, nil
	}

	where := []string{"1=1"}
	args := []any{}
	model := ""
	if s.emb != nil {
		model = s.emb.Model()
	}
	if !all {
		where = append(where, "m.namespace IN ("+placeholders(len(allowed))+")")
		args = append(args, toAny(allowed)...)
	}
	if len(in.Kinds) > 0 {
		where = append(where, "m.kind IN ("+placeholders(len(in.Kinds))+")")
		args = append(args, toAny(in.Kinds)...)
	}
	historical := in.AsOf != ""
	if historical {
		at, err := ParseTime(in.AsOf)
		if err != nil {
			return nil, err
		}
		// A date means the end of that day.
		if len(in.AsOf) == len("2006-01-02") {
			at = at.Add(24*time.Hour - time.Second)
		}
		a := ts(at)
		// "As of" asks what was true in the world then (valid time), using everything known now,
		// including facts recorded later about that period.
		where = append(where, "m.valid_from <= ? AND (m.valid_to IS NULL OR m.valid_to > ?)")
		args = append(args, a, a)
	} else {
		where = append(where, "(m.valid_to IS NULL OR m.valid_to > ?)")
		args = append(args, ts(Now()))
	}
	args = append([]any{model}, args...)
	args = append(args, maxCandidates)
	rows, err := s.db.Query(`SELECT `+memoryColumnsM+`, m.activations, v.vec FROM memories m
		LEFT JOIN vectors v ON v.memory_id = m.id AND v.model = ?
		WHERE `+strings.Join(where, " AND ")+` ORDER BY m.created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	var mems []Memory
	var vecs [][]float32
	var docs []doc
	var acts []int
	for rows.Next() {
		var vb []byte
		var n int
		m, err := scanMemoryWith(rows, &n, &vb)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if len(in.Tags) > 0 && !overlaps(m.Tags, in.Tags) {
			continue
		}
		mems = append(mems, m)
		acts = append(acts, n)
		if len(vb) > 0 {
			vecs = append(vecs, decodeVec(vb))
		} else {
			vecs = append(vecs, nil)
		}
		docs = append(docs, newDoc(m.Content+" "+strings.Join(m.Tags, " ")))
	}
	rows.Close()

	// Direct relevance: keywords and, when available, vector similarity.
	kw := bm25(query, docs)
	minSim := s.learn.MinSimilarity
	now := Now()
	type cand struct {
		score, conf float64
		reason      string
		path        []string
	}
	scored := map[int]cand{}
	index := map[string]int{}
	prior := make([]float64, len(mems)) // how readily each memory comes to mind, 0..1
	for i, m := range mems {
		index[m.ID] = i
		prior[i] = 0.6*m.Strength + 0.4*baseLevel(acts[i], m.CreatedAt, now)
		kwScore := 0.0
		if kw[i] > 0 {
			kwScore = kw[i] / (kw[i] + 1)
		}
		sem, hasSem := 0.0, false
		if qvec != nil && vecs[i] != nil {
			if c := embed.Cosine(qvec, vecs[i]); c >= minSim {
				sem, hasSem = (c-minSim)/(1-minSim), true
			}
		}
		if kwScore == 0 && !hasSem {
			continue
		}
		rel, conf := kwScore, kwScore
		reason := "keyword match"
		if qvec != nil {
			rel = 0.5*kwScore + 0.5*sem
			conf = rel
			switch {
			case hasSem && kwScore > 0:
				reason = "meaning and keyword match"
				conf = math.Min(1, rel*1.25) // two independent signals agree
			case hasSem:
				reason = "meaning match"
			}
		}
		if m.Kind == "decision" {
			reason += ", user decision"
		}
		scored[i] = cand{score: 0.75*rel + 0.25*prior[i], conf: conf, reason: reason}
	}

	// Memories the user explicitly overrode are demoted, and say so.
	if len(scored) > 0 {
		ids := make([]string, 0, len(scored))
		for i := range scored {
			ids = append(ids, mems[i].ID)
		}
		rows, err := s.db.Query(`SELECT DISTINCT dst FROM edges WHERE kind = 'overrode' AND dst IN (`+placeholders(len(ids))+`)`, toAny(ids)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			i := index[id]
			c := scored[i]
			c.score *= 0.5
			c.reason += ", overridden by the user"
			scored[i] = c
		}
		rows.Close()
	}

	// Spreading activation: a beam of the strongest activations spreads along
	// learned and similarity edges, a_j = a_i · w / √deg(i), for up to 3 hops.
	hops := max(0, min(in.Hops, 3))
	frontier := make([]int, 0, len(scored))
	for i := range scored {
		frontier = append(frontier, i)
	}
	for hop := 0; hop < hops && len(frontier) > 0; hop++ {
		sort.Slice(frontier, func(a, b int) bool { return scored[frontier[a]].score > scored[frontier[b]].score })
		if len(frontier) > 10 {
			frontier = frontier[:10]
		}
		ids := make([]string, len(frontier))
		for k, i := range frontier {
			ids[k] = mems[i].ID
		}
		nbs, err := s.neighbors(ids)
		if err != nil {
			return nil, err
		}
		var next []int
		for _, si := range frontier {
			src := scored[si]
			deg := math.Max(1, float64(len(nbs[mems[si].ID])))
			for _, n := range nbs[mems[si].ID] {
				j, ok := index[n.id] // only memories that pass this query's filters
				if !ok {
					continue
				}
				act := src.score * n.weight * 0.9 / math.Sqrt(deg)
				if cur, ok := scored[j]; ok && cur.score >= act {
					continue
				}
				path := append(append([]string{}, src.path...), n.id)
				if len(src.path) == 0 {
					path = []string{mems[si].ID, n.id}
				}
				scored[j] = cand{score: act, conf: src.conf * n.weight,
					reason: "via " + n.kind + " pathway from " + path[0], path: path}
				next = append(next, j)
			}
		}
		frontier = next
	}

	order := make([]int, 0, len(scored))
	for i, c := range scored {
		if c.score >= 0.005 && c.conf >= 0.01 { // drop noise; weak pathways still surface, ranked last with low confidence
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		ca, cb := scored[order[a]], scored[order[b]]
		if ca.score != cb.score {
			return ca.score > cb.score
		}
		return mems[order[a]].ID < mems[order[b]].ID
	})
	// Lateral inhibition: a near-duplicate of a better result is suppressed.
	results := make([]Scored, 0, in.K)
	var kept []int
	for _, i := range order {
		dup := false
		for _, k := range kept {
			if nearDuplicate(mems[i], mems[k], vecs[i], vecs[k]) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		kept = append(kept, i)
		c := scored[i]
		results = append(results, Scored{Memory: mems[i], Score: round4(c.score), Confidence: round4(math.Min(1, c.conf)),
			Reason: c.reason, Path: c.path})
		if len(results) == in.K {
			break
		}
	}
	if !historical && effects && len(results) > 0 {
		stamp := ts(now)
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.Memory.ID
			if _, err := s.db.Exec(`UPDATE memories SET last_activated_at = ?, activations = activations + 1 WHERE id = ?`, stamp, r.Memory.ID); err != nil {
				return nil, err
			}
		}
		if err := s.coactivate(client, ids); err != nil {
			return nil, err
		}
	}
	if effects {
		detail, _ := json.Marshal(map[string]any{"q": in.Query, "n": len(results)})
		_ = audit(s.db, client, "recall", "", string(detail))
	}
	return results, nil
}

// Stats summarizes the store.
type Stats struct {
	Memories   int      `json:"memories"`
	Current    int      `json:"current"`
	Edges      int      `json:"edges"`
	Clients    int      `json:"clients"`
	Namespaces []string `json:"namespaces"`
}

// Stats returns counts for the CLI and dashboard.
func (s *Store) Stats() (Stats, error) {
	var st Stats
	for _, q := range []struct {
		sql string
		dst *int
	}{
		{`SELECT count(*) FROM memories`, &st.Memories},
		{`SELECT count(*) FROM memories WHERE valid_to IS NULL`, &st.Current},
		{`SELECT count(*) FROM edges`, &st.Edges},
		{`SELECT count(DISTINCT client) FROM grants`, &st.Clients},
	} {
		if err := s.db.QueryRow(q.sql).Scan(q.dst); err != nil {
			return st, err
		}
	}
	rows, err := s.db.Query(`SELECT DISTINCT namespace FROM memories ORDER BY 1`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	st.Namespaces = []string{}
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return st, err
		}
		st.Namespaces = append(st.Namespaces, ns)
	}
	return st, rows.Err()
}

// ---------------------------------------------------------------- helpers

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func audit(e execer, client, action, namespace, detail string) error {
	_, err := e.Exec(`INSERT INTO audit(at, client, action, namespace, detail) VALUES (?,?,?,?,?)`,
		ts(Now()), client, action, nullable(namespace), nullable(detail))
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if contains(b, x) {
			return true
		}
	}
	return false
}

func dedupeSorted(in []string) []string {
	set := map[string]bool{}
	out := []string{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t != "" && !set[t] {
			set[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func round4(f float64) float64 { return float64(int(f*10000+0.5)) / 10000 }
