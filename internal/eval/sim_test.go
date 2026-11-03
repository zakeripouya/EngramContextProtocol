package eval

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/model"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// A simulated user over 30 days. For every topic the user wants the decision
// they made, but notes about the topic share more words with their questions,
// so the hand-set formula often ranks the notes first. A ranker trained on the
// feedback should learn to prefer decisions.
type world struct {
	st       *store.Store
	decision map[string]string // topic -> decision memory id
	topics   []string
	rng      *rand.Rand
	clock    time.Time
}

var topicNames = []string{"payouts", "consent forms", "booking deposits", "artist profiles", "shop subdomains",
	"refund policy", "id verification", "sms reminders", "tax reports", "staff roles", "gift cards", "waitlists",
	"portfolio uploads", "chargebacks", "multi-session billing", "walk-ins"}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{decision: map[string]string{}, rng: rand.New(rand.NewSource(42)), clock: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)}
	prev := store.Now
	store.Now = func() time.Time { return w.clock }
	t.Cleanup(func() { store.Now = prev })
	path := ":memory:"
	if p := os.Getenv("ENGRAM_SIM_DB"); p != "" && t.Name() == "TestEvaluateAndLearnFromASimulatedUser" {
		path = p // keep the simulated history, e.g. to try engram eval and the XGBoost worker on it
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetEmbedder(embed.NewHashing(embed.DefaultHashDims))
	if err := st.Pair("agent", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	w.st = st
	for i, topic := range topicNames {
		w.topics = append(w.topics, topic)
		for j := 0; j < 2; j++ {
			if _, err := st.Observe("agent", store.ObserveInput{Kind: "fact",
				Content: fmt.Sprintf("Notes on %s: how %s works today, %s questions, %s edge cases (%d)", topic, topic, topic, topic, j)}); err != nil {
				t.Fatal(err)
			}
		}
		res, err := st.Observe("agent", store.ObserveInput{Kind: "decision", Author: "user", Content: fmt.Sprintf("Decided for %s: option %d", topic, i),
			Decision: &store.Decision{Situation: "choosing how " + topic + " should work", Choice: fmt.Sprintf("option %d", i)}})
		if err != nil {
			t.Fatal(err)
		}
		w.decision[topic] = res.ID
	}
	return w
}

// day runs n recalls with feedback, as the user would give it.
func (w *world) day(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		w.clock = w.clock.Add(time.Duration(30+w.rng.Intn(60)) * time.Minute)
		topic := w.topics[w.rng.Intn(len(w.topics))]
		q := "what about " + topic + " questions"
		res, err := w.st.Recall("agent", store.RecallInput{Query: q, K: 5, Hops: 2})
		if err != nil {
			t.Fatal(err)
		}
		want := w.decision[topic]
		found := false
		for _, r := range res {
			if r.Memory.ID == want {
				found = true
			}
		}
		if found {
			obj := ""
			if w.rng.Float64() < 0.5 {
				obj = "succeeded"
			}
			if _, err := w.st.Feedback("agent", store.FeedbackInput{MemoryIDs: []string{want}, Outcome: "accepted", Objective: obj, Query: q}); err != nil {
				t.Fatal(err)
			}
		}
		if len(res) > 0 && res[0].Memory.ID != want && w.rng.Float64() < 0.6 {
			if _, err := w.st.Feedback("agent", store.FeedbackInput{MemoryIDs: []string{res[0].Memory.ID}, Outcome: "overridden", Query: q}); err != nil {
				t.Fatal(err)
			}
		}
	}
	w.clock = w.clock.Add(14 * time.Hour)
}

