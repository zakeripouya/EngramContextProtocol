package eval

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Four weeks of permission prompts: tests and reads are always approved, pushes
// are approved only when the same command was approved before, and deletes are
// usually denied.
func TestPermitModelOnSimulatedPrompts(t *testing.T) {
	clock := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	prev := store.Now
	store.Now = func() time.Time { return clock }
	defer func() { store.Now = prev }()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rng := rand.New(rand.NewSource(3))
	kinds := []struct {
		tool, category, tier string
		approve              func(seen int) bool
	}{
		{"Bash", "Bash:go test", "eligible", func(int) bool { return true }},
		{"Read", "Read:project", "eligible", func(int) bool { return rng.Float64() < 0.98 }},
		{"Bash", "Bash:git push", "eligible", func(seen int) bool { return seen > 0 && rng.Float64() < 0.9 }},
		{"Bash", "Bash:rm", "eligible", func(int) bool { return rng.Float64() < 0.1 }},
		{"WebFetch", "WebFetch:docs", "unknown", func(int) bool { return rng.Float64() < 0.7 }},
	}
	seen := map[string]int{}
	for i := 0; i < 400; i++ {
		clock = clock.Add(time.Duration(10+rng.Intn(90)) * time.Minute)
		k := kinds[rng.Intn(len(kinds))]
		input := fmt.Sprintf("%s %d", k.category, rng.Intn(3))
		hash := fmt.Sprintf("h-%s", input)
		a, d, f, _ := st.PermitPrecedents("claude-code", k.category)
		n := a + d
		conf := (float64(a-f) + 1) / (float64(n) + 2)
		pred := "ask"
		if k.tier == "eligible" && n >= 8 && conf >= 0.9 {
			pred = "allow"
		}
		if _, err := st.LogPermit(store.PermitRequest{Client: "claude-code", SessionID: "s1", Tool: k.tool, InputHash: hash,
			Summary: input, Category: k.category, Tier: k.tier, Predicted: pred, Confidence: conf, Precedents: n}); err != nil {
			t.Fatal(err)
		}
		if k.approve(seen[hash]) {
			seen[hash]++
			if _, err := st.ResolvePermitRan("claude-code", "s1", k.tool, hash, ""); err != nil {
				t.Fatal(err)
			}
		} else {
			clock = clock.Add(time.Second)
			if _, err := st.InferPermitDenials(0, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	rep, m, err := EvaluatePermits(st, 0.3, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Requests != 400 || rep.Test != 120 || len(rep.Systems) != 2 || m == nil {
		t.Fatalf("report %+v", rep)
	}
	rule, lr := rep.Systems[0], rep.Systems[1]
	if lr.ROCAUC < 0.85 {
		t.Errorf("the model should separate approvals from denials: ROC-AUC %.3f", lr.ROCAUC)
	}
	if rule.Allowed == 0 || rule.Precision.Mean < 0.9 {
		t.Errorf("rule %+v", rule)
	}
	if !strings.Contains(rep.Weights, "same_input") && !strings.Contains(rep.Weights, "beta") {
		t.Errorf("weights %s should rest on precedents", rep.Weights)
	}
	g := PermitGate(rep)
	if g.Reason == "" {
		t.Fatal("gate gave no reason")
	}
	// Features never look at the request's own outcome.
	d, _ := BuildPermitDataset(st)
	if d.X[0][2] != 0 || d.X[0][3] != 0 {
		t.Fatalf("the first request has precedents: %v", d.X[0])
	}
}

const locomoSample = `[{"sample_id": "conv-1",
 "conversation": {"speaker_a": "Ana", "speaker_b": "Ben",
  "session_1_date_time": "1:56 pm on 8 May, 2023",
  "session_1": [
   {"speaker": "Ana", "dia_id": "D1:1", "text": "I adopted a greyhound named Pixel last weekend."},
   {"speaker": "Ben", "dia_id": "D1:2", "text": "Congrats! I started learning the cello."},
   {"speaker": "Ana", "dia_id": "D1:3", "text": "Pixel loves the beach.", "blip_caption": "a dog running on sand"}],
  "session_2_date_time": "10:00 am on 20 May, 2023",
  "session_2": [
   {"speaker": "Ben", "dia_id": "D2:1", "text": "My cello teacher moved to Lisbon."},
   {"speaker": "Ana", "dia_id": "D2:2", "text": "I am training for a half marathon in October."}]},
 "qa": [
  {"question": "What is the name of Ana's dog?", "answer": "Pixel", "evidence": ["D1:1"], "category": 4},
  {"question": "Where did Ben's cello teacher move?", "answer": "Lisbon", "evidence": ["D2:1; D1:2"], "category": 1},
  {"question": "When does Ana run the half marathon?", "answer": 2023, "evidence": ["D2:2"], "category": 2},
  {"question": "What did Ben say about Paris?", "evidence": ["D1:2"], "category": 5, "adversarial_answer": "nothing"}]}]`

const longMemEvalSample = `[{"question_id": "q1", "question_type": "single-session-user",
 "question": "What breed is the dog I adopted?", "answer": "a greyhound", "question_date": "2023/05/30 (Tue) 23:40",
 "haystack_session_ids": ["s-a", "s-b"], "haystack_dates": ["2023/05/01 (Mon) 10:00", "2023/05/20 (Sat) 09:00"],
 "haystack_sessions": [
  [{"role": "user", "content": "I just adopted a greyhound.", "has_answer": true},
   {"role": "assistant", "content": "Congratulations on the new dog!"}],
  [{"role": "user", "content": "Can you suggest a cello practice plan?"},
   {"role": "assistant", "content": "Practice scales for ten minutes a day."}]],
 "answer_session_ids": ["s-a"]},
 {"question_id": "q2_abs", "question_type": "single-session-user", "question": "What is my cat's name?", "answer": "unknown",
  "question_date": "2023/05/30", "haystack_session_ids": [], "haystack_dates": [], "haystack_sessions": [], "answer_session_ids": []}]`

type fakeChat struct{ calls int }

func (f *fakeChat) Model() string { return "fake" }

func (f *fakeChat) Complete(_ context.Context, system, user string) (string, error) {
	f.calls++
	if strings.HasPrefix(system, "You grade") {
		if strings.Contains(strings.ToLower(user), "answer to grade: pixel") {
			return "CORRECT", nil
		}
		return "WRONG", nil
	}
	if strings.Contains(user, "Pixel") && strings.Contains(user, "dog") {
		return "Pixel", nil
	}
	return "unknown", nil
}

func TestBenchmarks(t *testing.T) {
	samples, err := LoadLoCoMo(strings.NewReader(locomoSample), false)
	if err != nil {
		t.Fatal(err)
	}
	s := samples[0]
	if len(s.Turns) != 5 || len(s.Questions) != 3 {
		t.Fatalf("turns %d questions %d (the adversarial one is skipped)", len(s.Turns), len(s.Questions))
	}
	if !strings.Contains(s.Turns[2].Text, "a dog running on sand") || s.Turns[3].When.Before(s.Turns[2].When) {
		t.Fatalf("turn %+v", s.Turns[2])
	}
	if q := s.Questions[1]; len(q.Evidence) != 2 || q.Category != "multi-hop" {
		t.Fatalf("evidence split: %+v", q)
	}
	if s.Questions[2].Answer != "2023" {
		t.Fatalf("numeric answers are kept as text: %q", s.Questions[2].Answer)
	}
	chat := &fakeChat{}
	rep, err := RunBench(context.Background(), "locomo", samples, BenchOptions{K: 3, LinkTurns: true, Chat: chat, QAConfigs: []string{"full"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Questions != 3 || len(rep.Systems) != 5 || len(rep.Warnings) == 0 {
		t.Fatalf("report %+v", rep)
	}
	var full BenchSystem
	for _, sys := range rep.Systems {
		if sys.Name == "full" {
			full = sys
		}
	}
	if full.RecallAtK.Mean < 0.5 || full.ByCategory["single-hop"] != 1 || full.Accuracy == nil || full.ContextTokens == 0 {
		t.Fatalf("full = %+v", full)
	}
	if full.Accuracy.Mean < 0.3 || chat.calls != 6 {
		t.Fatalf("accuracy %+v after %d chat calls (answer and judge per question)", full.Accuracy, chat.calls)
	}

	lme, err := LoadLongMemEval(strings.NewReader(longMemEvalSample))
	if err != nil {
		t.Fatal(err)
	}
	if len(lme) != 1 || len(lme[0].Turns) != 4 || strings.Join(lme[0].Questions[0].Evidence, ",") != "s-a:0" {
		t.Fatalf("longmemeval %+v (the abstention question is skipped; evidence is the marked turn)", lme)
	}
	rep2, err := RunBench(context.Background(), "longmemeval", lme, BenchOptions{K: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Systems[len(rep2.Systems)-1].RecallAtK.Mean != 1 {
		t.Fatalf("the greyhound turn should be found: %+v", rep2.Systems)
	}
}
