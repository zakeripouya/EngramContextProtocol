package store

import (
	"container/heap"
	"runtime"
	"sync"
)

// vecIndex is an exact in-memory vector index over the stored embeddings of the
// current model (milestone M11). Exact search over contiguous float32 vectors is
// fast enough for a personal memory (about 100k vectors in tens of milliseconds)
// and has no recall loss, unlike approximate indexes.
//
// Several processes share the database (agents' servers, hooks, the trigger
// listener, jobs), so the index refreshes incrementally from the vectors table on
// every search: new rows are appended by rowid, and a delete, an update or a
// model change (tracked by a trigger-maintained epoch) rebuilds it.
type vecIndex struct {
	mu      sync.Mutex
	model   string
	epoch   int64 // counters.vectors_epoch when built; deletes and updates bump it
	lastRow int64
	ids     []string
	ns      []string
	vecs    [][]float32
	pos     map[string]int
}

func (s *Store) index() *vecIndex {
	s.idxOnce.Do(func() { s.idx = &vecIndex{} })
	return s.idx
}

// refresh brings the index up to date. The caller holds ix.mu.
func (s *Store) refreshIndex(ix *vecIndex) error {
	if s.emb == nil {
		return nil
	}
	model := s.emb.Model()
	var epoch, maxRow int64
	if err := s.db.QueryRow(`SELECT (SELECT value FROM counters WHERE name = 'vectors_epoch'),
		(SELECT COALESCE(max(rowid), 0) FROM vectors)`).Scan(&epoch, &maxRow); err != nil {
		return err
	}
	if model != ix.model || epoch != ix.epoch || maxRow < ix.lastRow {
		// Reset fields one by one: assigning a new struct would also reset the held mutex.
		ix.model, ix.epoch, ix.lastRow, ix.ids, ix.ns, ix.vecs, ix.pos = model, epoch, 0, nil, nil, nil, map[string]int{}
	}
	if maxRow <= ix.lastRow {
		return nil
	}
	rows, err := s.db.Query(`SELECT v.rowid, v.memory_id, m.namespace, v.vec FROM vectors v JOIN memories m ON m.id = v.memory_id
		WHERE v.model = ? AND v.rowid > ? ORDER BY v.rowid`, model, ix.lastRow)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var row int64
		var id, ns string
		var b []byte
		if err := rows.Scan(&row, &id, &ns, &b); err != nil {
			return err
		}
		if p, ok := ix.pos[id]; ok {
			ix.vecs[p] = decodeVec(b)
		} else {
			ix.pos[id] = len(ix.ids)
			ix.ids = append(ix.ids, id)
			ix.ns = append(ix.ns, ns)
			ix.vecs = append(ix.vecs, decodeVec(b))
		}
		ix.lastRow = row
	}
	if err := rows.Err(); err != nil {
		return err
	}
	ix.lastRow = max(ix.lastRow, maxRow) // rows of other models are skipped, not reloaded
	return nil
}

type hit struct {
	id  string
	sim float64
}

type minHeap []hit

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].sim < h[j].sim }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)        { *h = append(*h, x.(hit)) }
func (h *minHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// searchVectors returns up to k ids most similar to q with similarity ≥ minSim, in the
// allowed namespaces (nil means all), excluding one id. It does not check validity:
// callers filter archived memories when they load rows.
func (s *Store) searchVectors(q []float32, allowed map[string]bool, k int, minSim float64, exclude string) ([]hit, error) {
	if s.emb == nil || q == nil || k <= 0 {
		return nil, nil
	}
	ix := s.index()
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if err := s.refreshIndex(ix); err != nil {
		return nil, err
	}
	n := len(ix.ids)
	if n == 0 {
		return nil, nil
	}
	workers := min(runtime.GOMAXPROCS(0), max(1, n/4096))
	chunk := (n + workers - 1) / workers
	parts := make([]minHeap, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := w*chunk, min(n, (w+1)*chunk)
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			h := minHeap{}
			for i := lo; i < hi; i++ {
				if ix.ids[i] == exclude || (allowed != nil && !allowed[ix.ns[i]]) || len(ix.vecs[i]) != len(q) {
					continue
				}
				sim := float64(dot32(q, ix.vecs[i]))
				if sim < minSim {
					continue
				}
				if h.Len() < k {
					heap.Push(&h, hit{ix.ids[i], sim})
				} else if sim > h[0].sim {
					h[0] = hit{ix.ids[i], sim}
					heap.Fix(&h, 0)
				}
			}
			parts[w] = h
		}(w, lo, hi)
	}
	wg.Wait()
	all := minHeap{}
	for _, p := range parts {
		for _, x := range p {
			if all.Len() < k {
				heap.Push(&all, x)
			} else if x.sim > all[0].sim {
				all[0] = x
				heap.Fix(&all, 0)
			}
		}
	}
	out := make([]hit, all.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(&all).(hit)
	}
	return out, nil
}

// dot32 is the dot product of two equal-length unit vectors (their cosine), in
// float32 with four accumulators: about three times faster than float64 on the
// index scan, and precise enough for ranking.
func dot32(a, b []float32) float32 {
	b = b[:len(a)]
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return s0 + s1 + s2 + s3
}