func TestEvaluateAndLearnFromASimulatedUser(t *testing.T) {
	w := newWorld(t)
	for d := 0; d < 30; d++ {
		w.day(t, 8)
	}
	st := w.st

	logged, labeled, err := LabeledQueries(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(logged) != 240 || len(labeled) < 120 {
		t.Fatalf("logged %d recalls, %d labeled", len(logged), len(labeled))
	}
	for _, r := range logged[:3] {
		if r.Variant != "default" || len(r.Results) == 0 || r.Results[0].Features.HandScore == 0 {
			t.Fatalf("recall log entry lacks features: %+v", r)
		}
	}

	rep, err := EvaluateRanking(st, RankingOptions{K: 5})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]SystemResult{}
	for _, s := range rep.Systems {
		names[s.Name] = s
	}
	for _, n := range []string{"served", "hand", "bm25", "embeddings", "hybrid", "prior"} {
		if _, ok := names[n]; !ok {
			t.Fatalf("system %s missing from %v", n, rep.Systems)
		}
	}
	if rep.Dataset.Test < MinQueries || names["served"].NDCG.Mean <= 0 || names["bm25"].VsHand == nil {
		t.Fatalf("report %+v", rep.Dataset)
	}
	if rep.Calibration == nil || rep.Calibration.Raw.N == 0 {
		t.Fatal("no calibration measured")
	}

	// Learn a ranker on the older queries and test it on the newer ones.
	train, _, split := SplitByTime(labeled, time.Time{}, 0.3)
	m, err := model.TrainPairwise(model.Names, Groups(train), model.TrainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Explain(4), "dec") {
		t.Errorf("the ranker should lean on the decision flag: %s", m.Explain(6))
	}
	params, _ := json.Marshal(m)
	id, err := st.SaveModel(store.ModelRecord{Kind: model.KindRanker, Algo: model.AlgoPairwise, Features: model.Names, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	rep2, err := EvaluateRanking(st, RankingOptions{K: 5, Split: split, Models: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	gate := RankerGate(rep2, "model:"+id)
	var learned SystemResult
	for _, s := range rep2.Systems {
		if s.Name == "model:"+id {
			learned = s
		}
	}
	if learned.VsHand == nil || learned.VsHand.Diff <= 0 || !gate.Pass {
		t.Fatalf("the learned ranker should beat the hand-set formula here: %+v gate %+v", learned.VsHand, gate)
	}

	// Activated, it serves real recalls, and the log says so.
	if err := st.ActivateModel(id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recall("agent", store.RecallInput{Query: "what about payouts questions", K: 5, Hops: 2}); err != nil {
		t.Fatal(err)
	}
	last, _ := st.RecallLog(w.clock.Add(-time.Minute), time.Time{}, false)
	if len(last) == 0 || !strings.Contains(last[len(last)-1].Variant, "+ranker:"+id) {
		t.Fatalf("the recall was not served by the active ranker: %+v", last)
	}
	if n, err := st.DeactivateModels(model.KindRanker); err != nil || n != 1 {
		t.Fatalf("deactivate: %d %v", n, err)
	}

	// Calibration: fit on train, judge on test.
	p, y := CalibrationPairs(train)
	cal, err := model.FitIsotonic(p, y)
	if err != nil {
		t.Fatal(err)
	}
	cp, _ := json.Marshal(cal)
	cid, err := st.SaveModel(store.ModelRecord{Kind: model.KindCalibrator, Algo: model.AlgoIsotonic, Params: cp})
	if err != nil {
		t.Fatal(err)
	}
	rep3, err := EvaluateRanking(st, RankingOptions{K: 5, Split: split, Calibrator: cid})
	if err != nil {
		t.Fatal(err)
	}
	if c := rep3.Calibration; c == nil || c.Calibrated == nil || c.Calibrated.ECE >= c.Raw.ECE {
		t.Fatalf("calibration should lower ECE on held-out judgments: %+v", c)
	}

	// Replaying every ablation runs without errors.
	o := RankingOptions{K: 5, Split: split, Replay: st, ReplayLabel: "current store",
		Configs: append(append(append([]string{"hand"}, store.Baselines...), store.Ablations...), store.Additions...)}
	rep4, err := EvaluateRanking(st, o)
	if err != nil {
		t.Fatal(err)
	}
	replayed := 0
	for _, s := range rep4.Systems {
		if s.Mode == "replay" {
			replayed++
			if strings.Contains(s.Note, "failed") && s.Name != "replay:no-semantic" {
				t.Errorf("%s: %s", s.Name, s.Note)
			}
		}
	}
	if replayed != len(o.Configs) {
		t.Fatalf("replayed %d of %d configurations", replayed, len(o.Configs))
	}

	// Export for the XGBoost worker.
	var buf strings.Builder
	n, err := ExportRows(&buf, train, nil)
	if err != nil || n == 0 || !strings.Contains(buf.String(), `"featureNames"`) {
		t.Fatalf("export: %d rows, %v", n, err)
	}
}

func TestOnlineExperiment(t *testing.T) {
	w := newWorld(t)
	if err := w.st.StartExperiment(store.Experiment{Name: "spread", Variant: store.RecallConfig{NoSpreading: true}, Share: 0.5}); err != nil {
		t.Fatal(err)
	}
	if err := w.st.StartExperiment(store.Experiment{Name: "bad name", Share: 0.5}); err == nil {
		t.Fatal("a name with a space should be refused")
	}
	for d := 0; d < 10; d++ {
		w.day(t, 8)
	}
	rep, err := AnalyzeExperiment(w.st, "spread", 1)
	if err != nil {
		t.Fatal(err)
	}
	c, v := rep.Arms[0], rep.Arms[1]
	if c.Recalls+v.Recalls != 80 || c.Recalls < 20 || v.Recalls < 20 || rep.AcceptDiff == nil {
		t.Fatalf("arms %+v / %+v", c, v)
	}
	if err := w.st.StopExperiment(); err != nil {
		t.Fatal(err)
	}
	if e, _ := w.st.CurrentExperiment(); e != nil {
		t.Fatal("experiment still running")
	}
}

func TestFeedbackLinksToItsRecall(t *testing.T) {
	w := newWorld(t)
	res, err := w.st.Recall("agent", store.RecallInput{Query: "what about payouts questions", K: 5})
	if err != nil {
		t.Fatal(err)
	}
	w.clock = w.clock.Add(time.Minute)
	if _, err := w.st.Feedback("agent", store.FeedbackInput{MemoryIDs: []string{res[1].Memory.ID}, Outcome: "accepted", Objective: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	// Feedback three hours later is not linked to it.
	w.clock = w.clock.Add(3 * time.Hour)
	if _, err := w.st.Feedback("agent", store.FeedbackInput{MemoryIDs: []string{res[0].Memory.ID}, Outcome: "overridden"}); err != nil {
		t.Fatal(err)
	}
	log, _ := w.st.RecallLog(time.Time{}, time.Time{}, true)
	if len(log) != 1 || log[0].Grades[res[1].Memory.ID] != 2 || len(log[0].Grades) != 1 {
		t.Fatalf("grades = %+v", log)
	}
	// Quiet recalls (attend, push) and historical recalls are not logged.
	if _, err := w.st.RecallQuiet("agent", store.RecallInput{Query: "payouts"}); err != nil {
		t.Fatal(err)
	}
	all, _ := w.st.RecallLog(time.Time{}, time.Time{}, false)
	if len(all) != 1 {
		t.Fatalf("logged %d recalls", len(all))
	}
}
