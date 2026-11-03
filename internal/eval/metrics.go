// Package eval measures Engram the way retrieval and ML systems are measured:
// held-out, time-split data; ranking metrics (Recall@k, MRR, NDCG@k);
// calibration (ECE, Brier, reliability bins); precision and recall for the
// permission assist; baselines and ablations; and bootstrap confidence
// intervals, so a difference is only called a win when it is more than noise.
package eval

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Ranked is one query's ranking: result ids in order, the graded relevance of
// judged ids (0 = not relevant), and how many relevant ids exist in all.
type Ranked struct {
	IDs      []string
	Grades   map[string]float64
	Relevant int // relevant ids, including ones the ranking missed
}

// RecallAt is the share of relevant ids found in the top k.
func RecallAt(r Ranked, k int) float64 {
	if r.Relevant == 0 {
		return 0
	}
	found := 0
	for i, id := range r.IDs {
		if i >= k {
			break
		}
		if r.Grades[id] > 0 {
			found++
		}
	}
	return float64(found) / float64(r.Relevant)
}

// PrecisionAt is the share of the top k that is relevant.
func PrecisionAt(r Ranked, k int) float64 {
	if k <= 0 {
		return 0
	}
	found := 0
	for i, id := range r.IDs {
		if i >= k {
			break
		}
		if r.Grades[id] > 0 {
			found++
		}
	}
	return float64(found) / float64(k)
}

// MRR is the reciprocal rank of the first relevant id (0 when none is found).
func MRR(r Ranked) float64 {
	for i, id := range r.IDs {
		if r.Grades[id] > 0 {
			return 1 / float64(i+1)
		}
	}
	return 0
}

// NDCGAt is normalized discounted cumulative gain with gains 2^grade − 1.
func NDCGAt(r Ranked, k int) float64 {
	dcg := 0.0
	for i, id := range r.IDs {
		if i >= k {
			break
		}
		if g := r.Grades[id]; g > 0 {
			dcg += (math.Pow(2, g) - 1) / math.Log2(float64(i+2))
		}
	}
	var ideal []float64
	for _, g := range r.Grades {
		if g > 0 {
			ideal = append(ideal, g)
		}
	}
	// Relevant ids that were never judged with a grade count as grade 1.
	for len(ideal) < r.Relevant {
		ideal = append(ideal, 1)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(ideal)))
	idcg := 0.0
	for i, g := range ideal {
		if i >= k {
			break
		}
		idcg += (math.Pow(2, g) - 1) / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// Bin is one bucket of a reliability diagram.
type Bin struct {
	Lo, Hi     float64
	Count      int
	Confidence float64 // mean predicted probability
	Accuracy   float64 // observed rate of positives
}

// Calibration summarizes how well probabilities match outcomes.
type Calibration struct {
	N     int     `json:"n"`
	ECE   float64 `json:"ece"`   // expected calibration error: Σ (n_b/N)|acc_b − conf_b|
	Brier float64 `json:"brier"` // mean squared error of the probability
	Bins  []Bin   `json:"bins"`
}

// Calibrate computes ECE over equal-width bins, the Brier score and the bins.
func Calibrate(p, y []float64, bins int) Calibration {
	if bins <= 0 {
		bins = 10
	}
	c := Calibration{N: len(p), Bins: make([]Bin, bins)}
	for b := range c.Bins {
		c.Bins[b].Lo, c.Bins[b].Hi = float64(b)/float64(bins), float64(b+1)/float64(bins)
	}
	if len(p) == 0 {
		return c
	}
	for i := range p {
		v := math.Max(0, math.Min(1, p[i]))
		b := min(bins-1, int(v*float64(bins)))
		c.Bins[b].Count++
		c.Bins[b].Confidence += v
		c.Bins[b].Accuracy += y[i]
		c.Brier += (v - y[i]) * (v - y[i])
	}
	c.Brier /= float64(len(p))
	for b := range c.Bins {
		if n := c.Bins[b].Count; n > 0 {
			c.Bins[b].Confidence /= float64(n)
			c.Bins[b].Accuracy /= float64(n)
			c.ECE += float64(n) / float64(len(p)) * math.Abs(c.Bins[b].Accuracy-c.Bins[b].Confidence)
		}
	}
	return c
}

// ROCAUC is the probability that a random positive scores above a random negative.
func ROCAUC(score, y []float64) float64 {
	type pt struct{ s, y float64 }
	pts := make([]pt, len(score))
	for i := range score {
		pts[i] = pt{score[i], y[i]}
	}
	sort.Slice(pts, func(a, b int) bool { return pts[a].s < pts[b].s })
	var pos, neg, rankSum float64
	for i := 0; i < len(pts); {
		j := i
		for j < len(pts) && pts[j].s == pts[i].s {
			j++
		}
		avg := float64(i+j+1) / 2 // average 1-based rank of the tie group
		for k := i; k < j; k++ {
			if pts[k].y > 0 {
				rankSum += avg
				pos++
			} else {
				neg++
			}
		}
		i = j
	}
	if pos == 0 || neg == 0 {
		return math.NaN()
	}
	return (rankSum - pos*(pos+1)/2) / (pos * neg)
}

// PRAUC is the area under the precision-recall curve (average precision).
func PRAUC(score, y []float64) float64 {
	idx := make([]int, len(score))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return score[idx[a]] > score[idx[b]] })
	pos := 0.0
	for _, v := range y {
		if v > 0 {
			pos++
		}
	}
	if pos == 0 {
		return math.NaN()
	}
	tp, ap := 0.0, 0.0
	for i, k := range idx {
		if y[k] > 0 {
			tp++
			ap += tp / float64(i+1)
		}
	}
	return ap / pos
}

