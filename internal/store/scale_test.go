package store

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
)

// Above FullScanLimit, recall ranks candidates from the full-text and vector
// indexes. These tests force that path with a small limit.
func TestCandidateRecallAtScale(t *testing.T) {
	old := FullScanLimit
	FullScanLimit = 20
	defer func() { FullScanLimit = old }()

	path := filepath.Join(t.TempDir(), "engram.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetEmbedder(embed.NewHashing(embed.DefaultHashDims))
	if err := s.Pair("cli", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	obs := func(content string) string {
		r, err := s.Observe("cli", ObserveInput{Kind: "fact", Content: content})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	target := obs("Deploys use blue green rollouts behind the load balancer") // the oldest memory
	runbook := obs("The rollback runbook lives in the ops wiki")
	if err := s.AddEdge(target, runbook, "coactive", 0.9); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 120; i++ {
		obs(fmt.Sprintf("Filler note %d about gardening, tomatoes and compost", i))
	}

	hits, err := s.Recall("cli", RecallInput{Query: "how do deploys roll out", Hops: 2})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.Memory.ID] = true
	}
	if len(hits) == 0 || hits[0].Memory.ID != target {
		t.Fatalf("the oldest memory should be found through the full-text index: %+v", hits)
	}
	if !got[runbook] {
		t.Fatalf("a neighbor that was not a candidate should be reached through its pathway: %+v", hits)
	}

	// A memory written by another process (another Store on the same file) is
	// visible to this process's indexes on the next recall.
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	other.SetEmbedder(embed.NewHashing(embed.DefaultHashDims))
	if _, err := other.Observe("cli", ObserveInput{Kind: "fact", Content: "Kubernetes canary analysis gates every release"}); err != nil {
		t.Fatal(err)
	}
	other.Close()
	hits, err = s.Recall("cli", RecallInput{Query: "kubernetes canary analysis"})
	if err != nil || len(hits) == 0 || !strings.Contains(hits[0].Memory.Content, "canary") {
		t.Fatalf("another process's memory should be recalled: %+v %v", hits, err)
	}

	// Archived memories never come back from the index.
	if err := s.Archive(target, ""); err != nil {
		t.Fatal(err)
	}
	hits, _ = s.Recall("cli", RecallInput{Query: "blue green rollouts"})
	for _, h := range hits {
		if h.Memory.ID == target {
			t.Fatal("an archived memory was recalled")
		}
	}
}

// TestRecallLatency100k measures recall at 100,000 memories. It takes about a
// minute to build, so it runs only with ENGRAM_SCALE=1.
func TestRecallLatency100k(t *testing.T) {
	if os.Getenv("ENGRAM_SCALE") == "" {
		t.Skip("set ENGRAM_SCALE=1 to measure recall at 100k memories")
	}
	const n = 100_000
	path := filepath.Join(t.TempDir(), "engram.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	emb := embed.NewHashing(embed.DefaultHashDims)
	s.SetEmbedder(emb)
	if err := s.Pair("cli", []string{"*"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	words := strings.Fields(`deploy branch ticket review release payout stripe invoice tattoo artist shop booking
		calendar schema migration index cache queue retry timeout backoff webhook token secret pricing fee tier
		dashboard graph memory recall sleep dream trait decision fact episode agent codex claude cursor test build`)
	rng := rand.New(rand.NewSource(1))
	start := time.Now()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	now := Now()
	for i := 0; i < n; i++ {
		var b strings.Builder
		for w := 0; w < 8; w++ {
			b.WriteString(words[rng.Intn(len(words))])
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "note %d", i)
		content := b.String()
		id := fmt.Sprintf("mem_%016x", i)
		at := ts(now.Add(-time.Duration(n-i) * time.Minute))
		if _, err := tx.Exec(`INSERT INTO memories(id, kind, content, tags, scope, namespace, strength, agent, client,
			author, created_at, valid_from) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, "fact", content, "[]", "private", fmt.Sprintf("ns%d", i%5), 0.3, "bench", "cli", "agent", at, at); err != nil {
			t.Fatal(err)
		}
		v, _ := emb.Embed(bgCtx(), []string{content}, embed.Document)
		if _, err := tx.Exec(`INSERT INTO vectors(memory_id, model, dims, vec) VALUES (?,?,?,?)`, id, emb.Model(), len(v[0]), encodeVec(v[0])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("built %d memories in %s", n, time.Since(start).Round(time.Second))

	queries := []string{"stripe payout retry backoff", "tattoo artist booking calendar", "schema migration index",
		"release review branch ticket", "webhook token secret", "graph memory recall", "pricing fee tier", "codex claude agent"}
	if _, err := s.RecallQuiet("cli", RecallInput{Query: "warm up the index", Hops: 2}); err != nil {
		t.Fatal(err)
	}
	var times []time.Duration
	for round := 0; round < 3; round++ {
		for _, q := range queries {
			t0 := time.Now()
			hits, err := s.RecallQuiet("cli", RecallInput{Query: q, Hops: 2})
			if err != nil || len(hits) == 0 {
				t.Fatalf("%q: %v %d", q, err, len(hits))
			}
			times = append(times, time.Since(t0))
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	p50, p95 := times[len(times)/2], times[len(times)*95/100]
	t.Logf("recall at %d memories: p50 %s, p95 %s (%d queries, 2 hops)", n, p50.Round(time.Millisecond), p95.Round(time.Millisecond), len(times))
	if p50 > 100*time.Millisecond {
		t.Errorf("p50 %s exceeds the 100 ms target", p50)
	}
}
