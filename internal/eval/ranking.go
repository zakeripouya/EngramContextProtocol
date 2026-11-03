package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/model"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// RankingOptions controls a ranking evaluation.
type RankingOptions struct {
	K            int       // cutoff for Recall@k, NDCG@k and precision; default 5
	Split        time.Time // queries at or after it are the test set; zero = TestFraction
	TestFraction float64   // newest share of queries held out; default 0.3
	Models       []string  // model ids (or "active") to evaluate as rankers
	Calibrator   string    // a calibrator model id (or "active") to evaluate
	Replay       *store.Store
	ReplayLabel  string   // what the replay store is, e.g. "snapshot 2026-10-01" or "current store (may leak)"
	Configs      []string // configurations to replay (names from store.NamedConfig)
	Seed         int64
}

func (o *RankingOptions) defaults() {
	if o.K <= 0 {
		o.K = 5
	}
	if o.TestFraction <= 0 || o.TestFraction >= 1 {
		o.TestFraction = 0.3
	}
	if o.Seed == 0 {
		o.Seed = 1
	}
}

// DatasetInfo describes what an evaluation ran on.
type DatasetInfo struct {
	Logged    int       `json:"loggedRecalls"`
	Labeled   int       `json:"labeledQueries"` // recalls with at least one positive judgment
	Train     int       `json:"train"`
	Test      int       `json:"test"`
	Split     time.Time `json:"split"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Judgments int       `json:"judgments"`
	Positives int       `json:"positives"`
}

// SystemResult is one ranking system measured on the test queries.
type SystemResult struct {
	Name      string      `json:"name"`
	Mode      string      `json:"mode"` // rerank (logged candidates) or replay (re-run recall)
	Queries   int         `json:"queries"`
	RecallAtK Estimate    `json:"recallAtK"`
	Precision Estimate    `json:"precisionAtK"`
	MRR       Estimate    `json:"mrr"`
	NDCG      Estimate    `json:"ndcgAtK"`
	VsHand    *Comparison `json:"vsHand,omitempty"` // NDCG@k difference against the hand-set formula
	Note      string      `json:"note,omitempty"`
	ndcg      []float64
}

// CalibrationResult compares raw and calibrated confidence on held-out judgments.
type CalibrationResult struct {
	Raw        Calibration  `json:"raw"`
	Calibrated *Calibration `json:"calibrated,omitempty"`
	Model      string       `json:"model,omitempty"`
}

// RankingReport is the result of EvaluateRanking.
type RankingReport struct {
	K           int                `json:"k"`
	Dataset     DatasetInfo        `json:"dataset"`
	Systems     []SystemResult     `json:"systems"`
	Calibration *CalibrationResult `json:"calibration,omitempty"`
	Warnings    []string           `json:"warnings,omitempty"`
}

// MinQueries is the number of labeled test queries below which results are
// reported with a warning: intervals are too wide to decide anything.
const MinQueries = 30

// LabeledQueries loads logged recalls that got at least one positive judgment, oldest first.
func LabeledQueries(st *store.Store) (all []store.LoggedRecall, labeled []store.LoggedRecall, err error) {
	all, err = st.RecallLog(time.Time{}, time.Time{}, false)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range all {
		for _, g := range r.Grades {
			if g > 0 {
				labeled = append(labeled, r)
				break
			}
		}
	}
	return all, labeled, nil
}

// SplitByTime divides queries (oldest first) into train and test.
func SplitByTime(qs []store.LoggedRecall, split time.Time, testFraction float64) (train, test []store.LoggedRecall, at time.Time) {
	if len(qs) == 0 {
		return nil, nil, split
	}
	if split.IsZero() {
		cut := int(math.Floor(float64(len(qs)) * (1 - testFraction)))
		cut = max(0, min(cut, len(qs)-1))
		split = qs[cut].At
	}
	for _, q := range qs {
		if q.At.Before(split) {
			train = append(train, q)
		} else {
			test = append(test, q)
		}
	}
	return train, test, split
}

func relevantCount(q store.LoggedRecall) int {
	n := 0
	for _, g := range q.Grades {
		if g > 0 {
			n++
		}
	}
	return n
}

// rerank orders a logged recall's results by a score of their features.
func rerank(q store.LoggedRecall, score func(model.Features) float64) Ranked {
	res := append([]store.LoggedResult{}, q.Results...)
	sort.SliceStable(res, func(a, b int) bool {
		sa, sb := score(res[a].Features), score(res[b].Features)
		if sa != sb {
			return sa > sb
		}
		return res[a].Rank < res[b].Rank
	})
	r := Ranked{Grades: q.Grades, Relevant: relevantCount(q)}
	for _, x := range res {
		r.IDs = append(r.IDs, x.ID)
	}
	return r
}

// rerankers are the systems evaluated on logged candidates without re-running recall.
func rerankers() []struct {
	name, note string
	score      func(model.Features) float64
} {
	return []struct {
		name, note string
		score      func(model.Features) float64
	}{
		{"hand", "the hand-set formula (relevance, prior, pathways)", func(f model.Features) float64 { return f.HandScore }},
		{"bm25", "keywords only", func(f model.Features) float64 { return f.Keyword }},
		{"embeddings", "meaning only", func(f model.Features) float64 { return f.Semantic }},
		{"hybrid", "keywords and meaning, no prior or pathways", func(f model.Features) float64 { return f.Relevance }},
		{"prior", "strength and use only", func(f model.Features) float64 { return f.Prior }},
	}
}

func (o RankingOptions) measure(name, mode, note string, rankings []Ranked) SystemResult {
	var rk, pr, mr, nd []float64
	for _, r := range rankings {
		rk = append(rk, RecallAt(r, o.K))
		pr = append(pr, PrecisionAt(r, o.K))
		mr = append(mr, MRR(r))
		nd = append(nd, NDCGAt(r, o.K))
	}
	return SystemResult{Name: name, Mode: mode, Note: note, Queries: len(rankings),
		RecallAtK: Bootstrap(rk, o.Seed), Precision: Bootstrap(pr, o.Seed+1), MRR: Bootstrap(mr, o.Seed+2),
		NDCG: Bootstrap(nd, o.Seed+3), ndcg: nd}
}

// LoadScorer loads a ranker model by id or "active".
func LoadScorer(st *store.Store, ref string) (model.Scorer, string, error) {
	var rec store.ModelRecord
	var err error
	if ref == "active" {
		ms, err := st.Models(model.KindRanker)
		if err != nil {
			return nil, "", err
		}
		for _, m := range ms {
			if m.Status == "active" {
				rec = m
			}
		}
		if rec.ID == "" {
			return nil, "", nil
		}
	} else if rec, err = st.ModelByID(ref); err != nil {
		return nil, "", err
	}
	m, err := model.Load(rec.Algo, rec.Params)
	if err != nil {
		return nil, "", err
	}
	sc, ok := m.(model.Scorer)
	if !ok {
		return nil, "", fmt.Errorf("model %s cannot rank", rec.ID)
	}
	return sc, rec.ID, nil
}

// EvaluateRanking measures the served rankings, the hand-set formula, baselines,
// stored models and (with a replay store) re-run configurations on the newest
// labeled queries, each with bootstrap intervals and a paired comparison
// against the hand-set formula.
func EvaluateRanking(st *store.Store, o RankingOptions) (RankingReport, error) {
	o.defaults()
	rep := RankingReport{K: o.K}
	all, labeled, err := LabeledQueries(st)
	if err != nil {
		return rep, err
	}
	train, test, split := SplitByTime(labeled, o.Split, o.TestFraction)
	rep.Dataset = DatasetInfo{Logged: len(all), Labeled: len(labeled), Train: len(train), Test: len(test), Split: split}
	if len(labeled) > 0 {
		rep.Dataset.From, rep.Dataset.To = labeled[0].At, labeled[len(labeled)-1].At
	}
	for _, q := range labeled {
		for _, g := range q.Grades {
			rep.Dataset.Judgments++
			if g > 0 {
				rep.Dataset.Positives++
			}
		}
	}
	if len(test) == 0 {
		rep.Warnings = append(rep.Warnings, "no labeled queries yet: use Engram, then give feedback (accept or override) on what recall returns")
		return rep, nil
	}
	if len(test) < MinQueries {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("only %d held-out queries; intervals are wide and differences are rarely significant below %d", len(test), MinQueries))
	}

	// What users were actually shown.
	var served []Ranked
	for _, q := range test {
		r := Ranked{Grades: q.Grades, Relevant: relevantCount(q)}
		for _, x := range q.Results {
			r.IDs = append(r.IDs, x.ID)
		}
		served = append(served, r)
	}
	rep.Systems = append(rep.Systems, o.measure("served", "rerank", "what recall actually returned", served))

	for _, rr := range rerankers() {
		var rs []Ranked
		for _, q := range test {
			rs = append(rs, rerank(q, rr.score))
		}
		rep.Systems = append(rep.Systems, o.measure(rr.name, "rerank", rr.note, rs))
	}
	for _, ref := range o.Models {
		sc, id, err := LoadScorer(st, ref)
		if err != nil {
			return rep, err
		}
		if sc == nil {
			rep.Warnings = append(rep.Warnings, "no active ranker model")
			continue
		}
		var rs []Ranked
		for _, q := range test {
			rs = append(rs, rerank(q, func(f model.Features) float64 { return sc.Score(f.Vector()) }))
		}
		rep.Systems = append(rep.Systems, o.measure("model:"+id, "rerank", "learned ranker", rs))
	}

	if o.Replay != nil {
		for _, name := range o.Configs {
			cfg, err := store.NamedConfig(name)
			if err != nil {
				return rep, err
			}
			var rs []Ranked
			skipped := 0
			for _, q := range test {
				res, err := o.Replay.RecallQuiet(q.Client, store.RecallInput{Query: q.Query, K: max(o.K, q.K), Hops: q.Hops,
					Namespaces: q.Namespaces, Config: &cfg})
				if err != nil {
					skipped++
					res = nil
				}
				r := Ranked{Grades: q.Grades, Relevant: relevantCount(q)}
				for _, x := range res {
					r.IDs = append(r.IDs, x.Memory.ID)
				}
				rs = append(rs, r)
			}
			note := o.ReplayLabel
			if skipped > 0 {
				note += fmt.Sprintf("; %d queries failed and count as misses", skipped)
			}
			rep.Systems = append(rep.Systems, o.measure("replay:"+name, "replay", note, rs))
		}
	}

	// Paired comparison of every system against the hand-set formula on NDCG@k.
	var hand []float64
	for _, s := range rep.Systems {
		if s.Name == "hand" {
			hand = s.ndcg
		}
	}
	for i := range rep.Systems {
		if rep.Systems[i].Name != "hand" && len(rep.Systems[i].ndcg) == len(hand) {
			c := Paired(rep.Systems[i].ndcg, hand, o.Seed+int64(i)+10)
			rep.Systems[i].VsHand = &c
		}
	}

	// Calibration of the confidence recall reports, on held-out judgments.
	p, y := CalibrationPairs(test)
	if len(p) > 0 {
		cr := &CalibrationResult{Raw: Calibrate(p, y, 10)}
		if o.Calibrator != "" {
			cal, id, err := loadCalibrator(st, o.Calibrator)
			if err != nil {
				return rep, err
			}
			if cal != nil {
				q := make([]float64, len(p))
				for i := range p {
					q[i] = cal.Calibrate(p[i])
				}
				c := Calibrate(q, y, 10)
				cr.Calibrated, cr.Model = &c, id
			}
		}
		rep.Calibration = cr
	}
	return rep, nil
}

// CalibrationPairs returns (raw confidence, outcome) for every judged result.
func CalibrationPairs(qs []store.LoggedRecall) (p, y []float64) {
	for _, q := range qs {
		for _, r := range q.Results {
			g, ok := q.Grades[r.ID]
			if !ok {
				continue
			}
			conf := r.Features.HandConf
			if conf == 0 && r.Features.HandScore == 0 {
				conf = r.Confidence // logged before features existed
			}
			p = append(p, conf)
			if g > 0 {
				y = append(y, 1)
			} else {
				y = append(y, 0)
			}
		}
	}
	return p, y
}

func loadCalibrator(st *store.Store, ref string) (model.Calibrator, string, error) {
	var rec store.ModelRecord
	if ref == "active" {
		ms, err := st.Models(model.KindCalibrator)
		if err != nil {
			return nil, "", err
		}
		for _, m := range ms {
			if m.Status == "active" {
				rec = m
			}
		}
		if rec.ID == "" {
			return nil, "", nil
		}
	} else {
		var err error
		if rec, err = st.ModelByID(ref); err != nil {
			return nil, "", err
		}
	}
	m, err := model.Load(rec.Algo, rec.Params)
	if err != nil {
		return nil, "", err
	}
	c, ok := m.(model.Calibrator)
	if !ok {
		return nil, "", fmt.Errorf("model %s is not a calibrator", rec.ID)
	}
	return c, rec.ID, nil
}

// Groups turns logged recalls into training groups: every shown result is an
// example, graded by its judgment, or 0 when it was shown and not judged.
func Groups(qs []store.LoggedRecall) []model.Group {
	var out []model.Group
	for _, q := range qs {
		var g model.Group
		for _, r := range q.Results {
			g.X = append(g.X, r.Features.Vector())
			g.Grade = append(g.Grade, q.Grades[r.ID])
		}
		if len(g.X) > 1 {
			out = append(out, g)
		}
	}
	return out
}

// ExportRows writes the dataset as JSON lines, one per shown result, for
// training outside Engram (the XGBoost worker): {"qid", "split", "grade", "features"}.
func ExportRows(w io.Writer, train, test []store.LoggedRecall) (int, error) {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	n := 0
	header := map[string]any{"featureNames": model.Names}
	if err := enc.Encode(header); err != nil {
		return 0, err
	}
	for split, qs := range map[string][]store.LoggedRecall{"train": train, "test": test} {
		for _, q := range qs {
			for _, r := range q.Results {
				row := map[string]any{"qid": q.ID, "split": split, "at": q.At, "grade": q.Grades[r.ID], "features": r.Features.Vector()}
				if err := enc.Encode(row); err != nil {
					return n, err
				}
				n++
			}
		}
	}
	return n, bw.Flush()
}

// GateResult says whether a candidate may replace what it would replace.
type GateResult struct {
	Pass   bool   `json:"pass"`
	Reason string `json:"reason"`
}

// RankerGate passes a ranker only when its NDCG@k gain over the hand-set
// formula is significant on at least MinQueries held-out queries.
func RankerGate(rep RankingReport, systemName string) GateResult {
	for _, s := range rep.Systems {
		if s.Name != systemName || s.VsHand == nil {
			continue
		}
		c := s.VsHand
		switch {
		case s.Queries < MinQueries:
			return GateResult{false, fmt.Sprintf("only %d held-out queries; need %d", s.Queries, MinQueries)}
		case c.Significant && c.Diff > 0:
			return GateResult{true, fmt.Sprintf("NDCG@%d +%.3f over the hand-set formula (95%% CI %+.3f to %+.3f)", rep.K, c.Diff, c.Lo, c.Hi)}
		default:
			return GateResult{false, fmt.Sprintf("no significant gain over the hand-set formula: NDCG@%d %+.3f (95%% CI %+.3f to %+.3f)", rep.K, c.Diff, c.Lo, c.Hi)}
		}
	}
	return GateResult{false, "the model was not evaluated"}
}

// CalibratorGate passes a calibrator when it lowers ECE and Brier on held-out judgments.
func CalibratorGate(c *CalibrationResult) GateResult {
	switch {
	case c == nil || c.Calibrated == nil:
		return GateResult{false, "the calibrator was not evaluated"}
	case c.Raw.N < 50:
		return GateResult{false, fmt.Sprintf("only %d held-out judgments; need 50", c.Raw.N)}
	case c.Calibrated.ECE < c.Raw.ECE && c.Calibrated.Brier <= c.Raw.Brier:
		return GateResult{true, fmt.Sprintf("ECE %.3f → %.3f, Brier %.3f → %.3f", c.Raw.ECE, c.Calibrated.ECE, c.Raw.Brier, c.Calibrated.Brier)}
	}
	return GateResult{false, fmt.Sprintf("no improvement: ECE %.3f → %.3f, Brier %.3f → %.3f", c.Raw.ECE, c.Calibrated.ECE, c.Raw.Brier, c.Calibrated.Brier)}
}
