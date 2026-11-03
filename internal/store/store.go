// Package store is Engram's persistence and access-control layer (milestone M1).
//
// One SQLite file holds memories, edges, client grants and an audit log.
// Every read and write is checked against the calling client's namespace grants,
// and every memory carries provenance and bitemporal validity
// (docs/design.md, "Memory model", "Time validity", "Namespaces and isolation").
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
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

// Scored is a recall result.
type Scored struct {
	Memory Memory  `json:"memory"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
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
}

// Store is one Engram instance.
type Store struct{ db *sql.DB }

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
	return &Store{db: db}, nil
}

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
	ID     string `json:"id"`
	Linked int    `json:"linked"`
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

	tx, err := s.db.Begin()
	if err != nil {
		return ObserveResult{}, err
	}
	defer tx.Rollback()
	if in.Supersedes != "" {
		var ns string
		err := tx.QueryRow(`SELECT namespace FROM memories WHERE id = ?`, in.Supersedes).Scan(&ns)
		if err == sql.ErrNoRows {
			return ObserveResult{}, ecp.Errorf(ecp.MemoryNotFound, "no memory %q", in.Supersedes)
		} else if err != nil {
			return ObserveResult{}, err
		}
		if ok, err := s.can(client, ns, true); err != nil || !ok {
			return ObserveResult{}, ecp.Errorf(ecp.ScopeDenied, "cannot supersede a memory in namespace %q", ns)
		}
	}
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
	return ObserveResult{ID: id, Linked: linked}, tx.Commit()
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

type scanner interface{ Scan(dest ...any) error }

func scanMemory(r scanner) (Memory, error) {
	var m Memory
	var tags, decision string
	var conf sql.NullFloat64
	err := r.Scan(&m.ID, &m.Kind, &m.Content, &tags, &m.Scope, &m.Namespace, &m.Strength, &conf, &decision,
		&m.Provenance.Agent, &m.Provenance.Client, &m.Provenance.Service, &m.Provenance.SessionID,
		&m.Provenance.Author, &m.CreatedAt, &m.ValidFrom, &m.ValidTo, &m.LastActivatedAt)
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
	if in.K <= 0 {
		in.K = 5
	}
	in.K = min(in.K, 50)
	readable, all, err := s.readable(client)
	if err != nil {
		return nil, err
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
	query := Tokenize(in.Query)
	if len(query) == 0 {
		return nil, ecp.Errorf(ecp.InvalidParams, "query has no searchable words")
	}
	if !all && len(allowed) == 0 {
		return []Scored{}, nil
	}

	where := []string{"1=1"}
	args := []any{}
	if !all {
		where = append(where, "namespace IN ("+placeholders(len(allowed))+")")
		args = append(args, toAny(allowed)...)
	}
	if len(in.Kinds) > 0 {
		where = append(where, "kind IN ("+placeholders(len(in.Kinds))+")")
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
		where = append(where, "created_at <= ? AND valid_from <= ? AND (valid_to IS NULL OR valid_to > ?)")
		args = append(args, a, a, a)
	} else {
		where = append(where, "(valid_to IS NULL OR valid_to > ?)")
		args = append(args, ts(Now()))
	}
	args = append(args, maxCandidates)
	rows, err := s.db.Query(`SELECT `+memoryColumns+` FROM memories WHERE `+strings.Join(where, " AND ")+
		` ORDER BY created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	var mems []Memory
	var docs []doc
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if len(in.Tags) > 0 && !overlaps(m.Tags, in.Tags) {
			continue
		}
		mems = append(mems, m)
		docs = append(docs, newDoc(m.Content+" "+strings.Join(m.Tags, " ")))
	}
	rows.Close()

	scores := bm25(query, docs)
	var results []Scored
	for i, sc := range scores {
		if sc <= 0 {
			continue
		}
		relevance := sc / (sc + 1) // squash to 0..1
		reason := "text match"
		if mems[i].Kind == "decision" {
			reason = "text match, user decision"
		}
		results = append(results, Scored{Memory: mems[i], Score: round4(0.75*relevance + 0.25*mems[i].Strength), Reason: reason})
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > in.K {
		results = results[:in.K]
	}
	if !historical && len(results) > 0 {
		now := ts(Now())
		for _, r := range results {
			if _, err := s.db.Exec(`UPDATE memories SET last_activated_at = ?, activations = activations + 1 WHERE id = ?`, now, r.Memory.ID); err != nil {
				return nil, err
			}
		}
	}
	detail, _ := json.Marshal(map[string]any{"q": in.Query, "n": len(results)})
	_ = audit(s.db, client, "recall", "", string(detail))
	if results == nil {
		results = []Scored{}
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
