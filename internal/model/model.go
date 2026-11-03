// Package model holds Engram's learned models: the features recall logs for each
// result, and the small models trained on them (a pairwise linear ranker,
// logistic regression, isotonic calibration, and scoring of XGBoost models
// trained by the optional Python worker). Models are plain data, stored in the
// database and loaded by kind; none of them is used until an evaluation on
// held-out data shows it beats what it would replace.
package model

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Features describes one recalled memory at the moment it was ranked. Recall
// logs them, evaluation re-ranks with them, and rankers score them.
type Features struct {
	Keyword     float64 `json:"kw"`    // BM25 match, squashed to 0..1
	Semantic    float64 `json:"sem"`   // embedding similarity above the threshold, 0..1
	Relevance   float64 `json:"rel"`   // the blend of the two
	Prior       float64 `json:"prior"` // how readily it comes to mind: strength and use
	Strength    float64 `json:"str"`
	BaseLevel   float64 `json:"base"` // ACT-R base-level activation, 0..1
	Hops        float64 `json:"hops"` // 0 for a direct match
	PathWeight  float64 `json:"pw"`   // product of edge weights along the path (1 when direct)
	Activations float64 `json:"acts"` // log(1 + times recalled)
	AgeDays     float64 `json:"age"`  // log(1 + days since created)
	Decision    float64 `json:"dec"`  // kind flags
	Fact        float64 `json:"fact"`
	Episode     float64 `json:"epi"`
	Concept     float64 `json:"con"`
	Overridden  float64 `json:"ovr"`
	ViaCoactive float64 `json:"vco"` // the last hop's edge kind
	ViaSimilar  float64 `json:"vsi"`
	ViaAbout    float64 `json:"vab"`
	Agreement   float64 `json:"agr"`  // keyword and meaning both matched
	HandScore   float64 `json:"hand"` // the hand-set formula's score
	HandConf    float64 `json:"conf"` // and its confidence
}

// Names lists the features in vector order.
var Names = []string{"kw", "sem", "rel", "prior", "str", "base", "hops", "pw", "acts", "age",
	"dec", "fact", "epi", "con", "ovr", "vco", "vsi", "vab", "agr", "hand", "conf"}

// Vector returns the features in Names order.
func (f Features) Vector() []float64 {
	return []float64{f.Keyword, f.Semantic, f.Relevance, f.Prior, f.Strength, f.BaseLevel, f.Hops, f.PathWeight,
		f.Activations, f.AgeDays, f.Decision, f.Fact, f.Episode, f.Concept, f.Overridden,
		f.ViaCoactive, f.ViaSimilar, f.ViaAbout, f.Agreement, f.HandScore, f.HandConf}
}

// SetKind sets the kind flags.
func (f *Features) SetKind(kind string) {
	f.Decision, f.Fact, f.Episode, f.Concept = 0, 0, 0, 0
	switch kind {
	case "decision":
		f.Decision = 1
	case "fact":
		f.Fact = 1
	case "episode":
		f.Episode = 1
	case "concept":
		f.Concept = 1
	}
}

// SetEdge sets the flags for the kind of the last edge on the path.
func (f *Features) SetEdge(kind string) {
	f.ViaCoactive, f.ViaSimilar, f.ViaAbout = 0, 0, 0
	switch kind {
	case "coactive":
		f.ViaCoactive = 1
	case "similar":
		f.ViaSimilar = 1
	case "about":
		f.ViaAbout = 1
	}
}

// Scorer turns a feature vector into a score; higher ranks first.
type Scorer interface {
	Score(x []float64) float64
}

// Calibrator maps a raw confidence to a probability that the result helps.
type Calibrator interface {
	Calibrate(p float64) float64
}

// Kinds of model, by what they replace.
const (
	KindRanker     = "ranker"     // replaces the hand-set recall score
	KindCalibrator = "calibrator" // turns recall confidence into a calibrated probability
	KindPermit     = "permit"     // predicts permission answers
)

// Algorithms.
const (
	AlgoPairwise = "pairwise-logreg" // a linear ranker trained on pairs (RankNet with a linear model)
	AlgoLogReg   = "logreg"          // logistic regression
	AlgoIsotonic = "isotonic"        // pool-adjacent-violators calibration
	AlgoXGBoost  = "xgboost"         // a model saved by XGBoost as JSON
)

// Load decodes stored parameters into a usable model.
func Load(algo string, params []byte) (any, error) {
	switch algo {
	case AlgoPairwise, AlgoLogReg:
		var m Linear
		if err := json.Unmarshal(params, &m); err != nil {
			return nil, err
		}
		if len(m.Weights) == 0 {
			return nil, fmt.Errorf("linear model has no weights")
		}
		return &m, nil
	case AlgoIsotonic:
		var m Isotonic
		if err := json.Unmarshal(params, &m); err != nil {
			return nil, err
		}
		if len(m.X) == 0 || len(m.X) != len(m.Y) {
			return nil, fmt.Errorf("isotonic model is empty or malformed")
		}
		return &m, nil
	case AlgoXGBoost:
		return ParseXGBoost(params)
	}
	return nil, fmt.Errorf("unknown model algorithm %q", algo)
}

