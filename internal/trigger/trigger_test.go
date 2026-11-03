package trigger

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	st.SetEmbedder(embed.NewHashing(embed.DefaultHashDims))
	t.Cleanup(func() { st.Close() })
	return st
}

// seed stores a ticket and a user decision about the payout job, and pairs an agent.
func seed(t *testing.T, st *store.Store) (ticket, decision string) {
	t.Helper()
	if err := st.Pair("claude-code", []string{"team/ink", "flow/payouts"}, []string{"team/ink"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Pair("user-cli", []string{"team/ink"}, []string{"team/ink"}, ""); err != nil {
		t.Fatal(err)
	}
	r1, err := st.Observe("user-cli", store.ObserveInput{Kind: "fact", Namespace: "team/ink", Tags: []string{"payouts"},
		Content: "INK-142: the payouts job times out calling Stripe transfers during large batches"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := st.Observe("user-cli", store.ObserveInput{Kind: "decision", Author: "user", Namespace: "team/ink",
		Tags: []string{"payouts"}, Content: "Payouts retry Stripe timeouts with exponential backoff, at most 3 attempts",
		Decision: &store.Decision{Situation: "Stripe timeouts in the payouts job", Choice: "exponential backoff, 3 attempts"}})
	if err != nil {
		t.Fatal(err)
	}
	return r1.ID, r2.ID
}

// M8 done-when: a failed job surfaces its related ticket and decisions to a subscribed agent.
func TestFailedJobSurfacesTicketAndDecision(t *testing.T) {
	st := newStore(t)
	ticket, decision := seed(t, st)
	sub, err := st.Subscribe("claude-code", store.SubscribeInput{Context: "fixing the payouts job"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	res, err := RunJob(context.Background(), st, "payouts",
		[]string{"sh", "-c", "echo 'Stripe transfers timed out after 30s in batch 7' >&2; exit 3"}, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || !strings.Contains(out.String(), "timed out") {
		t.Fatalf("the job's exit code and output should pass through: %d %q", res.ExitCode, out.String())
	}
	if len(res.Notified) != 1 || res.Notified[0].SubscriptionID != sub.ID {
		t.Fatalf("want one notification for the subscription, got %+v", res.Notified)
	}
	pending, err := st.PendingNotifications("claude-code", true)
	if err != nil || len(pending) != 1 {
		t.Fatalf("want one pending notification, got %v %v", pending, err)
	}
	got := map[string]bool{}
	for _, m := range pending[0].Memories {
		got[m.ID] = true
	}
	if !got[res.MemoryID] || !got[ticket] || !got[decision] {
		t.Fatalf("the notification should carry the event, the ticket and the decision: %+v", pending[0].Memories)
	}
	if again, _ := st.PendingNotifications("claude-code", false); len(again) != 0 {
		t.Fatal("reading should mark notifications delivered")
	}
	// The same failure again does not re-send memories already delivered for this subscription,
	// but the new event itself is new, so it is sent with nothing repeated.
	res2, err := RunJob(context.Background(), st, "payouts", []string{"sh", "-c", "exit 3"}, io.Discard, io.Discard)
	if err != nil || len(res2.Notified) != 1 {
		t.Fatalf("a new failure should notify again: %v %+v", err, res2.Notified)
	}
	p2, _ := st.PendingNotifications("claude-code", true)
	for _, m := range p2[0].Memories {
		if m.ID == ticket || m.ID == decision {
			t.Fatalf("memories already sent must not repeat for the same subscription")
		}
	}
}

func TestSubscriberWithoutGrantIsNotNotified(t *testing.T) {
	st := newStore(t)
	seed(t, st)
	if err := st.Pair("cursor", []string{"team/ink"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Subscribe("cursor", store.SubscribeInput{Context: "payouts job", Threshold: 0.1}); err != nil {
		t.Fatal(err)
	}
	res, err := RunJob(context.Background(), st, "payouts", []string{"sh", "-c", "exit 1"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notified) != 0 {
		t.Fatal("cursor cannot read flow/payouts, so it must not be notified about it")
	}
}

func TestUnrelatedEventBelowThreshold(t *testing.T) {
	st := newStore(t)
	seed(t, st)
	if _, err := st.Subscribe("claude-code", store.SubscribeInput{Context: "redesigning the onboarding emails"}); err != nil {
		t.Fatal(err)
	}
	res, err := RunJob(context.Background(), st, "payouts", []string{"true"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notified) != 0 {
		t.Fatalf("an unrelated success should not notify: %+v", res.Notified)
	}
}

func post(t *testing.T, h http.Handler, path string, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestWebhookAuthPauseRateLimitAndSecrets(t *testing.T) {
	st := newStore(t)
	tr, err := st.AddTrigger("deploys", store.TriggerWebhook, "team/ink")
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(st)
	auth := map[string]string{"Authorization": "Bearer " + tr.Secret}
	if w := post(t, h, "/hooks/deploys", `{"title":"Deploy failed"}`, nil); w.Code != 401 {
		t.Fatalf("missing token: want 401, got %d", w.Code)
	}
	if w := post(t, h, "/hooks/deploys", `{"title":"Deploy failed"}`, map[string]string{"Authorization": "Bearer nope"}); w.Code != 401 {
		t.Fatalf("wrong token: want 401, got %d", w.Code)
	}
	if w := post(t, h, "/hooks/nope", `{}`, auth); w.Code != 404 {
		t.Fatalf("unknown trigger: want 404, got %d", w.Code)
	}
	w := post(t, h, "/hooks/deploys", `{"title":"Deploy of api failed","status":"failure","tags":["deploy"]}`, auth)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d %s", w.Code, w.Body)
	}
	evs, _ := st.TriggerEvents("deploys", 5)
	if len(evs) != 1 || evs[0].Status != "failed" {
		t.Fatalf("want one failed event, got %+v", evs)
	}
	if w := post(t, h, "/hooks/deploys", `{"title":"leaked ghp_`+"abcdefghijklmnopqrstuvwxyz0123456789"+`"}`, auth); w.Code != 422 {
		t.Fatalf("a secret must be refused with 422, got %d", w.Code)
	}
	if err := st.PauseTrigger("deploys", true); err != nil {
		t.Fatal(err)
	}
	if w := post(t, h, "/hooks/deploys", `{"title":"x"}`, auth); w.Code != 409 {
		t.Fatalf("paused: want 409, got %d", w.Code)
	}
	_ = st.PauseTrigger("deploys", false)
	old := RateLimit
	RateLimit = 2
	defer func() { RateLimit = old }()
	post(t, h, "/hooks/deploys", `{"title":"second"}`, auth)
	if w := post(t, h, "/hooks/deploys", `{"title":"third"}`, auth); w.Code != 429 {
		t.Fatalf("rate limit: want 429, got %d", w.Code)
	}
}

func TestGitHubSignatureAndWorkflowFailure(t *testing.T) {
	st := newStore(t)
	tr, err := st.AddTrigger("gh", store.TriggerGitHub, "")
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(st)
	body := `{"action":"completed","repository":{"full_name":"acme/api"},
		"workflow_run":{"name":"CI","conclusion":"failure","head_branch":"main","html_url":"https://github.com/acme/api/actions/runs/1"}}`
	if w := post(t, h, "/hooks/gh", body, map[string]string{"X-GitHub-Event": "workflow_run", "X-Hub-Signature-256": "sha256=00"}); w.Code != 401 {
		t.Fatalf("bad signature: want 401, got %d", w.Code)
	}
	hdr := map[string]string{"X-GitHub-Event": "workflow_run", "X-Hub-Signature-256": Sign(tr.Secret, []byte(body)), "X-GitHub-Delivery": "d-1"}
	if w := post(t, h, "/hooks/gh", body, hdr); w.Code != 200 {
		t.Fatalf("want 200, got %d %s", w.Code, w.Body)
	}
	// A redelivery with the same delivery id records nothing new.
	if w := post(t, h, "/hooks/gh", body, hdr); w.Code != 200 || !strings.Contains(w.Body.String(), `"created":false`) {
		t.Fatalf("redelivery should be idempotent: %s", w.Body)
	}
	mems, _ := st.CurrentMemories("flow/gh")
	if len(mems) != 1 || !strings.Contains(mems[0].Content, `CI workflow "CI" failure on main in acme/api`) {
		t.Fatalf("unexpected memories %+v", mems)
	}
	ping := `{"zen":"hi"}`
	if w := post(t, h, "/hooks/gh", ping, map[string]string{"X-GitHub-Event": "ping", "X-Hub-Signature-256": Sign(tr.Secret, []byte(ping))}); w.Code != 202 {
		t.Fatalf("ping should be accepted and ignored, got %d", w.Code)
	}
}
