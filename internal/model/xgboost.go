package model

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// XGBoost scores a gradient-boosted tree model saved by XGBoost's save_model
// as JSON (XGBoost 1.0 and later), so models trained by the Python worker run in
// Go with no Python at recall time. Numeric splits only.
type XGBoost struct {
	FeatureNames []string
	base         float64
	logistic     bool
	trees        []xgbTree
}

type xgbTree struct {
	split       []int
	cond        []float64
	left, right []int
	defLeft     []bool
}

// ParseXGBoost reads a model from XGBoost's JSON format.
func ParseXGBoost(b []byte) (*XGBoost, error) {
	var raw struct {
		Learner struct {
			FeatureNames []string `json:"feature_names"`
			Param        struct {
				BaseScore string `json:"base_score"`
				NumClass  string `json:"num_class"`
			} `json:"learner_model_param"`
			Objective struct {
				Name string `json:"name"`
			} `json:"objective"`
			Booster struct {
				Name  string `json:"name"`
				Model struct {
					Trees []struct {
						SplitIndices    []int     `json:"split_indices"`
						SplitConditions []float64 `json:"split_conditions"`
						LeftChildren    []int     `json:"left_children"`
						RightChildren   []int     `json:"right_children"`
						DefaultLeft     []int     `json:"default_left"`
						SplitType       []int     `json:"split_type"`
					} `json:"trees"`
				} `json:"model"`
			} `json:"gradient_booster"`
		} `json:"learner"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("not an XGBoost JSON model: %w", err)
	}
	l := raw.Learner
	if l.Booster.Name != "gbtree" {
		return nil, fmt.Errorf("only gbtree boosters are supported, not %q", l.Booster.Name)
	}
	if n := l.Param.NumClass; n != "" && n != "0" && n != "1" {
		return nil, fmt.Errorf("multi-class models are not supported")
	}
	if len(l.Booster.Model.Trees) == 0 {
		return nil, fmt.Errorf("the model has no trees")
	}
	base, err := strconv.ParseFloat(strings.Trim(l.Param.BaseScore, "[] "), 64)
	if err != nil {
		return nil, fmt.Errorf("base_score %q: %w", l.Param.BaseScore, err)
	}
	m := &XGBoost{FeatureNames: l.FeatureNames}
	switch obj := l.Objective.Name; {
	case obj == "binary:logistic" || obj == "reg:logistic":
		m.logistic = true
		base = math.Log(base / (1 - base)) // base_score is a probability; trees add log-odds
	case strings.HasPrefix(obj, "rank:") || obj == "reg:squarederror" || obj == "binary:logitraw" || obj == "reg:linear":
	default:
		return nil, fmt.Errorf("objective %q is not supported", obj)
	}
	m.base = base
	for i, t := range l.Booster.Model.Trees {
		n := len(t.LeftChildren)
		if n == 0 || len(t.RightChildren) != n || len(t.SplitIndices) != n || len(t.SplitConditions) != n {
			return nil, fmt.Errorf("tree %d is malformed", i)
		}
		for _, st := range t.SplitType {
			if st != 0 {
				return nil, fmt.Errorf("tree %d uses categorical splits, which are not supported", i)
			}
		}
		tr := xgbTree{split: t.SplitIndices, cond: t.SplitConditions, left: t.LeftChildren, right: t.RightChildren,
			defLeft: make([]bool, n)}
		for j := range tr.defLeft {
			tr.defLeft[j] = j < len(t.DefaultLeft) && t.DefaultLeft[j] == 1
		}
		m.trees = append(m.trees, tr)
	}
	return m, nil
}

// Score implements Scorer: the margin for ranking and regression objectives,
// a probability for logistic ones. NaN means missing.
func (m *XGBoost) Score(x []float64) float64 {
	z := m.base
	for _, t := range m.trees {
		node := 0
		for steps := 0; t.left[node] != -1 && steps < len(t.left); steps++ {
			f := t.split[node]
			v := math.NaN()
			if f < len(x) {
				v = x[f]
			}
			switch {
			case math.IsNaN(v):
				if t.defLeft[node] {
					node = t.left[node]
				} else {
					node = t.right[node]
				}
			case float32(v) < float32(t.cond[node]): // XGBoost compares in float32
				node = t.left[node]
			default:
				node = t.right[node]
			}
		}
		z += t.cond[node] // a leaf's value is stored in split_conditions
	}
	if m.logistic {
		return sigmoid(z)
	}
	return z
}

// Trees reports the number of trees.
func (m *XGBoost) Trees() int { return len(m.trees) }
