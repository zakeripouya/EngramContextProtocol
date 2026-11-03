package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
)

// System writes come from the user's own tools (imports, packs, the sleep cycle),
// run locally through the CLI. They bypass client grants, never create decisions,
// and are keyed so that running the same import twice changes nothing.

// SystemMemory is a memory written by Engram's own tooling rather than an agent.
type SystemMemory struct {
	Key       string // stable identity for idempotence, e.g. "git-commit:<hash>"
	Kind      string // episode, fact, concept, procedure or trait
	Namespace string
	Service   string // git, jira, pack, import, sleep
	Content   string
	Tags      []string
	Strength  float64
	ValidFrom time.Time
}

var systemKinds = map[string]bool{"episode": true, "fact": true, "concept": true, "procedure": true, "trait": true}

// InsertSystem stores a system memory unless one with the same key exists.
// It returns the memory's id and whether it was created now.
func (s *Store) InsertSystem(m SystemMemory) (string, bool, error) {
	if m.Key == "" {
		return "", false, fmt.Errorf("system memory needs a key")
	}
	if !systemKinds[m.Kind] {
		return "", false, fmt.Errorf("system memories cannot have kind %q", m.Kind)
	}
	if m.Namespace == "" || !namespaceRE.MatchString(m.Namespace) {
		return "", false, fmt.Errorf("invalid namespace %q", m.Namespace)
	}
	m.Content = strings.TrimSpace(m.Content)
	if m.Content == "" {
		return "", false, fmt.Errorf("content is empty")
	}
	if hit := FindSensitive(m.Content); hit != "" {
		return "", false, ecp.Errorf(ecp.ContentRejected, "content looks like it contains a %s; nothing was stored", hit)
	}
	var existing string
	err := s.db.QueryRow(`SELECT memory_id FROM import_keys WHERE key = ?`, m.Key).Scan(&existing)
	if err == nil {
		return existing, false, nil
	} else if err != sql.ErrNoRows {
		return "", false, err
	}
	now := Now()
	vf := now
	if !m.ValidFrom.IsZero() {
		vf = m.ValidFrom.UTC()
	}
	if m.Strength <= 0 {
		m.Strength = 0.3
	}
	tags, _ := json.Marshal(dedupeSorted(m.Tags))
	id := "mem_" + newID()
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO memories(id, kind, content, tags, scope, namespace, strength, agent, client,
		service, author, created_at, valid_from) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, m.Kind, m.Content, string(tags), "private", m.Namespace, m.Strength, "engram", "engram-cli",
		nullable(m.Service), "system", ts(now), ts(vf)); err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(`INSERT INTO import_keys(key, memory_id) VALUES (?,?)`, m.Key, id); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	if _, _, err := s.embedAndLink(bgCtx(), id, m.Namespace, m.Content); err != nil {
		return id, true, err
	}
	return id, true, nil
}

// RemoveNamespace deletes every memory in a namespace and every edge into it
// (used to uninstall a pack). It returns the number of memories removed.
func (s *Store) RemoveNamespace(namespace string) (int, error) {
	if namespace == "" || namespace == "*" {
		return 0, fmt.Errorf("refusing to remove namespace %q", namespace)
	}
	res, err := s.db.Exec(`DELETE FROM memories WHERE namespace = ?`, namespace)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	// Keys of removed memories must go too, or reinstalling would find them and skip.
	if _, err := s.db.Exec(`DELETE FROM import_keys WHERE memory_id NOT IN (SELECT id FROM memories)`); err != nil {
		return int(n), err
	}
	if _, err := s.db.Exec(`DELETE FROM edges WHERE src NOT IN (SELECT id FROM memories) OR dst NOT IN (SELECT id FROM memories)`); err != nil {
		return int(n), err
	}
	_ = audit(s.db, "engram-cli", "remove-namespace", namespace, "")
	return int(n), nil
}

// KeyExists reports whether a system memory with this key was ever created.
func (s *Store) KeyExists(key string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM import_keys WHERE key = ?`, key).Scan(&n)
	return n > 0, err
}

// AddEdgeIfAbsent adds an edge only when none of that kind exists between the two
// memories, so re-running an import never resets a weight that use has changed.
func (s *Store) AddEdgeIfAbsent(src, dst, kind string, weight float64) (bool, error) {
	weight = max(0, min(1, weight))
	res, err := s.db.Exec(`INSERT INTO edges(src, dst, kind, weight) VALUES (?,?,?,?) ON CONFLICT(src, dst, kind) DO NOTHING`,
		src, dst, kind, weight)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
