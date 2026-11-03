package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
)

func newGraphStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	s.SetEmbedder(embed.NewHashing(embed.DefaultHashDims))
	return s
}

func TestKNNWiringStaysInsideNamespaces(t *testing.T) {
	s := newGraphStore(t)
	a := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Namespace: "team/ink",
		Content: "Branches start with the Jira ticket number"})
	res, err := s.Observe("claude-code", ObserveInput{Kind: "fact", Namespace: "team/ink",
		Content: "Name each branch after its Jira ticket"})
	must(t, err)
	if !res.Embedded || res.Linked != 1 {
		t.Fatalf("want the new memory embedded and linked to its neighbor, got %+v", res)
	}
	if w, _ := s.EdgeWeight(res.ID, a, "similar"); w < DefaultLearning.MinSimilarity {
		t.Fatalf("similar edge weight %.2f below threshold", w)
	}
	other, err := s.Observe("claude-code", ObserveInput{Kind: "fact", Namespace: "me",
		Content: "Branches start with the Jira ticket number"})
	must(t, err)
	if other.Linked != 0 {
		t.Fatalf("kNN wiring crossed namespaces: %+v", other)
	}
}

// The M2 done-check: recall surfaces a memory reachable only through a pathway.
func TestRecallFollowsPathways(t *testing.T) {
	s := newGraphStore(t)
	branch := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Branches start with the Jira ticket number"})
	prefix := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Pull request titles use the INKIN prefix"})
	observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "The payout job runs every night"})

	// Without a pathway, the PR convention shares no words with the question.
	hits, err := s.Recall("claude-code", RecallInput{Query: "how do I name a branch", Hops: 1})
	must(t, err)
	for _, h := range hits {
		if h.Memory.ID == prefix {
			t.Fatalf("the PR convention should not match yet: %+v", hits)
		}
	}

	// One task touches both conventions, so they are recalled together and wired.
	_, err = s.Recall("claude-code", RecallInput{Query: "jira ticket and INKIN prefix conventions", Hops: 1})
	must(t, err)
	if w, _ := s.EdgeWeight(branch, prefix, "coactive"); w <= 0 {
		t.Fatal("co-activation did not create a pathway")
	}

	// Now the same question reaches the PR convention through the pathway.
	hits, err = s.Recall("claude-code", RecallInput{Query: "how do I name a branch", Hops: 1})
	must(t, err)
	var found *Scored
	for i := range hits {
		if hits[i].Memory.ID == prefix {
			found = &hits[i]
		}
	}
	if found == nil {
		t.Fatalf("recall did not follow the pathway: %+v", hits)
	}
	if len(found.Path) != 2 || found.Path[0] != branch || found.Path[1] != prefix {
		t.Fatalf("pathway result should carry its path, got %+v", found)
	}
	if hits[0].Memory.ID != branch {
		t.Fatalf("the direct match should still rank first: %+v", hits)
	}

	// With spreading turned off, only direct matches come back.
	hits, err = s.Recall("claude-code", RecallInput{Query: "how do I name a branch", Hops: 0})
	must(t, err)
	for _, h := range hits {
		if h.Memory.ID == prefix {
			t.Fatal("Hops 0 must not follow pathways")
		}
	}
}

func TestHebbianAntiEcho(t *testing.T) {
	s := newGraphStore(t)
	a := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Refunds go through Stripe Connect"})
	b := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Stripe Connect payouts settle nightly"})
	query := RecallInput{Query: "stripe connect refunds payouts"}

	_, err := s.Recall("claude-code", query)
	must(t, err)
	first, _ := s.EdgeWeight(a, b, "coactive")
	if first <= 0 {
		t.Fatal("expected a coactive edge after the first recall")
	}
	_, err = s.Recall("claude-code", query) // same client, same pair, within the echo window
	must(t, err)
	if again, _ := s.EdgeWeight(a, b, "coactive"); again != first {
		t.Fatalf("an agent repeating itself must not reinforce: %.3f -> %.3f", first, again)
	}
	_, err = s.Recall("codex", query) // an independent client does count
	must(t, err)
	if third, _ := s.EdgeWeight(a, b, "coactive"); third <= first {
		t.Fatalf("an independent co-activation should strengthen: %.3f -> %.3f", first, third)
	}
}

func TestDecayWeakensAndPrunes(t *testing.T) {
	s := newGraphStore(t)
	a := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "first memory here"})
	b := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "second memory there"})
	c := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "third unrelated note"})
	must(t, s.AddEdge(a, b, "coactive", 0.5))
	must(t, s.AddEdge(a, c, "coactive", 0.021))

	r, err := s.Decay()
	must(t, err)
	if r.Decayed != 2 || r.Pruned != 1 {
		t.Fatalf("want 2 decayed and 1 pruned, got %+v", r)
	}
	if w, _ := s.EdgeWeight(a, b, "coactive"); w >= 0.5 || w < 0.47 {
		t.Fatalf("decay should apply (1-λ): got %.3f", w)
	}
	if w, _ := s.EdgeWeight(a, c, "coactive"); w != 0 {
		t.Fatal("edges below the floor should be pruned")
	}
}

func TestReembedBackfillsVectors(t *testing.T) {
	s := newStore(t) // no embedder yet, like a fresh install without an embedding server
	observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Branches start with the Jira ticket number"})
	observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Name each branch after its Jira ticket"})
	if _, err := s.Reembed(context.Background()); err == nil {
		t.Fatal("reembed without an embedder should fail clearly")
	}
	s.SetEmbedder(embed.NewHashing(embed.DefaultHashDims))
	r, err := s.Reembed(context.Background())
	must(t, err)
	if r.Embedded != 2 || r.Linked < 1 {
		t.Fatalf("want both memories embedded and linked, got %+v", r)
	}
	r, err = s.Reembed(context.Background())
	must(t, err)
	if r.Embedded != 0 {
		t.Fatal("a second reembed should have nothing to do")
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	db := filepath.Join(t.TempDir(), "engram.db")
	for i := 0; i < 2; i++ {
		s, err := Open(db)
		must(t, err)
		s.Close()
	}
}

func TestVectorsFromAnotherModelAreIgnored(t *testing.T) {
	s := newGraphStore(t)
	observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Branches start with the Jira ticket number"})
	s.SetEmbedder(embed.NewHashing(64)) // a different vector space
	res, err := s.Observe("claude-code", ObserveInput{Kind: "fact", Content: "Name each branch after its Jira ticket"})
	must(t, err)
	if res.Linked != 0 {
		t.Fatal("vectors from different models must never be compared")
	}
}