// Estimate is a mean with a 95% bootstrap interval.
type Estimate struct {
	Mean float64 `json:"mean"`
	Lo   float64 `json:"lo"`
	Hi   float64 `json:"hi"`
	N    int     `json:"n"`
}

// Comparison is a paired difference (variant − baseline) with its interval and
// the share of resamples in which the variant did not win.
type Comparison struct {
	Diff    float64 `json:"diff"`
	Lo      float64 `json:"lo"`
	Hi      float64 `json:"hi"`
	PNoGain float64 `json:"pNoGain"`
	// Significant is true when the whole 95% interval is above (or below) zero.
	Significant bool `json:"significant"`
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := min(lo+1, len(sorted)-1)
	return sorted[lo] + (pos-float64(lo))*(sorted[hi]-sorted[lo])
}

// Resamples is the number of bootstrap resamples.
var Resamples = 2000

// Bootstrap estimates the mean of per-query values with a percentile interval.
// The seed makes reports reproducible.
func Bootstrap(v []float64, seed int64) Estimate {
	e := Estimate{Mean: mean(v), N: len(v)}
	if len(v) < 2 {
		e.Lo, e.Hi = e.Mean, e.Mean
		return e
	}
	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, Resamples)
	for r := range means {
		s := 0.0
		for range v {
			s += v[rng.Intn(len(v))]
		}
		means[r] = s / float64(len(v))
	}
	sort.Float64s(means)
	e.Lo, e.Hi = quantile(means, 0.025), quantile(means, 0.975)
	return e
}

// Paired compares two systems on the same queries by resampling queries.
func Paired(variant, baseline []float64, seed int64) Comparison {
	n := min(len(variant), len(baseline))
	d := make([]float64, n)
	for i := 0; i < n; i++ {
		d[i] = variant[i] - baseline[i]
	}
	c := Comparison{Diff: mean(d)}
	if n < 2 {
		c.Lo, c.Hi, c.PNoGain = c.Diff, c.Diff, 1
		return c
	}
	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, Resamples)
	noGain := 0
	for r := range means {
		s := 0.0
		for range d {
			s += d[rng.Intn(n)]
		}
		means[r] = s / float64(n)
		if means[r] <= 0 {
			noGain++
		}
	}
	sort.Float64s(means)
	c.Lo, c.Hi = quantile(means, 0.025), quantile(means, 0.975)
	c.PNoGain = float64(noGain) / float64(Resamples)
	c.Significant = c.Lo > 0 || c.Hi < 0
	return c
}

// KS is the two-sample Kolmogorov–Smirnov test: the largest gap between the
// two empirical distributions, and its asymptotic p-value.
func KS(a, b []float64) (d, p float64) {
	if len(a) == 0 || len(b) == 0 {
		return 0, 1
	}
	x := append([]float64{}, a...)
	y := append([]float64{}, b...)
	sort.Float64s(x)
	sort.Float64s(y)
	i, j := 0, 0
	for i < len(x) && j < len(y) {
		v := math.Min(x[i], y[j])
		for i < len(x) && x[i] <= v {
			i++
		}
		for j < len(y) && y[j] <= v {
			j++
		}
		d = math.Max(d, math.Abs(float64(i)/float64(len(x))-float64(j)/float64(len(y))))
	}
	return d, KSProb(d, len(x), len(y))
}

