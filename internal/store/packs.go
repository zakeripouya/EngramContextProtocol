package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Installed pathway packs and the prior weights they installed, so the dashboard
// can show how real use has reweighted each pack (milestone M11).

// PackInfo describes an installed pack.
type PackInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Kind        string `json:"kind"`   // template, source or instance
	Source      string `json:"source"` // builtin, a file path, or registry:<name>
	Publisher   string `json:"publisher,omitempty"`
	Verified    bool   `json:"verified"`
	Description string `json:"description,omitempty"`
	InstalledAt string `json:"installedAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// PackStats is how use has changed a pack's pathways since install.
type PackStats struct {
	Memories     int     `json:"memories"`
	Edges        int     `json:"edges"`
	Strengthened int     `json:"strengthened"` // now at least 0.05 above the installed prior
	Weakened     int     `json:"weakened"`     // now at least 0.05 below it, or pruned
	MeanDelta    float64 `json:"meanDelta"`
	Recalled     int     `json:"recalled"` // pack memories activated by recall at least once
}

// RecordPack stores or updates an installed pack's metadata.
func (s *Store) RecordPack(p PackInfo) error {
	now := ts(Now())
	_, err := s.db.Exec(`INSERT INTO packs(name, version, kind, source, publisher, verified, description, installed_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET version=excluded.version, kind=excluded.kind, source=excluded.source,
		  publisher=excluded.publisher, verified=excluded.verified, description=excluded.description, updated_at=excluded.updated_at`,
		p.Name, p.Version, p.Kind, p.Source, p.Publisher, p.Verified, p.Description, now, now)
	return err
}

// Packs lists installed packs.
func (s *Store) Packs() ([]PackInfo, error) {
	rows, err := s.db.Query(`SELECT name, version, kind, source, publisher, verified, description, installed_at, updated_at
		FROM packs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PackInfo{}
	for rows.Next() {
		var p PackInfo
		if err := rows.Scan(&p.Name, &p.Version, &p.Kind, &p.Source, &p.Publisher, &p.Verified, &p.Description, &p.InstalledAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// InstalledPack returns one installed pack, or ok=false.
func (s *Store) InstalledPack(name string) (PackInfo, bool, error) {
	var p PackInfo
	err := s.db.QueryRow(`SELECT name, version, kind, source, publisher, verified, description, installed_at, updated_at
		FROM packs WHERE name = ?`, name).Scan(&p.Name, &p.Version, &p.Kind, &p.Source, &p.Publisher, &p.Verified, &p.Description, &p.InstalledAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

// ForgetPack removes a pack's metadata and recorded priors (its memories are removed separately).
func (s *Store) ForgetPack(name string) error {
	if _, err := s.db.Exec(`DELETE FROM pack_edges WHERE pack = ?`, name); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM packs WHERE name = ?`, name)
	return err
}

// AddPackEdge adds a pack pathway at its prior weight unless the pathway already
// exists: reinstalling or updating a pack never resets what use has learned.
// It reports whether the edge was new.
func (s *Store) AddPackEdge(pack, src, dst, kind string, prior float64) (bool, error) {
	prior = max(0, min(1, prior))
	res, err := s.db.Exec(`INSERT INTO edges(src, dst, kind, weight) VALUES (?,?,?,?) ON CONFLICT(src, dst, kind) DO NOTHING`,
		src, dst, kind, prior)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	_, err = s.db.Exec(`INSERT INTO pack_edges(pack, src, dst, kind, prior) VALUES (?,?,?,?,?)
		ON CONFLICT(src, dst, kind) DO UPDATE SET pack = excluded.pack, prior = excluded.prior`, pack, src, dst, kind, prior)
	return true, err
}

// PackStatsFor measures how use has reweighted a pack.
func (s *Store) PackStatsFor(name string) (PackStats, error) {
	var st PackStats
	if err := s.db.QueryRow(`SELECT count(*), COALESCE(sum(activations > 0), 0) FROM memories
		WHERE namespace = ? AND valid_to IS NULL`, "pack/"+name).Scan(&st.Memories, &st.Recalled); err != nil {
		return st, err
	}
	rows, err := s.db.Query(`SELECT p.prior, e.weight FROM pack_edges p
		LEFT JOIN edges e ON e.src = p.src AND e.dst = p.dst AND e.kind = p.kind WHERE p.pack = ?`, name)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	var sum float64
	for rows.Next() {
		var prior float64
		var w sql.NullFloat64
		if err := rows.Scan(&prior, &w); err != nil {
			return st, err
		}
		st.Edges++
		now := 0.0
		if w.Valid {
			now = w.Float64
		}
		d := now - prior
		sum += d
		switch {
		case !w.Valid || d <= -0.05:
			st.Weakened++
		case d >= 0.05:
			st.Strengthened++
		}
	}
	if st.Edges > 0 {
		st.MeanDelta = round4(sum / float64(st.Edges))
	}
	return st, rows.Err()
}

// SystemKeys returns system-memory keys with a prefix and their memory ids.
func (s *Store) SystemKeys(prefix string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, memory_id FROM import_keys WHERE key LIKE ? ESCAPE '\'`,
		strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, id string
		if err := rows.Scan(&k, &id); err != nil {
			return nil, err
		}
		out[k] = id
	}
	return out, rows.Err()
}

// UpdateSystemMemory changes a system memory's content and tags (a pack update),
// makes it current again if it was archived, and re-embeds it.
func (s *Store) UpdateSystemMemory(id, content string, tags []string) (bool, error) {
	if hit := FindSensitive(content); hit != "" {
		return false, errors.New("content looks like it contains a " + hit)
	}
	var old, oldTags, ns string
	var validTo sql.NullString
	if err := s.db.QueryRow(`SELECT content, tags, namespace, valid_to FROM memories WHERE id = ?`, id).Scan(&old, &oldTags, &ns, &validTo); err != nil {
		return false, err
	}
	tb, _ := json.Marshal(dedupeSorted(tags))
	if old == content && oldTags == string(tb) && !validTo.Valid {
		return false, nil
	}
	if _, err := s.db.Exec(`UPDATE memories SET content = ?, tags = ?, valid_to = NULL WHERE id = ?`, content, string(tb), id); err != nil {
		return false, err
	}
	if old != content {
		if _, _, err := s.embedAndLink(bgCtx(), id, ns, content); err != nil {
			return true, err
		}
	}
	return true, nil
}
