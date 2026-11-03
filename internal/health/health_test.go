package health

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

func TestBoundsTrip(t *testing.T) {
	healthy := Metrics{Saturated: 0.02, AtFloor: 0.1, MaxDegree: 12, DecisionRecall: 1}
	if r := Evaluate(healthy, nil); r.Worst != OK {
		t.Fatalf("healthy metrics flagged: %+v", r.Bounds)
	}
	sick := Metrics{Saturated: 0.4, AtFloor: 0.9, MaxDegree: 500, DecisionRecall: 0.3}
	r := Evaluate(sick, nil)
	breaches := 0
	for _, b := range r.Bounds {
		if b.Status == Breach {
			breaches++
		}
	}
	if breaches != 4 || r.Worst != Breach {
		t.Fatalf("want 4 breaches, got %+v", r.Bounds)
	}
	if r := Evaluate(Metrics{DecisionRecall: -1}, nil); r.Bounds[3].Status != Unknown {
		t.Fatal("no decisions should be not measurable, not a failure")
	}
}

func TestDriftDetection(t *testing.T) {
	steady := Metrics{CoactiveEdges: 50, WeightHist: []float64{0.1, 0.3, 0.3, 0.2, 0.1, 0, 0, 0, 0, 0}}
	history := []Metrics{steady, steady, steady}
	if r := Evaluate(steady, history); len(r.Drift) != 1 || r.Drift[0].Status != OK {
		t.Fatalf("no drift expected: %+v", r.Drift)
	}
	shifted := Metrics{CoactiveEdges: 50, WeightHist: []float64{0, 0, 0, 0, 0, 0, 0.1, 0.2, 0.3, 0.4}}
	if r := Evaluate(shifted, history); r.Drift[0].Status != Breach {
		t.Fatalf("a sharp shift should breach: %+v", r.Drift)
	}
	if r := Evaluate(steady, nil); len(r.Drift) != 0 {
		t.Fatal("drift needs history")
	}
}

func TestSelfTestPassesOnAHealthyStore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "engram.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetEmbedder(embed.NewHashing(embed.DefaultHashDims))
	if err := st.Pair("cli", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	res, err := st.Observe("cli", store.ObserveInput{Kind: "fact", Content: "Branches start with the Jira ticket number"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddGolden("how do I name a branch", res.ID); err != nil {
		t.Fatal(err)
	}
	results, err := SelfTest(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 5 {
		t.Fatalf("want golden + 4 built-in tests, got %d", len(results))
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s (%s) failed: %s", r.Name, r.Kind, r.Detail)
		}
	}
	// The sandbox never touches the real store.
	if mems, _ := st.CurrentMemories("selftest/sandbox"); len(mems) != 0 {
		t.Fatal("self-test wrote into the real memory")
	}
	if _, err := Check(st, -1, true); err != nil {
		t.Fatal(err)
	}
	if h, _ := st.HealthHistory(5); len(h) != 1 {
		t.Fatal("health metrics should be stored")
	}
}
