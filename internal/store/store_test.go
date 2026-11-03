package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	must(t, s.Pair("claude-code", []string{"me", "team/ink"}, []string{"me", "team/ink"}, ""))
	must(t, s.Pair("codex", []string{"me"}, []string{"me"}, ""))
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code ecp.Code) {
	t.Helper()
	var e *ecp.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want ECP error %d, got %v", code, err)
	}
}

func observe(t *testing.T, s *Store, client string, in ObserveInput) string {
	t.Helper()
	res, err := s.Observe(client, in)
	must(t, err)
	return res.ID
}

func TestObserveThenRecall(t *testing.T) {
	s := newStore(t)
	id := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Namespace: "team/ink",
		Content: "Branches start with the Jira ticket number", Tags: []string{"git"}})
	observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Payout job runs nightly"})

	hits, err := s.Recall("claude-code", RecallInput{Query: "how do I name branches for this ticket?"})
	must(t, err)
	if len(hits) == 0 || hits[0].Memory.ID != id {
		t.Fatalf("expected the branch convention first, got %+v", hits)
	}
	if hits[0].Memory.Provenance.Client != "claude-code" {
		t.Fatalf("provenance not recorded: %+v", hits[0].Memory.Provenance)
	}
}

func TestRecallPersistsAcrossConnections(t *testing.T) {
	db := filepath.Join(t.TempDir(), "engram.db")
	a, err := Open(db)
	must(t, err)
	must(t, a.Pair("claude-code", []string{"me"}, []string{"me"}, ""))
	observe(t, a, "claude-code", ObserveInput{Kind: "fact", Content: "Prod migrations are run by hand"})
	a.Close()

	b, err := Open(db)
	must(t, err)
	defer b.Close()
	hits, err := b.Recall("claude-code", RecallInput{Query: "how are prod migrations run"})
	must(t, err)
	if len(hits) == 0 || !strings.Contains(hits[0].Memory.Content, "migrations") {
		t.Fatalf("memory did not survive reopening: %+v", hits)
	}
}

func TestUnpairedClientIsUnauthorized(t *testing.T) {
	s := newStore(t)
	_, err := s.Recall("stranger", RecallInput{Query: "anything"})
	wantCode(t, err, ecp.Unauthorized)
	if !strings.Contains(err.Error(), "engram pair stranger") {
		t.Fatalf("error should tell the user how to pair: %v", err)
	}
}

func TestNamespaceGrants(t *testing.T) {
	s := newStore(t)
	observe(t, s, "claude-code", ObserveInput{Kind: "fact", Namespace: "team/ink", Content: "Payout job runs nightly"})

	hits, err := s.Recall("codex", RecallInput{Query: "payout job nightly"})
	must(t, err)
	if len(hits) != 0 {
		t.Fatalf("codex must not see team/ink: %+v", hits)
	}
	_, err = s.Recall("codex", RecallInput{Query: "payout", Namespaces: []string{"team/ink"}})
	wantCode(t, err, ecp.ScopeDenied)
	_, err = s.Observe("codex", ObserveInput{Kind: "fact", Namespace: "team/ink", Content: "x y z"})
	wantCode(t, err, ecp.ScopeDenied)
}

func TestDecisionRequiresUserAuthor(t *testing.T) {
	s := newStore(t)
	_, err := s.Observe("claude-code", ObserveInput{Kind: "decision", Content: "Use a flag column",
		Decision: &Decision{Situation: "artists", Choice: "flag"}})
	wantCode(t, err, ecp.InvalidProvenance)

	_, err = s.Observe("claude-code", ObserveInput{Kind: "decision", Author: "user", Content: "Use a flag column"})
	wantCode(t, err, ecp.InvalidParams)

	observe(t, s, "claude-code", ObserveInput{Kind: "decision", Author: "user",
		Content:  "Independent artists get a flagged shop row",
		Decision: &Decision{Situation: "How to model independent artists", Choice: "Flagged shop row",
			Options: []string{"Flagged shop row", "Separate table"}}})
}

func TestSecretsAreRejected(t *testing.T) {
	s := newStore(t)
	_, err := s.Observe("claude-code", ObserveInput{Kind: "fact", Content: "the key is ghp_abcdefghijklmnopqrstuvwxyz0123456789"})
	wantCode(t, err, ecp.ContentRejected)
	st, err := s.Stats()
	must(t, err)
	if st.Memories != 0 {
		t.Fatalf("rejected content was stored")
	}
}

