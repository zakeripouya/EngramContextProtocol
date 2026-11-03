// Package health measures whether Engram's learning dynamics stay in healthy
// bounds and whether they drift (milestone M6; docs/design.md, "Testing and
// drift detection"). A system can pass every software test and still drift into
// bad dynamics: runaway pathways, one hub absorbing everything, memories all
// pinned to the floor. These checks catch that.
//
// Only bounds Engram can measure today are reported as measured. The others in
// the design (sparse-code density, power-law forgetting fits, confidence
// calibration, trait stability) are listed as "not yet measurable" rather than faked.
package health

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Status of one bound.
const (
	OK       = "ok"
	Warn     = "warn"
	Breach   = "breach"
	Unknown  = "not yet measurable"
	MaxHub   = 120 // fan-out cap from the design
	binCount = 10
)

// Bound is one health check.
type Bound struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Limit  string  `json:"limit"`
	Status string  `json:"status"`
	Detail string  `json:"detail,omitempty"`
}

// Metrics is one night's measurement, stored for drift detection.
type Metrics struct {
	At             string    `json:"at"`
	Memories       int       `json:"memories"`
	CoactiveEdges  int       `json:"coactiveEdges"`
	WeightHist     []float64 `json:"weightHist"` // share of coactive edges per 0.1-wide bin
	StrengthHist   []float64 `json:"strengthHist"`
	Saturated      float64   `json:"saturated"` // share of coactive edges ≥ 0.95
	AtFloor        float64   `json:"atFloor"`   // share of memories at strength ≤ 0.06
	MaxDegree      int       `json:"maxDegree"`
	DecisionRecall float64   `json:"decisionRecall"` // -1 when there are no decisions
}

// Report combines bounds and drift.
type Report struct {
	Metrics Metrics  `json:"metrics"`
	Bounds  []Bound  `json:"bounds"`
	Drift   []Bound  `json:"drift"`
	Unknown []string `json:"notYetMeasurable"`
	Worst   string   `json:"worst"`
}

func hist(vals []float64) []float64 {
	h := make([]float64, binCount)
	if len(vals) == 0 {
		return h
	}
	for _, v := range vals {
		i := int(v * binCount)
		i = max(0, min(i, binCount-1))
		h[i]++
	}
	for i := range h {
		h[i] /= float64(len(vals))
	}
	return h
}

func share(vals []float64, pred func(float64) bool) float64 {
	if len(vals) == 0 {
		return 0
	}
	n := 0
	for _, v := range vals {
		if pred(v) {
			n++
		}
	}
	return float64(n) / float64(len(vals))
}

// PSI is the population stability index between two histograms; above 0.25 is a large shift.
func PSI(expected, actual []float64) float64 {
	const eps = 1e-4
	var psi float64
	for i := range expected {
		e, a := math.Max(expected[i], eps), math.Max(actual[i], eps)
		psi += (a - e) * math.Log(a/e)
	}
	return psi
}

// Measure computes tonight's metrics. decisionRecall comes from the sleep cycle's
// self-test (found / tested), or -1 when no decisions exist.
func Measure(st *store.Store, decisionRecall float64) (Metrics, error) {
	weights, err := st.Weights("coactive")
	if err != nil {
		return Metrics{}, err
	}
	strengths, err := st.Strengths()
	if err != nil {
		return Metrics{}, err
	}
	deg, _, err := st.DegreeMax()
	if err != nil {
		return Metrics{}, err
	}
	return Metrics{
		At:             store.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Memories:       len(strengths),
		CoactiveEdges:  len(weights),
		WeightHist:     hist(weights),
		StrengthHist:   hist(strengths),
		Saturated:      share(weights, func(w float64) bool { return w >= 0.95 }),
		AtFloor:        share(strengths, func(s float64) bool { return s <= 0.06 }),
		MaxDegree:      deg,
		DecisionRecall: decisionRecall,
	}, nil
}

