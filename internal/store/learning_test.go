package store

import (
	"strings"
	"testing"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
)

func TestBaseLevelRewardsFrequencyAndRecency(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	old := now.Add(-60 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-2 * time.Hour).Format(time.RFC3339)
	if baseLevel(10, old, now) <= baseLevel(0, old, now) {
		t.Fatal("more use should raise activation")
	}
	if baseLevel(0, recent, now) <= baseLevel(0, old, now) {
		t.Fatal("more recent should raise activation")
	}
}

// The M4 done-check: an override changes the next recall.
func TestOverrideChangesNextRecall(t *testing.T) {
	s := newGraphStore(t)
	table := observe(t, s, "claude-code", ObserveInput{Kind: "suggestion",
		Content: "Model independent artists with a separate artists table"})
	q := RecallInput{Query: "how should we model independent artists", Hops: 2}

	before, err := s.Recall("claude-code", q)
	must(t, err)
	if len(before) == 0 || before[0].Memory.ID != table {
		t.Fatalf("the suggestion should be recalled first: %+v", before)
	}
	res, err := s.Feedback("claude-code", FeedbackInput{
		MemoryIDs: []string{table}, Outcome: "overridden",
		Correction: &Decision{Situation: "How to model independent artists", Choice: "Use a flagged shop row instead of a separate table"},
	})
	must(t, err)
	if res.Direction != "weakened" || res.CorrectionID == "" {
		t.Fatalf("unexpected feedback result %+v", res)
	}
	after, err := s.Recall("claude-code", q)
	must(t, err)
	if len(after) == 0 || after[0].Memory.ID != res.CorrectionID || after[0].Memory.Kind != "decision" {
		t.Fatalf("the user's correction should now come first: %+v", after)
	}
	if w, _ := s.EdgeWeight(res.CorrectionID, table, "overrode"); w != 1 {
		t.Fatal("the correction should link to what it overrode")
	}
}

func TestFeedbackOutcomesOutweighApproval(t *testing.T) {
	s := newGraphStore(t)
	a := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Run migrations by hand on prod"})
	b := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Back up the database before prod migrations"})
	_, err := s.Recall("claude-code", RecallInput{Query: "prod migrations"})
	must(t, err)
	start, _ := s.EdgeWeight(a, b, "coactive")

	_, err = s.Feedback("claude-code", FeedbackInput{MemoryIDs: []string{a, b}, Outcome: "accepted", Objective: "succeeded"})
	must(t, err)
	up, _ := s.EdgeWeight(a, b, "coactive")
	if up <= start {
		t.Fatalf("a confirmed pathway should strengthen: %.3f -> %.3f", start, up)
	}
	got, err := s.Get("claude-code", []string{a})
	must(t, err)
	strengthUp := got[0].Strength

	// Approved but failed: the outcome wins.
	r, err := s.Feedback("claude-code", FeedbackInput{MemoryIDs: []string{a, b}, Outcome: "accepted", Objective: "failed"})
	must(t, err)
	if r.Direction != "weakened" {
		t.Fatal("a failed outcome must weaken even when approved")
	}
	down, _ := s.EdgeWeight(a, b, "coactive")
	got, err = s.Get("claude-code", []string{a})
	must(t, err)
	if down >= up || got[0].Strength >= strengthUp {
		t.Fatalf("failure should weaken pathway and strength: edge %.3f -> %.3f, strength %.3f -> %.3f",
			up, down, strengthUp, got[0].Strength)
	}
}

func TestFeedbackValidation(t *testing.T) {
	s := newGraphStore(t)
	a := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "something worth keeping"})
	_, err := s.Feedback("claude-code", FeedbackInput{MemoryIDs: []string{a}, Outcome: "loved"})
	wantCode(t, err, ecp.InvalidParams)
	_, err = s.Feedback("claude-code", FeedbackInput{MemoryIDs: []string{a}, Outcome: "accepted",
		Correction: &Decision{Situation: "x", Choice: "y"}})
	wantCode(t, err, ecp.InvalidParams)
	_, err = s.Feedback("codex", FeedbackInput{MemoryIDs: []string{"mem_missing"}, Outcome: "accepted"})
	wantCode(t, err, ecp.MemoryNotFound)
}

func TestLateralInhibitionDropsNearDuplicates(t *testing.T) {
	s := newGraphStore(t)
	observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Branches start with the Jira ticket number"})
	observe(t, s, "claude-code", ObserveInput{Kind: "episode", Content: "branches start with the jira ticket number!"})
	observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Pull requests need one approving review"})
	hits, err := s.Recall("claude-code", RecallInput{Query: "jira ticket branches", K: 5})
	must(t, err)
	n := 0
	for _, h := range hits {
		if strings.Contains(strings.ToLower(h.Memory.Content), "jira ticket number") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("near-duplicates should collapse to one result, got %d: %+v", n, hits)
	}
}

func TestMultiHopPathwaysAndConfidence(t *testing.T) {
	s := newGraphStore(t)
	a := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Refunds go through Stripe Connect"})
	b := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Ledger entries are written by the payout worker"})
	c := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Finance reconciles accounts every Monday"})
	must(t, s.AddEdge(a, b, "coactive", 0.8))
	must(t, s.AddEdge(b, c, "coactive", 0.8))

	one, err := s.Recall("claude-code", RecallInput{Query: "stripe refunds", Hops: 1})
	must(t, err)
	for _, h := range one {
		if h.Memory.ID == c {
			t.Fatal("one hop must not reach two steps away")
		}
	}
	two, err := s.Recall("claude-code", RecallInput{Query: "stripe refunds", Hops: 2})
	must(t, err)
	var far *Scored
	for i := range two {
		if two[i].Memory.ID == c {
			far = &two[i]
		}
	}
	if far == nil || len(far.Path) != 3 || far.Path[0] != a || far.Path[1] != b {
		t.Fatalf("two hops should reach c via a→b→c: %+v", two)
	}
	if two[0].Confidence <= far.Confidence {
		t.Fatalf("a direct match should be more confident than a two-hop result: %.2f vs %.2f", two[0].Confidence, far.Confidence)
	}
}
