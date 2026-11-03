package store

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/text"
)

// Attend returns what matters now for a context, rather than what merely matches
// (milestone M8; design, "Attend"). Salience combines relevance from recall,
// overlap with the caller's active goals, recent change, unresolved conflicts and
// a kind preference set by the retrieval mode. It returns a handful of items
// (working-memory capacity) and never trains pathways: being shown something is
// not evidence that it was useful.

// Retrieval modes preset attend's parameters by function.
var attendModes = map[string]struct {
	hops    int
	k       int
	kind    map[string]float64
	novelty float64
	rel     float64
}{
	"":           {hops: 2, k: 4, rel: 0.45, kind: map[string]float64{"decision": 1, "fact": 0.8, "episode": 0.6, "suggestion": 0.3}},
	"detail":     {hops: 1, k: 4, rel: 0.45, kind: map[string]float64{"episode": 1, "decision": 0.9, "fact": 0.4, "suggestion": 0.3, "trait": 0.1}},
	"explore":    {hops: 3, k: 6, rel: 0.35, novelty: 0.2, kind: map[string]float64{"decision": 0.6, "fact": 0.6, "episode": 0.6, "suggestion": 0.6, "concept": 0.6}},
	"focus":      {hops: 3, k: 3, rel: 0.6, kind: map[string]float64{"decision": 1, "fact": 0.8, "episode": 0.6, "suggestion": 0.2}},
	"generalize": {hops: 2, k: 4, rel: 0.4, kind: map[string]float64{"trait": 1, "fact": 1, "procedure": 0.9, "decision": 0.7, "episode": 0.2}},
}

// AttendInput is the input of Attend.
type AttendInput struct {
	Context    string
	Goals      []string
	K          int
	Mode       string // detail, explore, focus, generalize or "" for balanced
	Namespaces []string
}

// Conflict is an unresolved disagreement between two memories.
type Conflict struct {
	A       string `json:"a"`
	B       string `json:"b"`
	Summary string `json:"summary"`
}

// AttendResult is the output of Attend.
type AttendResult struct {
	Memories  []Scored       `json:"memories"`
	Conflicts []Conflict     `json:"conflicts"`
	Pending   []Notification `json:"pending,omitempty"` // unread push notifications for this client
}

// Attend ranks what matters now for a client's context and goals.
func (s *Store) Attend(client string, in AttendInput) (AttendResult, error) {
	mode, ok := attendModes[in.Mode]
	if !ok {
		return AttendResult{}, ecp.Errorf(ecp.InvalidParams, "mode must be detail, explore, focus or generalize")
	}
	if strings.TrimSpace(in.Context) == "" {
		return AttendResult{}, ecp.Errorf(ecp.InvalidParams, "context is required")
	}
	k := in.K
	if k <= 0 {
		k = mode.k
	}
	k = min(k, 12)
	query := in.Context + " " + strings.Join(in.Goals, " ")
	hits, err := s.RecallQuiet(client, RecallInput{Query: query, K: max(k*4, 16), Hops: mode.hops, Namespaces: in.Namespaces})
	if err != nil {
		return AttendResult{}, err
	}
	res := AttendResult{Memories: []Scored{}, Conflicts: []Conflict{}}
	conflicts, inConflict, err := s.conflictsAmong(client, hits)
	if err != nil {
		return AttendResult{}, err
	}
	res.Conflicts = conflicts

	maxScore := 0.0
	for _, h := range hits {
		maxScore = math.Max(maxScore, h.Score)
	}
	goals := make([]map[string]bool, 0, len(in.Goals))
	for _, g := range in.Goals {
		set := map[string]bool{}
		for _, t := range text.Tokenize(g) {
			set[t] = true
		}
		if len(set) > 0 {
			goals = append(goals, set)
		}
	}
	now := Now()
	type cand struct {
		s   Scored
		sal float64
	}
	var cands []cand
	for _, h := range hits {
		rel := 0.0
		if maxScore > 0 {
			rel = h.Score / maxScore
		}
		var why []string
		goal := 0.0
		toks := text.Tokenize(h.Memory.Content + " " + strings.Join(h.Memory.Tags, " "))
		for gi, g := range goals {
			hit := 0
			for _, t := range toks {
				if g[t] {
					hit++
				}
			}
			if v := math.Min(1, float64(hit)/float64(len(g))); v > goal {
				goal = v
				if v >= 0.5 {
					why = append(why, fmt.Sprintf("serves goal %q", in.Goals[gi]))
				}
			}
		}
		recency := 0.0
		if t, err := time.Parse(time.RFC3339, h.Memory.CreatedAt); err == nil {
			age := now.Sub(t).Hours()
			recency = math.Exp(-math.Max(0, age) / 24)
			if age < 24 {
				why = append(why, "changed in the last day")
			}
		}
		kind := mode.kind[h.Memory.Kind]
		if in.Mode == "generalize" && h.Memory.Provenance.Service == "sleep" {
			kind = 1
		}
		if in.Mode == "detail" && h.Memory.Provenance.Service == "sleep" {
			kind *= 0.3
		}
		conflict := 0.0
		if inConflict[h.Memory.ID] {
			conflict = 1
			why = append(why, "in an unresolved conflict")
		}
		novelty := 0.0
		if mode.novelty > 0 && h.Memory.LastActivatedAt == "" {
			novelty = 1
			why = append(why, "not looked at before")
		}
		sal := mode.rel*rel + 0.2*goal + 0.15*recency + 0.1*kind + 0.1*conflict + mode.novelty*novelty
		reason := h.Reason
		if len(why) > 0 {
			reason += "; " + strings.Join(why, ", ")
		}
		h.Reason = reason
		h.Score = round4(sal)
		cands = append(cands, cand{h, sal})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].sal > cands[j].sal })
	for _, c := range cands {
		if len(res.Memories) == k {
			break
		}
		res.Memories = append(res.Memories, c.s)
	}
	if pending, err := s.PendingNotifications(client, false); err == nil {
		res.Pending = pending
	}
	_ = audit(s.db, client, "attend", "", Short(in.Context))
	return res, nil
}