// KSProb is the asymptotic p-value of a two-sample KS statistic d with sample sizes n1 and n2.
func KSProb(d float64, n1, n2 int) float64 {
	if n1 <= 0 || n2 <= 0 {
		return 1
	}
	ne := float64(n1) * float64(n2) / float64(n1+n2)
	lambda := (math.Sqrt(ne) + 0.12 + 0.11/math.Sqrt(ne)) * d
	if lambda < 0.3 {
		return 1 // the series converges slowly here, and its value is 1 to many decimals
	}
	sum := 0.0
	for k := 1; k <= 100; k++ {
		term := 2 * math.Pow(-1, float64(k-1)) * math.Exp(-2*float64(k*k)*lambda*lambda)
		sum += term
		if math.Abs(term) < 1e-12 {
			return math.Max(0, math.Min(1, sum))
		}
	}
	return 1
}

// Unpaired compares two independent samples (an experiment's arms) by
// resampling each, returning the difference of means (a − b) and its interval.
func Unpaired(a, b []float64, seed int64) Comparison {
	c := Comparison{Diff: mean(a) - mean(b)}
	if len(a) < 2 || len(b) < 2 {
		c.Lo, c.Hi, c.PNoGain = c.Diff, c.Diff, 1
		return c
	}
	rng := rand.New(rand.NewSource(seed))
	diffs := make([]float64, Resamples)
	noGain := 0
	for r := range diffs {
		sa, sb := 0.0, 0.0
		for range a {
			sa += a[rng.Intn(len(a))]
		}
		for range b {
			sb += b[rng.Intn(len(b))]
		}
		diffs[r] = sa/float64(len(a)) - sb/float64(len(b))
		if diffs[r] <= 0 {
			noGain++
		}
	}
	sort.Float64s(diffs)
	c.Lo, c.Hi = quantile(diffs, 0.025), quantile(diffs, 0.975)
	c.PNoGain = float64(noGain) / float64(Resamples)
	c.Significant = c.Lo > 0 || c.Hi < 0
	return c
}

// ArmResult is one arm of an online experiment.
type ArmResult struct {
	Arm        string   `json:"arm"`
	Recalls    int      `json:"recalls"`
	Judged     int      `json:"judged"`     // recalls that got feedback
	AcceptRate Estimate `json:"acceptRate"` // share of recalls with at least one accepted result
	MRR        Estimate `json:"mrr"`        // over judged recalls
}

// ExperimentReport compares an experiment's arms on the recalls they served.
type ExperimentReport struct {
	Name       string      `json:"name"`
	Arms       []ArmResult `json:"arms"`
	AcceptDiff *Comparison `json:"acceptRateDiff,omitempty"` // variant − control
	MRRDiff    *Comparison `json:"mrrDiff,omitempty"`
	Warnings   []string    `json:"warnings,omitempty"`
}

// AnalyzeExperiment reads the recalls an experiment served and compares its arms.
func AnalyzeExperiment(st *store.Store, name string, seed int64) (ExperimentReport, error) {
	if seed == 0 {
		seed = 1
	}
	rep := ExperimentReport{Name: name}
	all, err := st.RecallLog(time.Time{}, time.Time{}, false)
	if err != nil {
		return rep, err
	}
	type arm struct {
		accept, mrr []float64
		recalls     int
	}
	arms := map[string]*arm{"control": {}, "variant": {}}
	prefix := "exp:" + name + "/"
	for _, q := range all {
		if !strings.HasPrefix(q.Variant, prefix) {
			continue
		}
		which := strings.TrimPrefix(q.Variant, prefix)
		if i := strings.Index(which, "+"); i >= 0 {
			which = which[:i]
		}
		a := arms[which]
		if a == nil {
			continue
		}
		a.recalls++
		accepted := 0.0
		if len(q.Grades) > 0 {
			r := Ranked{Grades: q.Grades, Relevant: relevantCount(q)}
			for _, x := range q.Results {
				r.IDs = append(r.IDs, x.ID)
			}
			if r.Relevant > 0 {
				accepted = 1
			}
			a.mrr = append(a.mrr, MRR(r))
		}
		a.accept = append(a.accept, accepted)
	}
	for i, n := range []string{"control", "variant"} {
		a := arms[n]
		rep.Arms = append(rep.Arms, ArmResult{Arm: n, Recalls: a.recalls, Judged: len(a.mrr),
			AcceptRate: Bootstrap(a.accept, seed+int64(i)), MRR: Bootstrap(a.mrr, seed+10+int64(i))})
	}
	c, v := arms["control"], arms["variant"]
	if len(c.accept) > 0 && len(v.accept) > 0 {
		d := Unpaired(v.accept, c.accept, seed+20)
		rep.AcceptDiff = &d
	}
	if len(c.mrr) > 0 && len(v.mrr) > 0 {
		d := Unpaired(v.mrr, c.mrr, seed+21)
		rep.MRRDiff = &d
	}
	if len(v.mrr) < MinQueries || len(c.mrr) < MinQueries {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("each arm needs %d judged recalls before a difference means much", MinQueries))
	}
	return rep, nil
}
