package sleep

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "engram.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetEmbedder(embed.NewHashing(embed.DefaultHashDims))
	if err := st.Pair("claude-code", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	return st
}

func observe(t *testing.T, st *store.Store, in store.ObserveInput) string {
	t.Helper()
	res, err := st.Observe("claude-code", in)
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

// fakeChat answers distillation with a fixed claim and verification with a fixed verdict.
type fakeChat struct {
	fact, verdict string
	calls         []string
}

func (f *fakeChat) Model() string { return "fake" }
func (f *fakeChat) Complete(_ context.Context, system, user string) (string, error) {
	f.calls = append(f.calls, system)
	if strings.Contains(system, "fact-checker") {
		return "```json\n" + `{"verdict": "` + f.verdict + `", "reason": "test"}` + "\n```", nil
	}
	return `Sure! {"fact": "` + f.fact + `", "sources": [1, 2, 3]}`, nil
}

// Three memories that the graph wires into one hub.
func seedHub(t *testing.T, st *store.Store) []string {
	ids := []string{
		observe(t, st, store.ObserveInput{Kind: "episode", Content: "Payout job failed because refunds were not reconciled"}),
		observe(t, st, store.ObserveInput{Kind: "episode", Content: "Fixed the payout job by reconciling refunds first"}),
		observe(t, st, store.ObserveInput{Kind: "episode", Content: "Payout job runs nightly after refunds reconcile"}),
	}
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if err := st.AddEdge(ids[i], ids[j], "coactive", 0.6); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ids
}

func TestCycleDistillsVerifiedFactsAndCanUndo(t *testing.T) {
	st := newStore(t)
	sources := seedHub(t, st)
	chat := &fakeChat{fact: "The payout job needs refunds reconciled first", verdict: "supported"}
	rep, err := Run(context.Background(), st, Options{Chat: chat, SnapshotDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hubs != 1 || len(rep.Distilled) != 1 || rep.Distilled[0].MemoryID == "" || rep.Snapshot == "" {
		t.Fatalf("unexpected report %+v", rep)
	}
	if len(chat.calls) != 2 || !strings.Contains(chat.calls[1], "did not write the claim") {
		t.Fatal("distillation and verification must be separate calls")
	}
	fact := rep.Distilled[0].MemoryID
	for _, src := range sources {
		if w, _ := st.EdgeWeight(fact, src, "derived"); w != 1 {
			t.Fatal("a distilled fact must link to every source")
		}
	}

	// A second night does not distill the same hub again.
	rep2, err := Run(context.Background(), st, Options{Chat: chat})
	if err != nil || len(rep2.Distilled) != 0 {
		t.Fatalf("the same hub must not be distilled twice: %+v %v", rep2, err)
	}

	// Undo removes the fact.
	changes, err := st.Changes(rep.RunID)
	if err != nil || len(changes) != 1 || changes[0].Kind != "distilled" {
		t.Fatalf("want one undoable change, got %+v %v", changes, err)
	}
	if _, err := st.UndoChange(changes[0].ID); err != nil {
		t.Fatal(err)
	}
	if hits, _ := st.RecallSystem(store.RecallInput{Query: "payout job refunds reconciled"}); len(hits) > 0 && hits[0].Memory.ID == fact {
		t.Fatal("undone fact still recalled")
	}
	if _, err := st.UndoChange(changes[0].ID); err == nil {
		t.Fatal("undoing twice should fail")
	}
}

func TestUnsupportedClaimsAreDiscarded(t *testing.T) {
	st := newStore(t)
	seedHub(t, st)
	rep, err := Run(context.Background(), st, Options{Chat: &fakeChat{fact: "Refunds are always instant", verdict: "unsupported"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Distilled) != 1 || rep.Distilled[0].Verdict != "unsupported" || rep.Distilled[0].MemoryID != "" {
		t.Fatalf("an unsupported claim must be reported but not stored: %+v", rep.Distilled)
	}
}

func TestNoChatModelSkipsLanguageSteps(t *testing.T) {
	st := newStore(t)
	seedHub(t, st)
	rep, err := Run(context.Background(), st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Distilled) != 0 || len(rep.Notes) == 0 || !strings.Contains(rep.Notes[0], "skipped") {
		t.Fatalf("want a note that distillation was skipped: %+v", rep)
	}
}

func TestDedupeMergesAndUndoRestores(t *testing.T) {
	st := newStore(t)
	keep := observe(t, st, store.ObserveInput{Kind: "fact", Content: "Branches start with the Jira ticket number"})
	dup := observe(t, st, store.ObserveInput{Kind: "fact", Content: "branches start with the jira ticket number."})
	rep, err := Run(context.Background(), st, Options{Light: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Merged != 1 {
		t.Fatalf("want one merge, got %+v", rep)
	}
	changes, _ := st.Changes(rep.RunID)
	if changes[0].Kind != "merged" || changes[0].MemoryID != dup || changes[0].OtherID != keep {
		t.Fatalf("the newer duplicate should merge into the first: %+v", changes)
	}
	if _, err := st.UndoChange(changes[0].ID); err != nil {
		t.Fatal(err)
	}
	if mems, _ := st.CurrentMemories("me"); len(mems) != 2 {
		t.Fatalf("undo should restore the merged memory, have %d", len(mems))
	}
}

func TestSelfTestFindsDecisions(t *testing.T) {
	st := newStore(t)
	_, err := st.Observe("claude-code", store.ObserveInput{Kind: "decision", Author: "user",
		Content:  "Independent artists get a flagged shop row",
		Decision: &store.Decision{Situation: "How to model independent artists", Choice: "Flagged shop row"}})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), st, Options{Light: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.SelfTest.Tested != 1 || rep.SelfTest.Found != 1 {
		t.Fatalf("self-test should find the decision: %+v", rep.SelfTest)
	}
	// The report is stored with the run.
	runs, _ := st.Runs(1)
	var stored Report
	if err := json.Unmarshal(runs[0].Report, &stored); err != nil || stored.RunID != rep.RunID {
		t.Fatalf("report not stored: %v", err)
	}
}

func TestHoursAndDue(t *testing.T) {
	st := newStore(t)
	h, err := ParseHours("23:00-06:00", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	ny := h.Zone
	if !h.Contains(time.Date(2026, 11, 4, 2, 0, 0, 0, ny)) || h.Contains(time.Date(2026, 11, 4, 12, 0, 0, 0, ny)) {
		t.Fatal("window crossing midnight evaluated wrong")
	}
	if ok, why, _ := Due(st, time.Now()); ok || !strings.Contains(why, "not set") {
		t.Fatalf("without hours the cycle must not run: %s", why)
	}
	if err := SaveHours(st, h); err != nil {
		t.Fatal(err)
	}
	night := time.Date(2026, 11, 5, 3, 0, 0, 0, ny)
	store.Now = func() time.Time { return night }
	defer func() { store.Now = func() time.Time { return time.Now().UTC() } }()
	if ok, why, err := Due(st, night); !ok {
		t.Fatalf("should be due at 3 AM with no activity: %s %v", why, err)
	}
	if _, err := Run(context.Background(), st, Options{}); err != nil {
		t.Fatal(err)
	}
	if ok, why, _ := Due(st, night.Add(30*time.Minute)); ok || !strings.Contains(why, "already ran") {
		t.Fatalf("must run at most once a night: %s", why)
	}
	if _, err := ParseHours("25:00-06:00", "UTC"); err == nil {
		t.Fatal("invalid hours should fail")
	}
}