func TestFindSensitive(t *testing.T) {
	cases := map[string]string{
		"card 4242 4242 4242 4242 exp 12/29": "payment card number",
		"order 1234 5678 9012 3456":          "", // fails the Luhn check
		"we picked a flat fee":               "",
		"password: hunter2hunter2":           "password assignment",
	}
	for in, want := range cases {
		if got := FindSensitive(in); got != want {
			t.Errorf("FindSensitive(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSupersedeAndAsOf(t *testing.T) {
	s := newStore(t)
	old := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Namespace: "team/ink",
		Content: "The fee is tiered by service size", ValidFrom: "2026-08-01"})
	res, err := s.Observe("claude-code", ObserveInput{Kind: "fact", Namespace: "team/ink",
		Content: "The fee is a flat percentage plus a fixed amount", ValidFrom: "2026-09-15", Supersedes: old})
	must(t, err)
	if res.Linked != 1 {
		t.Fatalf("expected a supersedes edge")
	}

	now, err := s.Recall("claude-code", RecallInput{Query: "what is the fee"})
	must(t, err)
	if len(now) != 1 || now[0].Memory.ID != res.ID {
		t.Fatalf("current answer should be only the new fee: %+v", now)
	}
	past, err := s.Recall("claude-code", RecallInput{Query: "what is the fee", AsOf: "2026-08-20"})
	must(t, err)
	if len(past) != 1 || past[0].Memory.ID != old {
		t.Fatalf("as of August the old fee should apply: %+v", past)
	}
}

func TestForgetCascadesToDerivedOnly(t *testing.T) {
	s := newStore(t)
	a := observe(t, s, "claude-code", ObserveInput{Kind: "episode", Content: "CI failed on migrations"})
	b := observe(t, s, "claude-code", ObserveInput{Kind: "episode", Content: "Migrations rerun by hand"})
	c := observe(t, s, "claude-code", ObserveInput{Kind: "episode", Content: "Unrelated episode about payouts"})
	fact := observe(t, s, "claude-code", ObserveInput{Kind: "fact", Content: "Prod migrations need a manual step"})
	must(t, s.AddEdge(fact, a, "derived", 1))
	must(t, s.AddEdge(fact, b, "derived", 1))

	n, err := s.Forget("claude-code", []string{a}, true)
	must(t, err)
	if n != 1 {
		t.Fatalf("fact still has another source; removed %d", n)
	}
	n, err = s.Forget("claude-code", []string{b}, true)
	must(t, err)
	if n != 2 {
		t.Fatalf("last source gone, fact should go too; removed %d", n)
	}
	if _, err := s.Get("claude-code", []string{c}); err != nil {
		t.Fatalf("unrelated memory was affected: %v", err)
	}
	_, err = s.Get("claude-code", []string{fact})
	wantCode(t, err, ecp.MemoryNotFound)
}

func TestExpiredGrantIsUnauthorized(t *testing.T) {
	s := newStore(t)
	must(t, s.Pair("temp", []string{"me"}, nil, "2020-01-01T00:00:00Z"))
	_, err := s.Recall("temp", RecallInput{Query: "anything here"})
	wantCode(t, err, ecp.Unauthorized)
}

func TestInvalidNamespaceRejected(t *testing.T) {
	s := newStore(t)
	_, err := s.Observe("claude-code", ObserveInput{Kind: "fact", Content: "hello there", Namespace: "Bad Name"})
	wantCode(t, err, ecp.InvalidParams)
	if err := s.Pair("x", []string{"Bad Name"}, nil, ""); err == nil {
		t.Fatal("pairing with an invalid namespace should fail")
	}
}

func TestTokenize(t *testing.T) {
	cases := map[string]string{
		"How should I name the Branches for tickets?": "name branch ticket",
		"Categories, boxes and passes":                 "category box pass",
		"the class":                                    "class",
	}
	for in, want := range cases {
		if got := strings.Join(Tokenize(in), " "); got != want {
			t.Errorf("Tokenize(%q) = %q, want %q", in, got, want)
		}
	}
}
