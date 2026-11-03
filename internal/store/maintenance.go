package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Primitives for the sleep cycle (milestone M5; docs/design.md, "Consolidation").
// Every destructive step is recorded in sleep_changes so it can be undone,
// and every cycle starts from a snapshot so a whole night can be rolled back.

// Setting returns a stored setting, or "" if unset.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSetting stores a setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Snapshot writes a consistent copy of the database into dir and keeps the newest `keep` copies.
func (s *Store) Snapshot(dir string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := "engram-" + Now().Format("20060102-150405") + ".db"
	path := filepath.Join(dir, name)
	if _, err := s.db.Exec(`VACUUM INTO ?`, path); err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	if keep > 0 {
		entries, _ := filepath.Glob(filepath.Join(dir, "engram-*.db"))
		sort.Strings(entries)
		for len(entries) > keep {
			_ = os.Remove(entries[0])
			entries = entries[1:]
		}
	}
	return path, nil
}

// LastAgentActivity returns when any agent last read or wrote memory (zero if never).
func (s *Store) LastAgentActivity() (time.Time, error) {
	var at sql.NullString
	err := s.db.QueryRow(`SELECT max(at) FROM audit WHERE action IN ('observe','recall','get','feedback','forget')
		AND client NOT IN ('engram-cli','engram-sleep','system')`).Scan(&at)
	if err != nil || !at.Valid {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, at.String)
}

