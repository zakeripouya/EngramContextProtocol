package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
)

func TestAttendSurfacesConflictsGoalsAndModes(t *testing.T) {
	s := newStore(t)
	if err := s.Pair("claude-code", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	obs := func(kind, content string) string {
		in := ObserveInput{Kind: kind, Content: content}
		if kind == "decision" {
			in.Author = "user"
			in.Decision = &Decision{Situation: content, Choice: content}
		}
		r, err := s.Observe("claude-code", in)
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	a := obs("fact", "The release branch for the mobile app is cut on Mondays")
	b := obs("fact", "The release branch for the mobile app is cut on Thursdays")
	d := obs("decision", "Mobile releases ship behind a feature flag first")
	e := obs("episode", "Yesterday the mobile release was delayed by a flaky test")
	if err := s.MarkConflict("claude-code", b, a); err != nil {
		t.Fatal(err)
	}
	res, err := s.Attend("claude-code", AttendInput{Context: "preparing the mobile app release", Goals: []string{"ship behind a feature flag"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || !strings.Contains(res.Conflicts[0].Summary, "Thursdays") {
		t.Fatalf("want the Monday/Thursday conflict, got %+v", res.Conflicts)
	}
	if len(res.Memories) == 0 || len(res.Memories) > 4 {
		t.Fatalf("attend returns a handful of items, got %d", len(res.Memories))
	}
	if res.Memories[0].Memory.ID != d || !strings.Contains(res.Memories[0].Reason, "serves goal") {
		t.Fatalf("the decision serving the goal should lead: %+v", res.Memories[0])
	}
	detail, err := s.Attend("claude-code", AttendInput{Context: "mobile release delayed flaky test", Mode: "detail"})
	if err != nil || detail.Memories[0].Memory.ID != e {
		t.Fatalf("detail mode should lead with the specific episode, got %+v %v", detail.Memories, err)
	}
	var ee *ecp.Error
	if _, err := s.Attend("claude-code", AttendInput{Context: "x", Mode: "dream"}); !errors.As(err, &ee) || ee.Code != ecp.InvalidParams {
		t.Fatalf("unknown mode should be invalid params, got %v", err)
	}
	// Attend never trains: no Hebbian pathways from being shown things.
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM edges WHERE kind = 'coactive'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("attend must not create coactive pathways, got %d", n)
	}
}

func TestSubscriptionLifecycle(t *testing.T) {
	s := newStore(t)
	if err := s.Pair("claude-code", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	var e *ecp.Error
	if _, err := s.Subscribe("claude-code", SubscribeInput{Context: "x", Namespaces: []string{"team/secret"}}); !errors.As(err, &e) || e.Code != ecp.ScopeDenied {
		t.Fatalf("cannot subscribe to unreadable namespaces, got %v", err)
	}
	sub, err := s.Subscribe("claude-code", SubscribeInput{Context: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	if subs, _ := s.Subscriptions(); len(subs) != 1 || subs[0].Threshold != 0.6 {
		t.Fatalf("want one live subscription with the default threshold, got %+v", subs)
	}
	if err := s.Unsubscribe("someone-else", sub.ID); err == nil {
		t.Fatal("only the owner can unsubscribe")
	}
	if err := s.Unsubscribe("claude-code", sub.ID); err != nil {
		t.Fatal(err)
	}
	if subs, _ := s.Subscriptions(); len(subs) != 0 {
		t.Fatal("subscription should be gone")
	}
}
