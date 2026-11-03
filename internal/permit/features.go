package permit

import (
	"math"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/model"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Features for the learned permission model. Each request's features use only
// requests resolved before it, so a model trained on them never sees the future.

// FeatureNames lists the permission features in vector order.
var FeatureNames = []string{"eligible", "unknown", "approved", "denied", "failed", "log_n", "beta",
	"same_input_ok", "same_input_no", "bash", "edit", "write", "read", "web", "mcp", "hour_sin", "hour_cos", "recent_deny"}

// History accumulates resolved requests in time order.
type History struct {
	byCat   map[string][3]int // client|category -> approved, denied, failed
	byInput map[string][2]int // client|tool|hash -> approved, denied
	recent  map[string][]bool // client -> last 20 outcomes (true = denied)
}

// NewHistory starts an empty history.
func NewHistory() *History {
	return &History{byCat: map[string][3]int{}, byInput: map[string][2]int{}, recent: map[string][]bool{}}
}

// Add records a resolved request.
func (h *History) Add(r store.PermitRequest) {
	if r.Outcome != "approved" && r.Outcome != "denied" {
		return
	}
	c := h.byCat[r.Client+"|"+r.Category]
	in := h.byInput[r.Client+"|"+r.Tool+"|"+r.InputHash]
	if r.Outcome == "approved" {
		c[0]++
		in[0]++
		if r.Objective == "failed" {
			c[2]++
		}
	} else {
		c[1]++
		in[1]++
	}
	h.byCat[r.Client+"|"+r.Category] = c
	h.byInput[r.Client+"|"+r.Tool+"|"+r.InputHash] = in
	rec := append(h.recent[r.Client], r.Outcome == "denied")
	if len(rec) > 20 {
		rec = rec[len(rec)-20:]
	}
	h.recent[r.Client] = rec
}

// Vector returns a request's features given everything resolved before it.
func (h *History) Vector(r store.PermitRequest) []float64 {
	c := h.byCat[r.Client+"|"+r.Category]
	in := h.byInput[r.Client+"|"+r.Tool+"|"+r.InputHash]
	n := float64(c[0] + c[1])
	beta := (float64(c[0]-c[2]) + 1) / (n + 2)
	tool := strings.ToLower(r.Tool)
	flag := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	hour := 0.0
	if t, err := time.Parse(time.RFC3339, r.At); err == nil {
		hour = float64(t.Local().Hour()) + float64(t.Local().Minute())/60
	}
	recent := h.recent[r.Client]
	deny := 0.0
	for _, d := range recent {
		if d {
			deny++
		}
	}
	if len(recent) > 0 {
		deny /= float64(len(recent))
	}
	return []float64{flag(r.Tier == string(Eligible)), flag(r.Tier == string(Unknown)), float64(c[0]), float64(c[1]), float64(c[2]),
		math.Log1p(n), beta, float64(in[0]), float64(in[1]),
		flag(tool == "bash"), flag(tool == "edit" || tool == "multiedit" || tool == "notebookedit"), flag(tool == "write"),
		flag(tool == "read" || tool == "glob" || tool == "grep" || tool == "ls"), flag(tool == "webfetch" || tool == "websearch"),
		flag(strings.HasPrefix(tool, "mcp__")),
		math.Sin(2 * math.Pi * hour / 24), math.Cos(2 * math.Pi * hour / 24), deny}
}

// Label is 1 when the user approved and the run did not fail.
func Label(r store.PermitRequest) float64 {
	if r.Outcome == "approved" && r.Objective != "failed" {
		return 1
	}
	return 0
}

// Chronological returns resolved requests oldest first.
func Chronological(st *store.Store) ([]store.PermitRequest, error) {
	all, err := st.Permits(1 << 30)
	if err != nil {
		return nil, err
	}
	var out []store.PermitRequest
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Outcome == "approved" || all[i].Outcome == "denied" {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// activeModel returns the active permission model, if any.
func activeModel(st *store.Store) (model.Scorer, string) {
	ms, err := st.Models(model.KindPermit)
	if err != nil {
		return nil, ""
	}
	for _, m := range ms {
		if m.Status != "active" {
			continue
		}
		loaded, err := model.Load(m.Algo, m.Params)
		if err != nil {
			return nil, ""
		}
		if sc, ok := loaded.(model.Scorer); ok {
			return sc, m.ID
		}
	}
	return nil, ""
}
