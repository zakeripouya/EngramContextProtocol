// Package sleep runs Engram's nightly consolidation cycle (milestone M5;
// docs/design.md, "Consolidation", "Dreaming", "Sleep hours").
//
// Order of work, cheapest and safest first:
//
//  1. Snapshot the database so the whole night can be rolled back.
//  2. Decay learned pathways and downscale unused memories (homeostasis).
//  3. Merge near-duplicates and archive memories that faded out (classical, no LLM).
//  4. Find hubs: clusters of strongly connected memories (classical, no LLM).
//  5. Distill each new hub into one fact with a chat model, then have a separate
//     call verify the claim against its sources; unsupported claims are discarded.
//  6. Self-test: can recall still find each user decision from its situation?
//  7. Write the morning report. Every change is logged and can be undone.
//
// Steps 4 and 5 are skipped in a light cycle; step 5 is skipped when no chat model
// is available. Nothing is ever deleted outright: merged and archived memories stop
// being current and stay restorable.
package sleep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/llm"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
	"github.com/zakeripouya/EngramContextProtocol/internal/text"
)

// Options controls one cycle.
type Options struct {
	Light       bool     // decay, downscale and dedupe only
	SnapshotDir string   // where snapshots go; "" skips the snapshot
	KeepSnaps   int      // snapshots to keep; default 7
	Chat        llm.Chat // nil skips distillation and verification
	MaxHubs     int      // hubs distilled per night; default 5
}

// Distilled describes one claim the cycle proposed.
type Distilled struct {
	Fact     string `json:"fact"`
	Verdict  string `json:"verdict"` // supported, partial or unsupported
	Reason   string `json:"reason,omitempty"`
	Sources  int    `json:"sources"`
	MemoryID string `json:"memoryId,omitempty"` // set when kept
}

// SelfTest reports whether recall can still find each decision from its situation.
type SelfTest struct {
	Tested int      `json:"tested"`
	Found  int      `json:"found"`
	Misses []string `json:"misses,omitempty"`
}

// Report is the morning report.
type Report struct {
	RunID       int64       `json:"runId"`
	StartedAt   string      `json:"startedAt"`
	FinishedAt  string      `json:"finishedAt"`
	Light       bool        `json:"light"`
	Snapshot    string      `json:"snapshot,omitempty"`
	Decayed     int         `json:"decayed"`
	Pruned      int         `json:"pruned"`
	Downscaled  int         `json:"downscaled"`
	Merged      int         `json:"merged"`
	Archived    int         `json:"archived"`
	Hubs        int         `json:"hubs"`
	Distilled   []Distilled `json:"distilled"`
	SelfTest    SelfTest    `json:"selfTest"`
	Notes       []string    `json:"notes,omitempty"`
	ChangeCount int         `json:"changes"`
}

// Run executes one sleep cycle and stores its report.
func Run(ctx context.Context, st *store.Store, o Options) (Report, error) {
	if o.KeepSnaps <= 0 {
		o.KeepSnaps = 7
	}
	if o.MaxHubs <= 0 {
		o.MaxHubs = 5
	}
	rep := Report{Light: o.Light, StartedAt: store.Now().Format(time.RFC3339), Distilled: []Distilled{}}

	if o.SnapshotDir != "" {
		path, err := st.Snapshot(o.SnapshotDir, o.KeepSnaps)
		if err != nil {
			return rep, err
		}
		rep.Snapshot = path
	}
	runID, err := st.StartRun(o.Light, rep.Snapshot)
	if err != nil {
		return rep, err
	}
	rep.RunID = runID

	d, err := st.Decay()
	if err != nil {
		return rep, err
	}
	rep.Decayed, rep.Pruned = d.Decayed, d.Pruned
	if rep.Downscaled, err = st.Downscale(0.98, 14); err != nil {
		return rep, err
	}

	namespaces, err := st.Namespaces()
	if err != nil {
		return rep, err
	}
	for _, ns := range namespaces {
		mems, err := st.CurrentMemories(ns)
		if err != nil {
			return rep, err
		}
		merged, err := dedupe(st, runID, mems)
		if err != nil {
			return rep, err
		}
		rep.Merged += merged
		archived, err := archiveFaded(st, runID, mems)
		if err != nil {
			return rep, err
		}
		rep.Archived += archived

		if o.Light {
			continue
		}
		hubs, err := findHubs(st, ns, distilledIDs(mems))
		if err != nil {
			return rep, err
		}
		rep.Hubs += len(hubs)
		if o.Chat == nil || len(hubs) == 0 {
			continue
		}
		budget := o.MaxHubs - len(rep.Distilled)
		for _, hub := range hubs {
			if budget <= 0 {
				break
			}
			key := hubKey(hub)
			if done, err := st.KeyExists(key); err != nil {
				return rep, err
			} else if done {
				continue // this exact cluster was already distilled
			}
			budget--
			dist, err := distill(ctx, st, o.Chat, runID, ns, key, hub, mems)
			if err != nil {
				rep.Notes = append(rep.Notes, "distillation stopped: "+err.Error())
				break
			}
			if dist.Fact != "" {
				rep.Distilled = append(rep.Distilled, dist)
			}
		}
	}
	if !o.Light && o.Chat == nil {
		rep.Notes = append(rep.Notes, "no chat model available: distillation and verification skipped")
	}
	if rep.SelfTest, err = selfTest(st); err != nil {
		return rep, err
	}
	changes, err := st.Changes(runID)
	if err != nil {
		return rep, err
	}
	rep.ChangeCount = len(changes)
	rep.FinishedAt = store.Now().Format(time.RFC3339)
	return rep, st.FinishRun(runID, rep)
}

