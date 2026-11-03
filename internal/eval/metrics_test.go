package eval

import (
	"math"
	"testing"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f", name, got, want)
	}
}

func TestRankingMetrics(t *testing.T) {
	r := Ranked{IDs: []string{"a", "b", "c", "d", "e"}, Grades: map[string]float64{"b": 2, "d": 1, "z": 1}, Relevant: 3}
	near(t, "recall@5", RecallAt(r, 5), 2.0/3, 1e-12) // z was never retrieved
	near(t, "recall@2", RecallAt(r, 2), 1.0/3, 1e-12)
	near(t, "precision@5", PrecisionAt(r, 5), 0.4, 1e-12)
	near(t, "mrr", MRR(r), 0.5, 1e-12)
	// DCG = 3/log2(3) + 1/log2(5); ideal = 3/log2(2) + 1/log2(3) + 1/log2(4)
	dcg := 3/math.Log2(3) + 1/math.Log2(5)
	idcg := 3.0 + 1/math.Log2(3) + 0.5
	near(t, "ndcg@5", NDCGAt(r, 5), dcg/idcg, 1e-12)
	if MRR(Ranked{IDs: []string{"x"}, Grades: map[string]float64{"x": 0}}) != 0 {
		t.Error("a judged-irrelevant result is not a hit")
	}
}

func TestClassificationMetricsMatchScikitLearn(t *testing.T) {
	s := []float64{0.9, 0.8, 0.7, 0.6, 0.55, 0.4, 0.3, 0.2}
	y := []float64{1, 0, 1, 1, 0, 0, 1, 0}
	near(t, "roc auc", ROCAUC(s, y), 0.6875, 1e-12)           // sklearn.metrics.roc_auc_score
	near(t, "pr auc", PRAUC(s, y), 0.7470238095238095, 1e-12) // sklearn.metrics.average_precision_score
	c := Calibrate([]float64{0.05, 0.15, 0.95, 0.95}, []float64{0, 0, 1, 0}, 10)
	// bins: 0.0–0.1 conf 0.05 acc 0; 0.1–0.2 conf 0.15 acc 0; 0.9–1.0 conf 0.95 acc 0.5
	near(t, "ece", c.ECE, 0.25*0.05+0.25*0.15+0.5*0.45, 1e-12)
	near(t, "brier", c.Brier, (0.0025+0.0225+0.0025+0.9025)/4, 1e-12)
}

func TestKS(t *testing.T) {
	a := []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0}
	b := []float64{0.35, 0.45, 0.55, 0.65, 0.75, 0.85, 0.95, 1.05, 1.15, 1.25, 1.35, 1.45}
	d, p := KS(a, b)
	near(t, "D", d, 0.41666666666666674, 1e-12) // scipy.stats.ks_2samp
	near(t, "p", p, 0.267, 0.06)                // asymptotic approximations differ slightly
	if _, p := KS(a, a); p != 1 {
		t.Errorf("identical samples p = %v", p)
	}
}

func TestBootstrap(t *testing.T) {
	var v []float64
	for i := 0; i < 200; i++ {
		v = append(v, float64(i%2))
	}
	e := Bootstrap(v, 1)
	if e.Mean != 0.5 || e.Lo >= 0.5 || e.Hi <= 0.5 || e.Hi-e.Lo > 0.2 {
		t.Fatalf("estimate %+v", e)
	}
	better := make([]float64, len(v))
	for i := range v {
		better[i] = v[i] + 0.1
	}
	if c := Paired(better, v, 1); !c.Significant || c.Diff < 0.099 {
		t.Fatalf("a consistent gain should be significant: %+v", c)
	}
	noisy := make([]float64, len(v))
	for i := range v {
		noisy[i] = v[(i+1)%len(v)]
	}
	if c := Paired(noisy, v, 1); c.Significant {
		t.Fatalf("a shuffle is not a gain: %+v", c)
	}
	if c := Unpaired(better, v, 1); !c.Significant {
		t.Fatalf("unpaired: %+v", c)
	}
}