// Downscale applies homeostatic downscaling: memories unused for idleDays lose a
// fraction of their strength, keeping relative order. User decisions are exempt.
func (s *Store) Downscale(factor float64, idleDays int) (int, error) {
	cutoff := ts(Now().Add(-time.Duration(idleDays) * 24 * time.Hour))
	res, err := s.db.Exec(`UPDATE memories SET strength = max(0.05, strength * ?)
		WHERE kind <> 'decision' AND valid_to IS NULL AND created_at < ?
		AND (last_activated_at IS NULL OR last_activated_at < ?)`, factor, cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MemVec is a current memory with its activation count and vector, for maintenance passes.
type MemVec struct {
	Memory
	Activations int
	Vec         []float32
}

// CurrentMemories returns every current memory in a namespace with its vector (if any).
func (s *Store) CurrentMemories(namespace string) ([]MemVec, error) {
	model := ""
	if s.emb != nil {
		model = s.emb.Model()
	}
	rows, err := s.db.Query(`SELECT `+memoryColumnsM+`, m.activations, v.vec FROM memories m
		LEFT JOIN vectors v ON v.memory_id = m.id AND v.model = ?
		WHERE m.namespace = ? AND m.valid_to IS NULL ORDER BY m.created_at`, model, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemVec
	for rows.Next() {
		var mv MemVec
		var vb []byte
		m, err := scanMemoryWith(rows, &mv.Activations, &vb)
		if err != nil {
			return nil, err
		}
		mv.Memory = m
		if len(vb) > 0 {
			mv.Vec = decodeVec(vb)
		}
		out = append(out, mv)
	}
	return out, rows.Err()
}

// Namespaces returns every namespace that has current memories.
func (s *Store) Namespaces() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT namespace FROM memories WHERE valid_to IS NULL ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// Edge is a weighted edge between two memories.
type Edge struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Kind   string  `json:"kind"`
	Weight float64 `json:"weight"`
}

// EdgesIn returns edges of the given kinds between current memories of one namespace.
func (s *Store) EdgesIn(namespace string, kinds ...string) ([]Edge, error) {
	q := `SELECT e.src, e.dst, e.kind, e.weight FROM edges e
		JOIN memories a ON a.id = e.src JOIN memories b ON b.id = e.dst
		WHERE a.namespace = ? AND b.namespace = ? AND a.valid_to IS NULL AND b.valid_to IS NULL`
	args := []any{namespace, namespace}
	if len(kinds) > 0 {
		q += ` AND e.kind IN (` + placeholders(len(kinds)) + `)`
		args = append(args, toAny(kinds)...)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.From, &e.To, &e.Kind, &e.Weight); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Archive retires a memory without deleting it: it stops being current, and when
// replacedBy is set, a "supersedes" edge points from the replacement to it.
func (s *Store) Archive(id, replacedBy string) error {
	res, err := s.db.Exec(`UPDATE memories SET valid_to = ? WHERE id = ? AND valid_to IS NULL`, ts(Now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("memory %s is not current", id)
	}
	if replacedBy != "" {
		return s.AddEdge(replacedBy, id, "supersedes", 1)
	}
	return nil
}

// Restore makes an archived memory current again and removes edges that superseded it.
func (s *Store) Restore(id string) error {
	if _, err := s.db.Exec(`UPDATE memories SET valid_to = NULL WHERE id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM edges WHERE dst = ? AND kind = 'supersedes'`, id)
	return err
}

// DeleteMemory removes a memory outright (used to undo a distilled fact).
func (s *Store) DeleteMemory(id string) error {
	_, err := s.db.Exec(`DELETE FROM memories WHERE id = ?`, id)
	return err
}

// Decisions returns current user decisions in a namespace (all namespaces when "").
func (s *Store) Decisions(namespace string) ([]Memory, error) {
	q := `SELECT ` + memoryColumns + ` FROM memories WHERE kind = 'decision' AND valid_to IS NULL`
	var args []any
	if namespace != "" {
		q += ` AND namespace = ?`
		args = append(args, namespace)
	}
	rows, err := s.db.Query(q+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- run log and undo

// Change is one undoable step of a sleep cycle.
type Change struct {
	ID       int64  `json:"id"`
	RunID    int64  `json:"runId"`
	Kind     string `json:"kind"` // merged, archived, distilled
	MemoryID string `json:"memoryId"`
	OtherID  string `json:"otherId,omitempty"`
	Detail   string `json:"detail,omitempty"`
	UndoneAt string `json:"undoneAt,omitempty"`
}

// Run is one sleep cycle.
type Run struct {
	ID         int64           `json:"id"`
	StartedAt  string          `json:"startedAt"`
	FinishedAt string          `json:"finishedAt,omitempty"`
	Light      bool            `json:"light"`
	Snapshot   string          `json:"snapshot,omitempty"`
	Report     json.RawMessage `json:"report,omitempty"`
}

// StartRun records the start of a sleep cycle.
func (s *Store) StartRun(light bool, snapshot string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO sleep_runs(started_at, light, snapshot) VALUES (?,?,?)`,
		ts(Now()), light, nullable(snapshot))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishRun stores a cycle's report.
func (s *Store) FinishRun(id int64, report any) error {
	b, err := json.Marshal(report)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE sleep_runs SET finished_at = ?, report = ? WHERE id = ?`, ts(Now()), string(b), id)
	return err
}

// RecordChange logs an undoable change made during run.
func (s *Store) RecordChange(run int64, kind, memoryID, otherID, detail string) error {
	_, err := s.db.Exec(`INSERT INTO sleep_changes(run_id, kind, memory_id, other_id, detail) VALUES (?,?,?,?,?)`,
		run, kind, memoryID, nullable(otherID), nullable(detail))
	return err
}

// Runs returns the most recent sleep cycles, newest first.
func (s *Store) Runs(limit int) ([]Run, error) {
	rows, err := s.db.Query(`SELECT id, started_at, COALESCE(finished_at,''), light, COALESCE(snapshot,''), COALESCE(report,'')
		FROM sleep_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var report string
		if err := rows.Scan(&r.ID, &r.StartedAt, &r.FinishedAt, &r.Light, &r.Snapshot, &report); err != nil {
			return nil, err
		}
		if report != "" {
			r.Report = json.RawMessage(report)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Changes returns the changes of one run.
func (s *Store) Changes(run int64) ([]Change, error) {
	rows, err := s.db.Query(`SELECT id, run_id, kind, memory_id, COALESCE(other_id,''), COALESCE(detail,''), COALESCE(undone_at,'')
		FROM sleep_changes WHERE run_id = ? ORDER BY id`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.ID, &c.RunID, &c.Kind, &c.MemoryID, &c.OtherID, &c.Detail, &c.UndoneAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UndoChange reverses one recorded change: a merge or archive is restored, a distilled fact is deleted.
func (s *Store) UndoChange(id int64) (Change, error) {
	var c Change
	err := s.db.QueryRow(`SELECT id, run_id, kind, memory_id, COALESCE(other_id,''), COALESCE(undone_at,'')
		FROM sleep_changes WHERE id = ?`, id).Scan(&c.ID, &c.RunID, &c.Kind, &c.MemoryID, &c.OtherID, &c.UndoneAt)
	if err == sql.ErrNoRows {
		return c, fmt.Errorf("no change %d", id)
	} else if err != nil {
		return c, err
	}
	if c.UndoneAt != "" {
		return c, fmt.Errorf("change %d was already undone", id)
	}
	switch c.Kind {
	case "merged", "archived":
		err = s.Restore(c.MemoryID)
	case "distilled":
		err = s.DeleteMemory(c.MemoryID)
	default:
		err = fmt.Errorf("change kind %q cannot be undone", c.Kind)
	}
	if err != nil {
		return c, err
	}
	_, err = s.db.Exec(`UPDATE sleep_changes SET undone_at = ? WHERE id = ?`, ts(Now()), id)
	return c, err
}

// shortContent trims content for reports.
func shortContent(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 90 {
		return s[:87] + "..."
	}
	return s
}

// Short exposes shortContent to the sleep package.
func Short(s string) string { return shortContent(s) }

// ---------------------------------------------------------------- health data (M6)

// DegreeMax returns the largest number of similar or coactive edges on any current memory.
func (s *Store) DegreeMax() (int, string, error) {
	var n int
	var id sql.NullString
	err := s.db.QueryRow(`SELECT id, count(*) AS d FROM (
		SELECT src AS id FROM edges WHERE kind IN ('similar','coactive')
		UNION ALL SELECT dst AS id FROM edges WHERE kind IN ('similar','coactive'))
		GROUP BY id ORDER BY d DESC LIMIT 1`).Scan(&id, &n)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	return n, id.String, err
}

// Weights returns the weights of all edges of one kind.
func (s *Store) Weights(kind string) ([]float64, error) {
	rows, err := s.db.Query(`SELECT weight FROM edges WHERE kind = ?`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var w float64
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Strengths returns the strength of every current memory.
func (s *Store) Strengths() ([]float64, error) {
	rows, err := s.db.Query(`SELECT strength FROM memories WHERE valid_to IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var w float64
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SaveHealth stores one night's metrics.
func (s *Store) SaveHealth(metrics any) error {
	b, err := json.Marshal(metrics)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO health(at, metrics) VALUES (?,?)`, ts(Now()), string(b))
	return err
}

// HealthHistory returns stored metrics, newest first.
func (s *Store) HealthHistory(limit int) ([]json.RawMessage, error) {
	rows, err := s.db.Query(`SELECT metrics FROM health ORDER BY at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(m))
	}
	return out, rows.Err()
}

// Golden is one query in the user's golden recall suite.
type Golden struct {
	ID        int64  `json:"id"`
	Query     string `json:"query"`
	ExpectID  string `json:"expectId"`
	Namespace string `json:"namespace"`
}

// AddGolden records that query should recall expectID within the top three.
func (s *Store) AddGolden(query, expectID string) (int64, error) {
	var ns string
	if err := s.db.QueryRow(`SELECT namespace FROM memories WHERE id = ?`, expectID).Scan(&ns); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("no memory %s", expectID)
		}
		return 0, err
	}
	res, err := s.db.Exec(`INSERT INTO golden(query, expect_id, namespace, added_at) VALUES (?,?,?,?)`,
		query, expectID, ns, ts(Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GoldenSuite returns every golden query.
func (s *Store) GoldenSuite() ([]Golden, error) {
	rows, err := s.db.Query(`SELECT id, query, expect_id, namespace FROM golden ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Golden
	for rows.Next() {
		var g Golden
		if err := rows.Scan(&g.ID, &g.Query, &g.ExpectID, &g.Namespace); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Path returns where an on-disk store lives ("" for in-memory).
func (s *Store) Path() string { return s.path }