// conflictsAmong returns conflicts edges touching the hits whose both ends the client may read.
func (s *Store) conflictsAmong(client string, hits []Scored) ([]Conflict, map[string]bool, error) {
	out := []Conflict{}
	in := map[string]bool{}
	if len(hits) == 0 {
		return out, in, nil
	}
	readable, all, err := s.readable(client)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]any, len(hits))
	for i, h := range hits {
		ids[i] = h.Memory.ID
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := s.db.Query(`SELECT e.src, e.dst, a.content, b.content, a.namespace, b.namespace
		FROM edges e JOIN memories a ON a.id = e.src JOIN memories b ON b.id = e.dst
		WHERE e.kind = 'conflicts' AND (e.src IN (`+ph+`) OR e.dst IN (`+ph+`))
		  AND a.valid_to IS NULL AND b.valid_to IS NULL`, append(ids, ids...)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var a, b, ca, cb, na, nb string
		if err := rows.Scan(&a, &b, &ca, &cb, &na, &nb); err != nil {
			return nil, nil, err
		}
		if !all && (!contains(readable, na) || !contains(readable, nb)) {
			continue
		}
		key := min(a, b) + "|" + max(a, b)
		if seen[key] {
			continue
		}
		seen[key] = true
		in[a], in[b] = true, true
		out = append(out, Conflict{A: a, B: b, Summary: fmt.Sprintf("%q disagrees with %q", Short(ca), Short(cb))})
	}
	return out, in, rows.Err()
}

// MarkConflict records that two current memories disagree. Conflicts are kept, not
// merged, and surface through attend until one supersedes the other.
func (s *Store) MarkConflict(client, a, b string) error {
	if a == b {
		return ecp.Errorf(ecp.InvalidParams, "a memory cannot conflict with itself")
	}
	mems, err := s.Get(client, []string{a, b})
	if err != nil {
		return err
	}
	if len(mems) != 2 {
		return ecp.Errorf(ecp.MemoryNotFound, "both memories must exist")
	}
	if ok, err := s.can(client, mems[0].Namespace, true); err != nil {
		return err
	} else if !ok {
		return ecp.Errorf(ecp.ScopeDenied, "client %q cannot write to namespace %q", client, mems[0].Namespace)
	}
	if err := s.AddEdge(a, b, "conflicts", 1); err != nil {
		return err
	}
	if err := s.AddEvent("conflict", map[string]string{"a": a, "b": b}); err != nil {
		return err
	}
	return audit(s.db, client, "conflict", mems[0].Namespace, a+" "+b)
}
