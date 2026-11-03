package store

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/text"
)

// Milestone M4 (docs/design.md, "Wiring and learning", "Retrieval",
// "Neuroscience grounding" v1 mechanisms):
//
//   - ACT-R base-level activation: memories used often and recently are easier to recall.
//   - Three-factor learning: recall gives co-activated pairs half a Hebbian step;
//     feedback that confirms them (accepted, or better, succeeded) gives the other half,
//     and feedback that contradicts them weakens them.
//   - Outcomes over approval: a failed objective outcome always counts as negative,
//     so the memory learns what works rather than what gets approved.
//   - Lateral inhibition: near-duplicate results suppress each other.
//   - Confidence: each result carries a confidence; weak results are dropped
//     rather than padding the list.

// baseLevel returns ACT-R base-level activation squashed to 0..1, using the standard
// approximation B = ln(n / (1 − d)) − d·ln(L) with decay d = 0.5, where n counts the
// creation plus every recall and L is the memory's lifetime in days.
func baseLevel(activations int, created string, now time.Time) float64 {
	const d = 0.5
	t, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return 0.5
	}
	life := now.Sub(t).Hours() / 24
	if life < 1.0/24 {
		life = 1.0 / 24
	}
	n := float64(activations + 1)
	b := math.Log(n/(1-d)) - d*math.Log(life)
	return 1 / (1 + math.Exp(-b))
}

// nearDuplicate reports whether two results say essentially the same thing:
// near-identical vectors, or nearly the same words.
func nearDuplicate(a, b Memory, va, vb []float32) bool {
	if va != nil && vb != nil && len(va) == len(vb) {
		var dot float64
		for i := range va {
			dot += float64(va[i]) * float64(vb[i])
		}
		if dot >= 0.95 {
			return true
		}
	}
	return jaccard(text.Tokenize(a.Content), text.Tokenize(b.Content)) >= 0.85
}

func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	sa := map[string]bool{}
	for _, x := range a {
		sa[x] = true
	}
	sb := map[string]bool{}
	inter := 0
	for _, x := range b {
		if !sb[x] {
			sb[x] = true
			if sa[x] {
				inter++
			}
		}
	}
	return float64(inter) / float64(len(sa)+len(sb)-inter)
}

// FeedbackInput reports how recalled memories turned out.
type FeedbackInput struct {
	MemoryIDs  []string
	Outcome    string    // "accepted" or "overridden"
	Objective  string    // "", "succeeded" or "failed": what actually happened
	Correction *Decision // when overridden: what the user chose instead
	Query      string    // optional: what was being asked, kept as training data
}

// FeedbackResult reports what feedback changed.
type FeedbackResult struct {
	Updated      int    `json:"updated"`
	Direction    string `json:"direction"` // "strengthened" or "weakened"
	CorrectionID string `json:"correctionId,omitempty"`
}

// Feedback applies the third factor of learning to memories an agent used.
func (s *Store) Feedback(client string, in FeedbackInput) (FeedbackResult, error) {
	if in.Outcome != "accepted" && in.Outcome != "overridden" {
		return FeedbackResult{}, ecp.Errorf(ecp.InvalidParams, "outcome must be accepted or overridden")
	}
	if in.Objective != "" && in.Objective != "succeeded" && in.Objective != "failed" {
		return FeedbackResult{}, ecp.Errorf(ecp.InvalidParams, "objective must be succeeded or failed")
	}
	if len(in.MemoryIDs) == 0 {
		return FeedbackResult{}, ecp.Errorf(ecp.InvalidParams, "memoryIds is empty")
	}
	if in.Correction != nil && in.Outcome != "overridden" {
		return FeedbackResult{}, ecp.Errorf(ecp.InvalidParams, "a correction only makes sense with outcome overridden")
	}
	mems, err := s.Get(client, in.MemoryIDs) // checks existence and read access
	if err != nil {
		return FeedbackResult{}, err
	}

	// The third factor: a real outcome outweighs approval.
	factor := 0.0
	switch {
	case in.Objective == "failed" || in.Outcome == "overridden":
		factor = -1
	case in.Objective == "succeeded":
		factor = 1
	default: // accepted, outcome unknown
		factor = 0.5
	}

	now := Now()
	tx, err := s.db.Begin()
	if err != nil {
		return FeedbackResult{}, err
	}
	defer tx.Rollback()
	for _, m := range mems {
		st := m.Strength
		if factor > 0 {
			st += 0.1 * factor * (1 - st)
		} else {
			st = math.Max(0.05, st*0.7)
		}
		if _, err := tx.Exec(`UPDATE memories SET strength = ? WHERE id = ?`, st, m.ID); err != nil {
			return FeedbackResult{}, err
		}
		if _, err := tx.Exec(`INSERT INTO feedback(at, client, memory_id, outcome, objective, query) VALUES (?,?,?,?,?,?)`,
			ts(now), client, m.ID, in.Outcome, nullable(in.Objective), nullable(in.Query)); err != nil {
			return FeedbackResult{}, err
		}
	}
	// Pathways among the memories used together get the rest of their Hebbian step, or lose it.
	for i := 0; i < len(mems); i++ {
		for j := i + 1; j < len(mems); j++ {
			a, b := mems[i].ID, mems[j].ID
			if a > b {
				a, b = b, a
			}
			var w float64
			err := tx.QueryRow(`SELECT weight FROM edges WHERE src = ? AND dst = ? AND kind = 'coactive'`, a, b).Scan(&w)
			if err == sql.ErrNoRows {
				if factor <= 0 {
					continue
				}
			} else if err != nil {
				return FeedbackResult{}, err
			}
			if factor > 0 {
				w += 0.5 * s.learn.Eta * factor * (1 - w)
			} else {
				w *= 0.5
			}
			if _, err := tx.Exec(`INSERT INTO edges(src, dst, kind, weight, updated_at) VALUES (?,?,'coactive',?,?)
				ON CONFLICT(src, dst, kind) DO UPDATE SET weight = excluded.weight, updated_at = excluded.updated_at`,
				a, b, w, ts(now)); err != nil {
				return FeedbackResult{}, err
			}
		}
	}
	if err := audit(tx, client, "feedback", "", fmt.Sprintf(`{"n":%d,"outcome":%q,"objective":%q}`, len(mems), in.Outcome, in.Objective)); err != nil {
		return FeedbackResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeedbackResult{}, err
	}

	res := FeedbackResult{Updated: len(mems), Direction: "strengthened"}
	if factor < 0 {
		res.Direction = "weakened"
	}
	// An override with a correction records what the user chose, linked to what it overrode.
	if in.Correction != nil {
		content := strings.TrimSpace(in.Correction.Choice)
		if in.Correction.Situation != "" {
			content = in.Correction.Situation + ": " + content
		}
		obs, err := s.Observe(client, ObserveInput{
			Kind: "decision", Author: "user", Content: content, Decision: in.Correction,
			Namespace: mems[0].Namespace, Scope: mems[0].Scope,
		})
		if err != nil {
			return res, err
		}
		res.CorrectionID = obs.ID
		for _, m := range mems {
			if err := s.AddEdge(obs.ID, m.ID, "overrode", 1); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}
