package eval

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/model"
	"github.com/zakeripouya/EngramContextProtocol/internal/permit"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// PermitDataset is the permission log turned into examples: features from
// earlier requests only, and whether the user approved without a failure.
type PermitDataset struct {
	Requests []store.PermitRequest
	X        [][]float64
	Y        []float64
}

// BuildPermitDataset reads resolved requests oldest first and computes features.
func BuildPermitDataset(st *store.Store) (PermitDataset, error) {
	reqs, err := permit.Chronological(st)
	if err != nil {
		return PermitDataset{}, err
	}
	d := PermitDataset{Requests: reqs}
	h := permit.NewHistory()
	for _, r := range reqs {
		d.X = append(d.X, h.Vector(r))
		d.Y = append(d.Y, permit.Label(r))
		h.Add(r) // only after its own features: no peeking at the answer
	}
	return d, nil
}

// Split divides the dataset by time; test is the newest share.
func (d PermitDataset) Split(testFraction float64) (train, test PermitDataset, at time.Time) {
	if testFraction <= 0 || testFraction >= 1 {
		testFraction = 0.3
	}
	cut := int(math.Floor(float64(len(d.Requests)) * (1 - testFraction)))
	cut = max(0, min(cut, len(d.Requests)))
	train = PermitDataset{Requests: d.Requests[:cut], X: d.X[:cut], Y: d.Y[:cut]}
	test = PermitDataset{Requests: d.Requests[cut:], X: d.X[cut:], Y: d.Y[cut:]}
	if cut < len(d.Requests) {
		at, _ = time.Parse(time.RFC3339, d.Requests[cut].At)
	}
	return train, test, at
}