// dedupe merges near-identical memories of the same kind: the strongest (then oldest) is kept.
func dedupe(st *store.Store, run int64, mems []store.MemVec) (int, error) {
	if len(mems) > 3000 {
		mems = mems[len(mems)-3000:]
	}
	gone := map[string]bool{}
	merged := 0
	tokens := make([][]string, len(mems))
	for i, m := range mems {
		tokens[i] = text.Tokenize(m.Content)
	}
	for i := range mems {
		if gone[mems[i].ID] || mems[i].Kind == "decision" {
			continue
		}
		for j := i + 1; j < len(mems); j++ {
			a, b := mems[i], mems[j]
			if gone[b.ID] || b.Kind != a.Kind {
				continue
			}
			same := jaccard(tokens[i], tokens[j]) >= 0.9
			if !same && a.Vec != nil && b.Vec != nil {
				same = embed.Cosine(a.Vec, b.Vec) >= 0.97
			}
			if !same {
				continue
			}
			keep, drop := a, b
			if b.Strength > a.Strength {
				keep, drop = b, a
			}
			if err := st.Archive(drop.ID, keep.ID); err != nil {
				return merged, err
			}
			if err := st.RecordChange(run, "merged", drop.ID, keep.ID, store.Short(drop.Content)); err != nil {
				return merged, err
			}
			gone[drop.ID] = true
			merged++
			if drop.ID == a.ID {
				break
			}
		}
	}
	return merged, nil
}