func sigmoid(z float64) float64 {
	if z >= 0 {
		return 1 / (1 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1 + e)
}

// Linear is a standardized linear model: z = b + Σ w_i (x_i − μ_i)/σ_i. As a
// ranker it scores z; as a classifier it returns σ(z).
type Linear struct {
	Features []string  `json:"features"`
	Mean     []float64 `json:"mean"`
	Std      []float64 `json:"std"`
	Weights  []float64 `json:"weights"`
	Bias     float64   `json:"bias"`
	Logistic bool      `json:"logistic"` // Score returns a probability
}

func (m *Linear) z(x []float64) float64 {
	z := m.Bias
	for i, w := range m.Weights {
		if i >= len(x) {
			break
		}
		v := x[i]
		if math.IsNaN(v) {
			continue // a missing value contributes its mean
		}
		if i < len(m.Mean) {
			v = (v - m.Mean[i]) / m.Std[i]
		}
		z += w * v
	}
	return z
}

// Score implements Scorer.
func (m *Linear) Score(x []float64) float64 {
	if m.Logistic {
		return sigmoid(m.z(x))
	}
	return m.z(x)
}

// Explain lists the largest standardized weights, for reports.
func (m *Linear) Explain(n int) string {
	type fw struct {
		name string
		w    float64
	}
	var ws []fw
	for i, w := range m.Weights {
		name := fmt.Sprintf("x%d", i)
		if i < len(m.Features) {
			name = m.Features[i]
		}
		ws = append(ws, fw{name, w})
	}
	for i := range ws { // selection sort by |w|; the lists are tiny
		for j := i + 1; j < len(ws); j++ {
			if math.Abs(ws[j].w) > math.Abs(ws[i].w) {
				ws[i], ws[j] = ws[j], ws[i]
			}
		}
	}
	var parts []string
	for i := 0; i < len(ws) && i < n; i++ {
		parts = append(parts, fmt.Sprintf("%s %+.2f", ws[i].name, ws[i].w))
	}
	return strings.Join(parts, ", ")
}

func standardize(rows [][]float64) (mean, std []float64) {
	if len(rows) == 0 {
		return nil, nil
	}
	d := len(rows[0])
	mean, std = make([]float64, d), make([]float64, d)
	for j := 0; j < d; j++ {
		n := 0.0
		for _, r := range rows {
			if !math.IsNaN(r[j]) {
				mean[j] += r[j]
				n++
			}
		}
		if n > 0 {
			mean[j] /= n
		}
		for _, r := range rows {
			if !math.IsNaN(r[j]) {
				std[j] += (r[j] - mean[j]) * (r[j] - mean[j])
			}
		}
		if n > 1 {
			std[j] = math.Sqrt(std[j] / (n - 1))
		}
		if std[j] < 1e-9 {
			std[j] = 1 // a constant feature carries no signal; keep it inert
		}
	}
	return mean, std
}

// TrainOptions controls the gradient-descent trainers.
type TrainOptions struct {
	Epochs       int     // default 300
	LearningRate float64 // default 0.1
	L2           float64 // default 0.01
}

func (o *TrainOptions) defaults() {
	if o.Epochs <= 0 {
		o.Epochs = 300
	}
	if o.LearningRate <= 0 {
		o.LearningRate = 0.1
	}
	if o.L2 < 0 {
		o.L2 = 0
	} else if o.L2 == 0 {
		o.L2 = 0.01
	}
}

// TrainLogReg fits logistic regression by full-batch gradient descent on
// standardized features with L2 regularization. y is 0 or 1.
func TrainLogReg(names []string, x [][]float64, y []float64, o TrainOptions) (*Linear, error) {
	if len(x) == 0 || len(x) != len(y) {
		return nil, fmt.Errorf("need matching, non-empty examples and labels")
	}
	o.defaults()
	mean, std := standardize(x)
	m := &Linear{Features: names, Mean: mean, Std: std, Weights: make([]float64, len(mean)), Logistic: true}
	pos := 0.0
	for _, v := range y {
		pos += v
	}
	if pos == 0 || pos == float64(len(y)) {
		return nil, fmt.Errorf("labels are all one class; nothing to learn")
	}
	m.Bias = math.Log(pos / (float64(len(y)) - pos))
	n := float64(len(x))
	grad := make([]float64, len(mean))
	for e := 0; e < o.Epochs; e++ {
		for j := range grad {
			grad[j] = 0
		}
		gb := 0.0
		for i, r := range x {
			err := sigmoid(m.z(r)) - y[i]
			gb += err
			for j := range grad {
				v := r[j]
				if math.IsNaN(v) {
					continue
				}
				grad[j] += err * (v - mean[j]) / std[j]
			}
		}
		m.Bias -= o.LearningRate * gb / n
		for j := range m.Weights {
			m.Weights[j] -= o.LearningRate * (grad[j]/n + o.L2*m.Weights[j])
		}
	}
	return m, nil
}

// Group is one query's candidates with graded relevance (0 = not relevant).
type Group struct {
	X     [][]float64
	Grade []float64
}

// TrainPairwise fits a linear ranker on every pair within a group whose grades
// differ: P(i above j) = σ(w·(x_i − x_j)), the linear form of RankNet.
func TrainPairwise(names []string, groups []Group, o TrainOptions) (*Linear, error) {
	o.defaults()
	var all [][]float64
	for _, g := range groups {
		all = append(all, g.X...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no examples")
	}
	mean, std := standardize(all)
	type pair struct{ hi, lo []float64 }
	var pairs []pair
	std0 := func(r []float64) []float64 {
		out := make([]float64, len(r))
		for j, v := range r {
			if math.IsNaN(v) {
				continue // the mean, which is 0 after standardizing
			}
			out[j] = (v - mean[j]) / std[j]
		}
		return out
	}
	for _, g := range groups {
		for i := range g.X {
			for j := range g.X {
				if g.Grade[i] > g.Grade[j] {
					pairs = append(pairs, pair{std0(g.X[i]), std0(g.X[j])})
				}
			}
		}
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("no pairs with different grades; nothing to learn")
	}
	w := make([]float64, len(mean))
	grad := make([]float64, len(mean))
	n := float64(len(pairs))
	for e := 0; e < o.Epochs; e++ {
		for j := range grad {
			grad[j] = 0
		}
		for _, p := range pairs {
			z := 0.0
			for j := range w {
				z += w[j] * (p.hi[j] - p.lo[j])
			}
			g := sigmoid(z) - 1 // d/dz of −log σ(z)
			for j := range w {
				grad[j] += g * (p.hi[j] - p.lo[j])
			}
		}
		for j := range w {
			w[j] -= o.LearningRate * (grad[j]/n + o.L2*w[j])
		}
	}
	return &Linear{Features: names, Mean: mean, Std: std, Weights: w}, nil
}

// Isotonic is a monotone step function fitted by pool-adjacent-violators:
// confidence X[i] maps to probability Y[i], linearly interpolated between steps.
type Isotonic struct {
	X []float64 `json:"x"`
	Y []float64 `json:"y"`
}

// FitIsotonic fits a non-decreasing map from p to the observed outcome rate.
func FitIsotonic(p, y []float64) (*Isotonic, error) {
	if len(p) == 0 || len(p) != len(y) {
		return nil, fmt.Errorf("need matching, non-empty scores and labels")
	}
	idx := make([]int, len(p))
	for i := range idx {
		idx[i] = i
	}
	for i := 1; i < len(idx); i++ { // insertion sort by p keeps ties stable
		for j := i; j > 0 && p[idx[j]] < p[idx[j-1]]; j-- {
			idx[j], idx[j-1] = idx[j-1], idx[j]
		}
	}
	type block struct{ sumY, n, lo, hi float64 }
	var blocks []block
	for _, i := range idx {
		blocks = append(blocks, block{y[i], 1, p[i], p[i]})
		for len(blocks) > 1 {
			a, b := blocks[len(blocks)-2], blocks[len(blocks)-1]
			if a.sumY/a.n <= b.sumY/b.n {
				break
			}
			blocks = append(blocks[:len(blocks)-2], block{a.sumY + b.sumY, a.n + b.n, a.lo, b.hi})
		}
	}
	m := &Isotonic{}
	for _, b := range blocks {
		v := b.sumY / b.n
		m.X = append(m.X, b.lo)
		m.Y = append(m.Y, v)
		if b.hi > b.lo {
			m.X = append(m.X, b.hi)
			m.Y = append(m.Y, v)
		}
	}
	return m, nil
}

// Calibrate implements Calibrator.
func (m *Isotonic) Calibrate(p float64) float64 {
	n := len(m.X)
	switch {
	case n == 0:
		return p
	case p <= m.X[0]:
		return m.Y[0]
	case p >= m.X[n-1]:
		return m.Y[n-1]
	}
	for i := 1; i < n; i++ {
		if p <= m.X[i] {
			if m.X[i] == m.X[i-1] {
				return m.Y[i]
			}
			t := (p - m.X[i-1]) / (m.X[i] - m.X[i-1])
			return m.Y[i-1] + t*(m.Y[i]-m.Y[i-1])
		}
	}
	return m.Y[n-1]
}
