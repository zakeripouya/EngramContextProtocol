package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
)

// Milestone M2: vectors, kNN wiring, Hebbian learning and decay
// (docs/design.md, "Encoding" and "Wiring and learning").
//
//   - On write, a memory is embedded and linked to its k nearest neighbors in
//     the same namespace with "similar" edges (weight = cosine similarity).
//   - On recall, memories returned together are co-activated: their "coactive"
//     edge is strengthened with w ← w + η·(1 − w).
//   - Anti-echo: a pair counts once per client per hour, so an agent repeating
//     the same lookup cannot reinforce its own results.
//   - Decay: learned edges fade with w ← (1 − λ)·w and are pruned below a floor.

// Learning holds the M2 tuning parameters.
type Learning struct {
	K             int           // neighbors linked on write
	MinSimilarity float64       // a vector neighbor must be at least this similar to be linked or recalled
	Eta           float64       // Hebbian learning rate
	CoactiveTopN  int           // how many top recall results co-activate
	EchoWindow    time.Duration // same client, same pair: count once per window
	Lambda        float64       // decay rate per Decay call
	Floor         float64       // learned edges below this weight are pruned
}

// DefaultLearning is used when a store is opened. MinSimilarity suits the
// hashing embedder and nomic-embed-text; other models may need tuning.
var DefaultLearning = Learning{
	K: 8, MinSimilarity: 0.35, Eta: 0.15, CoactiveTopN: 4,
	EchoWindow: time.Hour, Lambda: 0.05, Floor: 0.02,
}

// SetEmbedder attaches an embedder. With nil, Engram uses keyword ranking only.
func (s *Store) SetEmbedder(e embed.Embedder) { s.emb = e }

// Embedder returns the attached embedder, or nil.
func (s *Store) Embedder() embed.Embedder { return s.emb }

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

type vecRow struct {
	id  string
	vec []float32
}

// vectorsIn loads vectors of the current model for the given namespaces (nil = all), current memories only.
func (s *Store) vectorsIn(namespaces []string, all bool, exclude string) ([]vecRow, error) {
	if s.emb == nil {
		return nil, nil
	}
	q := `SELECT v.memory_id, v.vec FROM vectors v JOIN memories m ON m.id = v.memory_id
		WHERE v.model = ? AND m.id <> ? AND (m.valid_to IS NULL OR m.valid_to > ?)`
	args := []any{s.emb.Model(), exclude, ts(Now())}
	if !all {
		if len(namespaces) == 0 {
			return nil, nil
		}
		q += ` AND m.namespace IN (` + placeholders(len(namespaces)) + `)`
		args = append(args, toAny(namespaces)...)
	}
	q += ` ORDER BY m.created_at DESC LIMIT ?`
	args = append(args, maxCandidates)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []vecRow
	for rows.Next() {
		var r vecRow
		var b []byte
		if err := rows.Scan(&r.id, &b); err != nil {
			return nil, err
		}
		r.vec = decodeVec(b)
		out = append(out, r)
	}
	return out, rows.Err()
}

// embedAndLink embeds one memory, stores its vector and links its nearest neighbors.
// It returns the number of edges created. Failure to embed is not an error: the
// memory stays searchable by keywords and can be embedded later with Reembed.
func (s *Store) embedAndLink(ctx context.Context, id, namespace, content string) (int, bool, error) {
	if s.emb == nil {
		return 0, false, nil
	}
	vecs, err := s.emb.Embed(ctx, []string{content}, embed.Document)
	if err != nil || len(vecs) != 1 {
		return 0, false, nil
	}
	vec := vecs[0]
	if _, err := s.db.Exec(`INSERT INTO vectors(memory_id, model, dims, vec) VALUES (?,?,?,?)
		ON CONFLICT(memory_id) DO UPDATE SET model=excluded.model, dims=excluded.dims, vec=excluded.vec`,
		id, s.emb.Model(), len(vec), encodeVec(vec)); err != nil {
		return 0, false, err
	}
	others, err := s.vectorsIn([]string{namespace}, false, id)
	if err != nil {
		return 0, true, err
	}
	type nb struct {
		id  string
		sim float64
	}
	var nbs []nb
	for _, o := range others {
		if sim := embed.Cosine(vec, o.vec); sim >= s.learn.MinSimilarity {
			nbs = append(nbs, nb{o.id, sim})
		}
	}
	sort.Slice(nbs, func(i, j int) bool { return nbs[i].sim > nbs[j].sim })
	if len(nbs) > s.learn.K {
		nbs = nbs[:s.learn.K]
	}
	for _, n := range nbs {
		if err := s.AddEdge(id, n.id, "similar", n.sim); err != nil {
			return 0, true, err
		}
	}
	return len(nbs), true, nil
}