// DecisionMetrics measures "allow" decisions against what the user did.
type DecisionMetrics struct {
	Name      string   `json:"name"`
	Allowed   int      `json:"allowed"` // requests the system would auto-approve
	Precision Estimate `json:"precision"`
	Recall    Estimate `json:"recall"` // share of the user's approvals it would have auto-approved
	F1        float64  `json:"f1"`
	ROCAUC    float64  `json:"rocAuc"`
	PRAUC     float64  `json:"prAuc"`
	Threshold float64  `json:"threshold,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// PermitReport is the result of EvaluatePermits.
type PermitReport struct {
	Requests  int               `json:"requests"`
	Train     int               `json:"train"`
	Test      int               `json:"test"`
	Split     time.Time         `json:"split"`
	Positives int               `json:"testApprovals"`
	Systems   []DecisionMetrics `json:"systems"`
	Weights   string            `json:"weights,omitempty"`
	Warnings  []string          `json:"warnings,omitempty"`
}

// bootstrapRatio estimates num/den over resampled examples.
func bootstrapRatio(n int, num, den func(i int) float64, seed int64) Estimate {
	ratio := func(idx []int) float64 {
		a, b := 0.0, 0.0
		for _, i := range idx {
			a += num(i)
			b += den(i)
		}
		if b == 0 {
			return math.NaN()
		}
		return a / b
	}
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	e := Estimate{Mean: ratio(all), N: n}
	if n < 2 {
		e.Lo, e.Hi = e.Mean, e.Mean
		return e
	}
	rng := rand.New(rand.NewSource(seed))
	var vals []float64
	idx := make([]int, n)
	for r := 0; r < Resamples; r++ {
		for i := range idx {
			idx[i] = rng.Intn(n)
		}
		if v := ratio(idx); !math.IsNaN(v) {
			vals = append(vals, v)
		}
	}
	sort.Float64s(vals)
	e.Lo, e.Hi = quantile(vals, 0.025), quantile(vals, 0.975)
	return e
}

func decisionMetrics(name string, allow []bool, score, y []float64, seed int64) DecisionMetrics {
	m := DecisionMetrics{Name: name, ROCAUC: ROCAUC(score, y), PRAUC: PRAUC(score, y)}
	for _, a := range allow {
		if a {
			m.Allowed++
		}
	}
	tp := func(i int) float64 {
		if allow[i] && y[i] > 0 {
			return 1
		}
		return 0
	}
	m.Precision = bootstrapRatio(len(y), tp, func(i int) float64 {
		if allow[i] {
			return 1
		}
		return 0
	}, seed)
	m.Recall = bootstrapRatio(len(y), tp, func(i int) float64 { return y[i] }, seed+1)
	if p, r := m.Precision.Mean, m.Recall.Mean; p+r > 0 && !math.IsNaN(p) && !math.IsNaN(r) {
		m.F1 = 2 * p * r / (p + r)
	}
	return m
}

// PermitModel is a trained permission model and its decision threshold.
type PermitModel struct {
	Model     *model.Linear
	Threshold float64
}

// TrainPermitModel fits logistic regression on the training requests.
func TrainPermitModel(train PermitDataset) (*model.Linear, error) {
	return model.TrainLogReg(permit.FeatureNames, train.X, train.Y, model.TrainOptions{Epochs: 500})
}

// EvaluatePermits compares the count rule that made the shadow predictions with
// a logistic-regression model trained on older requests, on the newest ones.
// The model allows only eligible-tier requests with at least MinPrecedents,
// like the rule, at threshold MinConfidence.
func EvaluatePermits(st *store.Store, testFraction float64, candidate *model.Linear, seed int64) (PermitReport, *model.Linear, error) {
	if seed == 0 {
		seed = 1
	}
	d, err := BuildPermitDataset(st)
	if err != nil {
		return PermitReport{}, nil, err
	}
	train, test, at := d.Split(testFraction)
	rep := PermitReport{Requests: len(d.Requests), Train: len(train.Requests), Test: len(test.Requests), Split: at}
	for _, v := range test.Y {
		rep.Positives += int(v)
	}
	if len(test.Requests) == 0 {
		rep.Warnings = append(rep.Warnings, "no resolved permission requests yet; turn on the hooks with engram init --permission-hooks")
		return rep, nil, nil
	}
	if len(test.Requests) < 50 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("only %d held-out requests; precision intervals are wide", len(test.Requests)))
	}

	// The rule, as it predicted at the time.
	allow := make([]bool, len(test.Requests))
	score := make([]float64, len(test.Requests))
	for i, r := range test.Requests {
		allow[i] = r.Predicted == "allow"
		score[i] = r.Confidence
	}
	rule := decisionMetrics("rule", allow, score, test.Y, seed)
	rule.Note = fmt.Sprintf("eligible tier, ≥%d precedents, Beta posterior ≥ %.2f (logged at the time)", permit.MinPrecedents, permit.MinConfidence)
	rep.Systems = append(rep.Systems, rule)

	m := candidate
	if m == nil {
		m, err = TrainPermitModel(train)
		if err != nil {
			rep.Warnings = append(rep.Warnings, "no model: "+err.Error())
			return rep, nil, nil
		}
	}
	mAllow := make([]bool, len(test.Requests))
	mScore := make([]float64, len(test.Requests))
	for i, r := range test.Requests {
		mScore[i] = m.Score(test.X[i])
		n := r.Precedents
		mAllow[i] = r.Tier == permit.Eligible && n >= permit.MinPrecedents && mScore[i] >= permit.MinConfidence
	}
	mm := decisionMetrics("logreg", mAllow, mScore, test.Y, seed+7)
	mm.Threshold = permit.MinConfidence
	mm.Note = "logistic regression on earlier requests, same tier and precedent rails"
	rep.Systems = append(rep.Systems, mm)
	rep.Weights = m.Explain(6)
	return rep, m, nil
}

// PermitGate passes a permission model when its precision is at least the
// rule's and at least 0.98 (lower bound ≥ 0.95), and it auto-approves more.
func PermitGate(rep PermitReport) GateResult {
	if len(rep.Systems) < 2 {
		return GateResult{false, "the model was not evaluated"}
	}
	rule, m := rep.Systems[0], rep.Systems[1]
	switch {
	case rep.Test < 50:
		return GateResult{false, fmt.Sprintf("only %d held-out requests; need 50", rep.Test)}
	case math.IsNaN(m.Precision.Mean) || m.Allowed == 0:
		return GateResult{false, "the model would never auto-approve on the held-out requests"}
	case m.Precision.Mean < 0.98 || m.Precision.Lo < 0.95:
		return GateResult{false, fmt.Sprintf("precision %.3f (95%% CI %.3f–%.3f) is under 0.98", m.Precision.Mean, m.Precision.Lo, m.Precision.Hi)}
	case !math.IsNaN(rule.Precision.Mean) && m.Precision.Mean < rule.Precision.Mean:
		return GateResult{false, fmt.Sprintf("precision %.3f is under the rule's %.3f", m.Precision.Mean, rule.Precision.Mean)}
	case m.Allowed <= rule.Allowed:
		return GateResult{false, fmt.Sprintf("auto-approves %d, no more than the rule's %d", m.Allowed, rule.Allowed)}
	}
	return GateResult{true, fmt.Sprintf("precision %.3f (CI %.3f–%.3f), %d auto-approvals vs the rule's %d", m.Precision.Mean, m.Precision.Lo, m.Precision.Hi, m.Allowed, rule.Allowed)}
}