// archiveFaded retires memories that faded out: weak, never recalled, and over 60 days old.
func archiveFaded(st *store.Store, run int64, mems []store.MemVec) (int, error) {
	cutoff := store.Now().Add(-60 * 24 * time.Hour)
	n := 0
	for _, m := range mems {
		if m.Kind == "decision" || m.Strength >= 0.1 || m.Activations > 0 {
			continue
		}
		created, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil || created.After(cutoff) {
			continue
		}
		if err := st.Archive(m.ID, ""); err != nil {
			continue // already merged this cycle
		}
		if err := st.RecordChange(run, "archived", m.ID, "", store.Short(m.Content)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// distilledIDs returns memories the sleep cycle itself wrote. They are excluded from
// hubs, or each night's fact would join its own sources and be distilled again.
func distilledIDs(mems []store.MemVec) map[string]bool {
	out := map[string]bool{}
	for _, m := range mems {
		if m.Provenance.Service == "sleep" {
			out[m.ID] = true
		}
	}
	return out
}

// findHubs groups memories joined by strong similarity or learned edges (weight ≥ 0.3)
// and returns groups of 3 to 12 memories, largest first.
func findHubs(st *store.Store, ns string, exclude map[string]bool) ([][]string, error) {
	edges, err := st.EdgesIn(ns, "similar", "coactive")
	if err != nil {
		return nil, err
	}
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if p, ok := parent[x]; ok && p != x {
			parent[x] = find(p)
			return parent[x]
		}
		parent[x] = x
		return x
	}
	for _, e := range edges {
		if e.Weight < 0.3 || exclude[e.From] || exclude[e.To] {
			continue
		}
		a, b := find(e.From), find(e.To)
		if a != b {
			parent[a] = b
		}
	}
	groups := map[string][]string{}
	for id := range parent {
		r := find(id)
		groups[r] = append(groups[r], id)
	}
	var hubs [][]string
	for _, g := range groups {
		if len(g) >= 3 && len(g) <= 12 {
			sort.Strings(g)
			hubs = append(hubs, g)
		}
	}
	sort.Slice(hubs, func(i, j int) bool {
		if len(hubs[i]) != len(hubs[j]) {
			return len(hubs[i]) > len(hubs[j])
		}
		return hubs[i][0] < hubs[j][0]
	})
	return hubs, nil
}

func hubKey(ids []string) string {
	h := sha256.Sum256([]byte(strings.Join(ids, ",")))
	return "sleep-hub:" + hex.EncodeToString(h[:8])
}

const distillPrompt = `You consolidate a person's notes into one durable fact.
Use only information stated in the notes; never add anything.
Reply with JSON only: {"fact": "one plain sentence", "sources": [numbers of the notes that state it]}.
If the notes do not share one durable fact, reply {"fact": ""}.`

const verifyPrompt = `You are a strict fact-checker. You did not write the claim.
Decide whether the claim is fully supported by the notes, partly supported, or unsupported.
A claim that adds anything the notes do not say is at most partly supported.
Reply with JSON only: {"verdict": "supported" | "partial" | "unsupported", "reason": "short reason"}.`

// distill proposes one fact for a hub, has it verified by a separate call, and keeps it
// only if it is supported. The verifier never grades a claim it wrote.
func distill(ctx context.Context, st *store.Store, chat llm.Chat, run int64, ns, key string, hub []string, mems []store.MemVec) (Distilled, error) {
	byID := map[string]store.MemVec{}
	for _, m := range mems {
		byID[m.ID] = m
	}
	var notes []store.MemVec
	for _, id := range hub {
		if m, ok := byID[id]; ok {
			notes = append(notes, m)
		}
	}
	if len(notes) < 3 {
		return Distilled{}, nil
	}
	var b strings.Builder
	for i, m := range notes {
		fmt.Fprintf(&b, "%d. %s\n", i+1, m.Content)
	}
	reply, err := chat.Complete(ctx, distillPrompt, "Notes:\n"+b.String())
	if err != nil {
		return Distilled{}, err
	}
	var proposal struct {
		Fact    string `json:"fact"`
		Sources []int  `json:"sources"`
	}
	if err := json.Unmarshal([]byte(llm.ExtractJSON(reply)), &proposal); err != nil || strings.TrimSpace(proposal.Fact) == "" {
		return Distilled{}, nil
	}
	var sources []store.MemVec
	seen := map[int]bool{}
	for _, n := range proposal.Sources {
		if n >= 1 && n <= len(notes) && !seen[n] {
			seen[n] = true
			sources = append(sources, notes[n-1])
		}
	}
	if len(sources) == 0 {
		return Distilled{Fact: proposal.Fact, Verdict: "unsupported", Reason: "cited no notes"}, nil
	}
	b.Reset()
	for i, m := range sources {
		fmt.Fprintf(&b, "%d. %s\n", i+1, m.Content)
	}
	reply, err = chat.Complete(ctx, verifyPrompt, "Notes:\n"+b.String()+"\nClaim: "+proposal.Fact)
	if err != nil {
		return Distilled{}, err
	}
	var check struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(llm.ExtractJSON(reply)), &check)
	out := Distilled{Fact: proposal.Fact, Verdict: check.Verdict, Reason: check.Reason, Sources: len(sources)}
	strength := 0.0
	switch check.Verdict {
	case "supported":
		strength = 0.45
	case "partial":
		strength = 0.3
	default:
		out.Verdict = "unsupported"
		return out, nil // discarded, but reported
	}
	id, created, err := st.InsertSystem(store.SystemMemory{
		Key: key, Kind: "fact", Namespace: ns, Service: "sleep", Content: proposal.Fact,
		Tags: []string{"distilled"}, Strength: strength,
	})
	if err != nil {
		return out, err
	}
	if created {
		for _, src := range sources {
			if err := st.AddEdge(id, src.ID, "derived", 1); err != nil {
				return out, err
			}
		}
		if err := st.RecordChange(run, "distilled", id, "", check.Verdict+": "+store.Short(proposal.Fact)); err != nil {
			return out, err
		}
	}
	out.MemoryID = id
	return out, nil
}

// selfTest checks, without side effects, whether each recent user decision is still
// found among the top three results for its own situation.
func selfTest(st *store.Store) (SelfTest, error) {
	decisions, err := st.Decisions("")
	if err != nil {
		return SelfTest{}, err
	}
	if len(decisions) > 20 {
		decisions = decisions[:20]
	}
	var res SelfTest
	for _, d := range decisions {
		q := d.Content
		if d.Decision != nil && d.Decision.Situation != "" {
			q = d.Decision.Situation
		}
		hits, err := st.RecallSystem(store.RecallInput{Query: q, K: 3, Namespaces: []string{d.Namespace}, Hops: 2})
		if err != nil {
			continue // a situation with no searchable words cannot be tested
		}
		res.Tested++
		found := false
		for _, h := range hits {
			if h.Memory.ID == d.ID {
				found = true
				break
			}
		}
		if found {
			res.Found++
		} else {
			res.Misses = append(res.Misses, store.Short(q))
		}
	}
	return res, nil
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