// coactivate strengthens "coactive" edges between memories returned together,
// once per pair per client per echo window.
func (s *Store) coactivate(client string, ids []string) error {
	if len(ids) > s.learn.CoactiveTopN {
		ids = ids[:s.learn.CoactiveTopN]
	}
	if len(ids) < 2 {
		return nil
	}
	now := Now()
	cutoff := ts(now.Add(-s.learn.EchoWindow))
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			a, b := ids[i], ids[j]
			if a > b {
				a, b = b, a
			}
			var recent int
			if err := tx.QueryRow(`SELECT count(*) FROM coactivations WHERE a = ? AND b = ? AND client = ? AND at > ?`,
				a, b, client, cutoff).Scan(&recent); err != nil {
				return err
			}
			if recent > 0 {
				continue // anti-echo: this client already reinforced this pair recently
			}
			var w float64
			err := tx.QueryRow(`SELECT weight FROM edges WHERE src = ? AND dst = ? AND kind = 'coactive'`, a, b).Scan(&w)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			w += s.learn.Eta * (1 - w)
			if _, err := tx.Exec(`INSERT INTO edges(src, dst, kind, weight, updated_at) VALUES (?,?,'coactive',?,?)
				ON CONFLICT(src, dst, kind) DO UPDATE SET weight = excluded.weight, updated_at = excluded.updated_at`,
				a, b, w, ts(now)); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO coactivations(a, b, client, at) VALUES (?,?,?,?)`, a, b, client, ts(now)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// DecayResult reports what a decay pass changed.
type DecayResult struct {
	Decayed int `json:"decayed"`
	Pruned  int `json:"pruned"`
}

// Decay weakens learned ("coactive") edges by λ and prunes those below the floor.
// The sleep cycle (M5) will call it nightly; until then `engram decay` runs it.
func (s *Store) Decay() (DecayResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DecayResult{}, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE edges SET weight = weight * ? WHERE kind = 'coactive'`, 1-s.learn.Lambda)
	if err != nil {
		return DecayResult{}, err
	}
	decayed, _ := res.RowsAffected()
	res, err = tx.Exec(`DELETE FROM edges WHERE kind = 'coactive' AND weight < ?`, s.learn.Floor)
	if err != nil {
		return DecayResult{}, err
	}
	pruned, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM coactivations WHERE at < ?`, ts(Now().Add(-30*24*time.Hour))); err != nil {
		return DecayResult{}, err
	}
	if err := audit(tx, "system", "decay", "", fmt.Sprintf(`{"decayed":%d,"pruned":%d}`, decayed, pruned)); err != nil {
		return DecayResult{}, err
	}
	return DecayResult{Decayed: int(decayed), Pruned: int(pruned)}, tx.Commit()
}

// ReembedResult reports what Reembed did.
type ReembedResult struct {
	Embedded int    `json:"embedded"`
	Linked   int    `json:"linked"`
	Model    string `json:"model"`
}

// Reembed embeds every current memory that has no vector from the attached model,
// then wires its neighbors. Use it after starting an embedding server or changing models.
func (s *Store) Reembed(ctx context.Context) (ReembedResult, error) {
	if s.emb == nil {
		return ReembedResult{}, fmt.Errorf("no embedder is configured")
	}
	rows, err := s.db.Query(`SELECT m.id, m.namespace, m.content FROM memories m
		LEFT JOIN vectors v ON v.memory_id = m.id AND v.model = ?
		WHERE v.memory_id IS NULL AND m.valid_to IS NULL ORDER BY m.created_at`, s.emb.Model())
	if err != nil {
		return ReembedResult{}, err
	}
	type item struct{ id, ns, content string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.ns, &it.content); err != nil {
			rows.Close()
			return ReembedResult{}, err
		}
		items = append(items, it)
	}
	rows.Close()
	res := ReembedResult{Model: s.emb.Model()}
	for _, it := range items {
		linked, ok, err := s.embedAndLink(ctx, it.id, it.ns, it.content)
		if err != nil {
			return res, err
		}
		if !ok {
			return res, fmt.Errorf("the embedding server stopped responding after %d memories", res.Embedded)
		}
		res.Embedded++
		res.Linked += linked
	}
	return res, nil
}

// neighbor is an edge seen from one endpoint; the graph is traversed undirected.
type neighbor struct {
	id     string
	kind   string
	weight float64
}

// neighbors returns the "similar" and "coactive" edges touching each id.
func (s *Store) neighbors(ids []string) (map[string][]neighbor, error) {
	out := map[string][]neighbor{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := placeholders(len(ids))
	args := append(toAny(ids), toAny(ids)...)
	rows, err := s.db.Query(`SELECT src, dst, kind, weight FROM edges
		WHERE kind IN ('similar','coactive') AND (src IN (`+ph+`) OR dst IN (`+ph+`))`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	for rows.Next() {
		var src, dst, kind string
		var w float64
		if err := rows.Scan(&src, &dst, &kind, &w); err != nil {
			return nil, err
		}
		if set[src] {
			out[src] = append(out[src], neighbor{dst, kind, w})
		}
		if set[dst] {
			out[dst] = append(out[dst], neighbor{src, kind, w})
		}
	}
	return out, rows.Err()
}

// EdgeCounts returns the number of edges of each kind, for stats and tests.
func (s *Store) EdgeCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT kind, count(*) FROM edges GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// EdgeWeight returns the weight of an edge in either direction, or 0 if there is none.
func (s *Store) EdgeWeight(a, b, kind string) (float64, error) {
	var w float64
	err := s.db.QueryRow(`SELECT weight FROM edges WHERE kind = ? AND ((src = ? AND dst = ?) OR (src = ? AND dst = ?))`,
		kind, a, b, b, a).Scan(&w)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return w, err
}

func bgCtx() context.Context { return context.Background() }
