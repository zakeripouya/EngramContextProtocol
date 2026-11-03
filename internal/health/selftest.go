package health

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/sleep"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Self-tests run against a sandbox copy of the user's memory, never the real one.
// Expected answers come from what was stored, never from distilled facts, so
// Engram cannot pass tests against its own hallucinations.

// TestResult is one self-test.
type TestResult struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // golden, programmatic or mutation
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// SelfTest copies the store into a temporary sandbox and runs the golden suite,
// programmatic checks and mutation tests there.
func SelfTest(ctx context.Context, st *store.Store) ([]TestResult, error) {
	dir, err := os.MkdirTemp("", "engram-selftest-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	snap, err := st.Snapshot(dir, 0)
	if err != nil {
		return nil, err
	}
	sb, err := store.Open(snap)
	if err != nil {
		return nil, err
	}
	defer sb.Close()
	if e := st.Embedder(); e != nil {
		sb.SetEmbedder(e)
	} else {
		sb.SetEmbedder(embed.NewHashing(embed.DefaultHashDims))
	}

	var out []TestResult
	golden, err := sb.GoldenSuite()
	if err != nil {
		return nil, err
	}
	for _, g := range golden {
		hits, err := sb.RecallSystem(store.RecallInput{Query: g.Query, K: 3, Namespaces: []string{g.Namespace}, Hops: 2})
		r := TestResult{Name: fmt.Sprintf("golden %d: %q", g.ID, g.Query), Kind: "golden"}
		if err != nil {
			r.Detail = err.Error()
		} else {
			for _, h := range hits {
				if h.Memory.ID == g.ExpectID {
					r.Passed = true
				}
			}
			if !r.Passed {
				r.Detail = "expected memory " + g.ExpectID + " not in the top 3"
			}
		}
		out = append(out, r)
	}

	ns := "selftest/sandbox"
	insert := func(key, content string) (string, error) {
		id, _, err := sb.InsertSystem(store.SystemMemory{Key: "selftest:" + key, Kind: "fact", Namespace: ns,
			Service: "selftest", Content: content, Strength: 0.5})
		return id, err
	}

	// Programmatic: a superseded fact is not returned as current.
	{
		r := TestResult{Name: "superseded facts are not current", Kind: "programmatic"}
		old, err1 := insert("old", "The selftest widget color is blue")
		newer, err2 := insert("new", "The selftest widget color is green")
		if err := errors.Join(err1, err2); err != nil {
			return nil, err
		}
		if err := sb.Archive(old, newer); err != nil {
			return nil, err
		}
		hits, err := sb.RecallSystem(store.RecallInput{Query: "selftest widget color", Namespaces: []string{ns}})
		r.Passed = err == nil
		for _, h := range hits {
			if h.Memory.ID == old {
				r.Passed, r.Detail = false, "the superseded fact was recalled as current"
			}
		}
		out = append(out, r)
	}

	// Mutation: an injected duplicate is merged by a light sleep cycle.
	{
		r := TestResult{Name: "injected duplicate is merged", Kind: "mutation"}
		a, err1 := insert("dup-a", "Selftest deploys always run the smoke suite first")
		_, err2 := insert("dup-b", "selftest deploys always run the smoke suite first!")
		if err := errors.Join(err1, err2); err != nil {
			return nil, err
		}
		rep, err := sleep.Run(ctx, sb, sleep.Options{Light: true})
		if err != nil {
			return nil, err
		}
		r.Passed = rep.Merged >= 1
		if mems, err := sb.CurrentMemories(ns); err == nil {
			kept := false
			for _, m := range mems {
				kept = kept || m.ID == a
			}
			r.Passed = r.Passed && kept // the original survives; the copy is merged into it
		}
		if !r.Passed {
			r.Detail = "the duplicate survived consolidation"
		}
		out = append(out, r)
	}

	// Mutation: a poisoned memory carrying a secret is refused.
	{
		r := TestResult{Name: "poisoned memory with a secret is refused", Kind: "mutation"}
		_, _, err := sb.InsertSystem(store.SystemMemory{Key: "selftest:poison", Kind: "fact", Namespace: ns,
			Service: "selftest", Content: "use token ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789 for deploys"})
		var e *ecp.Error
		r.Passed = errors.As(err, &e) && e.Code == ecp.ContentRejected
		if !r.Passed {
			r.Detail = "the secret was accepted"
		}
		out = append(out, r)
	}

	// Mutation: a runaway hub is caught by the health bounds.
	{
		r := TestResult{Name: "runaway hub breaches the degree bound", Kind: "mutation"}
		center, err := insert("hub", "Selftest central node")
		if err != nil {
			return nil, err
		}
		for i := 0; i <= MaxHub; i++ {
			id, err := insert(fmt.Sprintf("spoke-%d", i), fmt.Sprintf("Selftest spoke number %d", i))
			if err != nil {
				return nil, err
			}
			if err := sb.AddEdge(center, id, "coactive", 0.5); err != nil {
				return nil, err
			}
		}
		m, err := Measure(sb, -1)
		if err != nil {
			return nil, err
		}
		rep := Evaluate(m, nil)
		for _, b := range rep.Bounds {
			if b.Name == "Largest hub degree" && b.Status == Breach {
				r.Passed = true
			}
		}
		if !r.Passed {
			r.Detail = "the bound did not trip"
		}
		out = append(out, r)
	}
	return out, nil
}
