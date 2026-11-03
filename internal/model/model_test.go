package model

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"testing"
)

func TestXGBoostMatchesPython(t *testing.T) {
	var exp struct {
		X          [][]*float64 `json:"x"`
		Ranker     []float64    `json:"ranker"`
		Classifier []float64    `json:"classifier"`
	}
	b, err := os.ReadFile("testdata/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &exp); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		file string
		want []float64
	}{{"testdata/ranker.json", exp.Ranker}, {"testdata/classifier.json", exp.Classifier}} {
		raw, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		m, err := Load(AlgoXGBoost, raw)
		if err != nil {
			t.Fatal(err)
		}
		for i, row := range exp.X {
			x := make([]float64, len(row))
			for j, v := range row {
				if v == nil {
					x[j] = math.NaN() // missing values follow each split's default branch
				} else {
					x[j] = *v
				}
			}
			if got := m.(Scorer).Score(x); math.Abs(got-c.want[i]) > 1e-5 {
				t.Fatalf("%s row %d: Go %.7f, XGBoost %.7f", c.file, i, got, c.want[i])
			}
		}
	}
}

func TestLogRegLearns(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var x [][]float64
	var y []float64
	for i := 0; i < 400; i++ {
		a, b := rng.NormFloat64(), rng.NormFloat64()
		x = append(x, []float64{a, b, 5}) // the constant column must stay inert
		label := 0.0
		if 2*a-b+0.3*rng.NormFloat64() > 0 {
			label = 1
		}
		y = append(y, label)
	}
	m, err := TrainLogReg([]string{"a", "b", "c"}, x, y, TrainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Weights[0] <= 0 || m.Weights[1] >= 0 || math.Abs(m.Weights[2]) > 1e-9 {
		t.Fatalf("weights %v", m.Weights)
	}
	correct := 0
	for i := range x {
		if (m.Score(x[i]) > 0.5) == (y[i] == 1) {
			correct++
		}
	}
	if correct < 360 {
		t.Fatalf("training accuracy %d/400", correct)
	}
	if _, err := TrainLogReg(nil, x[:3], []float64{1, 1, 1}, TrainOptions{}); err == nil {
		t.Fatal("one-class labels should be refused")
	}
}

func TestPairwiseRanks(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	var groups []Group
	for q := 0; q < 60; q++ {
		var g Group
		for i := 0; i < 8; i++ {
			rel, noise := rng.Float64(), rng.Float64()
			g.X = append(g.X, []float64{noise, rel})
			grade := 0.0
			if rel > 0.7 {
				grade = 1
			}
			g.Grade = append(g.Grade, grade)
		}
		groups = append(groups, g)
	}
	m, err := TrainPairwise([]string{"noise", "rel"}, groups, TrainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Weights[1] <= 5*math.Abs(m.Weights[0]) {
		t.Fatalf("the ranker should rely on rel, not noise: %v", m.Weights)
	}
	if m.Score([]float64{0.5, 0.9}) <= m.Score([]float64{0.5, 0.2}) {
		t.Fatal("higher relevance should score higher")
	}
	if m.Explain(1) != "rel "+m.Explain(1)[4:] {
		t.Fatalf("explain = %q", m.Explain(1))
	}
}

func TestIsotonic(t *testing.T) {
	p := []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9}
	y := []float64{0, 1, 0, 0, 1, 0, 1, 1, 1}
	m, err := FitIsotonic(p, y)
	if err != nil {
		t.Fatal(err)
	}
	prev := -1.0
	for v := 0.0; v <= 1.0; v += 0.05 {
		c := m.Calibrate(v)
		if c < prev-1e-12 || c < 0 || c > 1 {
			t.Fatalf("not monotone in [0,1] at %.2f: %.3f after %.3f", v, c, prev)
		}
		prev = c
	}
	if m.Calibrate(0.95) != 1 || m.Calibrate(0.05) != 0 {
		t.Fatalf("ends: %.2f %.2f", m.Calibrate(0.05), m.Calibrate(0.95))
	}
	b, _ := json.Marshal(m)
	if back, err := Load(AlgoIsotonic, b); err != nil || back.(Calibrator).Calibrate(0.5) != m.Calibrate(0.5) {
		t.Fatalf("round trip: %v", err)
	}
}

func TestFeaturesVectorMatchesNames(t *testing.T) {
	f := Features{Keyword: 1, HandConf: 2}
	f.SetKind("fact")
	f.SetEdge("about")
	v := f.Vector()
	if len(v) != len(Names) || v[0] != 1 || v[len(v)-1] != 2 || v[11] != 1 || v[17] != 1 {
		t.Fatalf("vector %v does not follow Names", v)
	}
}