// Evaluate checks metrics against the bounds and the stored history for drift.
func Evaluate(m Metrics, history []Metrics) Report {
	r := Report{Metrics: m, Unknown: []string{
		"sparse-code density (sparse codes are not built yet)",
		"forgetting-curve fit (needs months of activation history)",
		"confidence calibration (needs feedback with confidence logged; see M9 shadow data)",
		"trait stability (traits are not distilled yet)",
	}}
	add := func(list *[]Bound, b Bound) { *list = append(*list, b) }

	sat := Bound{Name: "Weight saturation", Value: m.Saturated, Limit: "under 10% of pathways at ≥ 0.95", Status: OK}
	if m.Saturated >= 0.25 {
		sat.Status, sat.Detail = Breach, "runaway Hebbian growth: many pathways pinned near 1"
	} else if m.Saturated >= 0.10 {
		sat.Status, sat.Detail = Warn, "pathways are piling up near the maximum"
	}
	add(&r.Bounds, sat)

	floor := Bound{Name: "Memories at the strength floor", Value: m.AtFloor, Limit: "under 50%", Status: OK}
	if m.AtFloor >= 0.8 {
		floor.Status, floor.Detail = Breach, "decay or downscaling is too aggressive"
	} else if m.AtFloor >= 0.5 {
		floor.Status, floor.Detail = Warn, "most memories have faded to the floor"
	}
	add(&r.Bounds, floor)

	hub := Bound{Name: "Largest hub degree", Value: float64(m.MaxDegree), Limit: fmt.Sprintf("at most %d links", MaxHub), Status: OK}
	if m.MaxDegree > MaxHub {
		hub.Status, hub.Detail = Breach, "one memory is absorbing too many pathways"
	} else if m.MaxDegree > MaxHub*3/4 {
		hub.Status, hub.Detail = Warn, "approaching the fan-out cap"
	}
	add(&r.Bounds, hub)

	dec := Bound{Name: "Decision recall", Value: m.DecisionRecall, Limit: "at least 80% found in the top 3", Status: OK}
	switch {
	case m.DecisionRecall < 0:
		dec.Status, dec.Detail = Unknown, "no user decisions recorded yet"
	case m.DecisionRecall < 0.6:
		dec.Status, dec.Detail = Breach, "recall can no longer find the user's decisions"
	case m.DecisionRecall < 0.8:
		dec.Status, dec.Detail = Warn, "some decisions are getting hard to find"
	}
	add(&r.Bounds, dec)

	// Drift: compare tonight with the mean of up to 7 previous nights.
	if len(history) >= 3 && m.CoactiveEdges >= 20 {
		base := make([]float64, binCount)
		n := min(len(history), 7)
		for _, h := range history[:n] {
			for i := range base {
				if i < len(h.WeightHist) {
					base[i] += h.WeightHist[i] / float64(n)
				}
			}
		}
		psi := PSI(base, m.WeightHist)
		d := Bound{Name: "Pathway weight drift (PSI vs last week)", Value: psi, Limit: "under 0.10; over 0.25 is severe", Status: OK}
		if psi > 0.25 {
			d.Status, d.Detail = Breach, "the weight distribution shifted sharply; consider restoring last night's snapshot"
		} else if psi > 0.10 {
			d.Status, d.Detail = Warn, "the weight distribution is shifting"
		}
		add(&r.Drift, d)
	} else {
		r.Unknown = append(r.Unknown, "drift (needs 3 nights of history and 20 learned pathways)")
	}

	r.Worst = OK
	for _, b := range append(append([]Bound{}, r.Bounds...), r.Drift...) {
		if b.Status == Breach {
			r.Worst = Breach
		} else if b.Status == Warn && r.Worst == OK {
			r.Worst = Warn
		}
	}
	sort.Strings(r.Unknown)
	return r
}

// Check measures, evaluates against stored history and records tonight's metrics.
func Check(st *store.Store, decisionRecall float64, save bool) (Report, error) {
	m, err := Measure(st, decisionRecall)
	if err != nil {
		return Report{}, err
	}
	raw, err := st.HealthHistory(14)
	if err != nil {
		return Report{}, err
	}
	var history []Metrics
	for _, b := range raw {
		var h Metrics
		if json.Unmarshal(b, &h) == nil {
			history = append(history, h)
		}
	}
	rep := Evaluate(m, history)
	if save {
		if err := st.SaveHealth(m); err != nil {
			return rep, err
		}
	}
	return rep, nil
}
