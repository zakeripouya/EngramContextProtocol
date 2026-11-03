package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
)

func pairBoth(t *testing.T, s *Store) {
	t.Helper()
	for _, c := range []string{"claude-code", "codex"} {
		if err := s.Pair(c, []string{"me"}, []string{"me"}, ""); err != nil {
			t.Fatal(err)
		}
	}
}

// M7 done-when: a task started in one agent is finished in another without re-explaining.
func TestHandoffBetweenAgents(t *testing.T) {
	s := newStore(t)
	pairBoth(t, s)
	cp, err := s.SaveCheckpoint("claude-code", Checkpoint{
		Goal: "Add rate limiting to the API", Plan: []string{"middleware", "config", "tests"},
		Done: []string{"middleware"}, Next: "add the config flag", Files: []string{"api/limit.go"},
		Environment: &Environment{Repo: "github.com/acme/api", Tools: []string{"go", "docker"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	open, err := s.ListSessions("codex", "open")
	if err != nil || len(open) != 1 || open[0].SessionID != cp.SessionID {
		t.Fatalf("codex should see the open session, got %v %v", open, err)
	}
	res, err := s.Resume("codex", cp.SessionID, &Environment{Repo: "/home/p/src/acme/api", Tools: []string{"go"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Checkpoint.Next != "add the config flag" || len(res.Checkpoint.Done) != 1 {
		t.Fatalf("checkpoint not carried over: %+v", res.Checkpoint)
	}
	if len(res.Missing) != 1 || !strings.Contains(res.Missing[0], "docker") {
		t.Fatalf("want only docker missing (the repo matches by owner/name), got %v", res.Missing)
	}
	// Codex updates progress with only the changed fields; the rest is kept.
	up, err := s.SaveCheckpoint("codex", Checkpoint{SessionID: cp.SessionID, Done: []string{"middleware", "config"}, Next: "tests"})
	if err != nil {
		t.Fatal(err)
	}
	if up.Goal != cp.Goal || len(up.Plan) != 3 || up.LastClient != "codex" || up.CreatedBy != "claude-code" {
		t.Fatalf("partial update lost fields: %+v", up)
	}
	if _, err := s.SaveCheckpoint("codex", Checkpoint{SessionID: cp.SessionID, Status: "done", Result: "rate limiting shipped"}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Recall("claude-code", RecallInput{Query: "rate limiting API task"})
	if err != nil || len(hits) == 0 || !strings.Contains(hits[0].Memory.Content, "Finished task") {
		t.Fatalf("a finished task should become an episode, got %v %v", hits, err)
	}
	if done, _ := s.ListSessions("claude-code", "done"); len(done) != 1 {
		t.Fatalf("want one done session, got %d", len(done))
	}
}

func TestDelegationReturnsResultToParent(t *testing.T) {
	s := newStore(t)
	pairBoth(t, s)
	parent, err := s.SaveCheckpoint("claude-code", Checkpoint{Goal: "Release v2", Environment: &Environment{Repo: "acme/api"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Delegate("claude-code", parent.SessionID, "Write the migration guide", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentID != parent.SessionID || child.Environment == nil || child.Environment.Repo != "acme/api" {
		t.Fatalf("child should inherit the parent's environment: %+v", child)
	}
	list, _ := s.ListSessions("codex", "open")
	if len(list) != 2 || list[0].SessionID != child.SessionID {
		t.Fatalf("the subtask delegated to codex should come first, got %+v", list)
	}
	if _, err := s.SaveCheckpoint("codex", Checkpoint{SessionID: child.SessionID, Status: "done", Result: "docs/migrate.md written"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Resume("claude-code", parent.SessionID, &Environment{Repo: "acme/api"})
	if err != nil {
		t.Fatal(err)
	}
	last := res.Checkpoint.Done[len(res.Checkpoint.Done)-1]
	if !strings.Contains(last, "docs/migrate.md written") || len(res.Subtasks) != 1 || res.Subtasks[0].Status != "done" {
		t.Fatalf("the result should return to the parent: %v %+v", res.Checkpoint.Done, res.Subtasks)
	}
	if len(res.Missing) != 0 {
		t.Fatalf("nothing should be missing, got %v", res.Missing)
	}
}

func TestSessionScopesAndSecrets(t *testing.T) {
	s := newStore(t)
	pairBoth(t, s)
	if err := s.Pair("cursor", []string{"team/ink"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	cp, err := s.SaveCheckpoint("claude-code", Checkpoint{Goal: "Private task"})
	if err != nil {
		t.Fatal(err)
	}
	var e *ecp.Error
	if _, err := s.Resume("cursor", cp.SessionID, nil); !errors.As(err, &e) || e.Code != ecp.ScopeDenied {
		t.Fatalf("cursor cannot read me, got %v", err)
	}
	if l, _ := s.ListSessions("cursor", ""); len(l) != 0 {
		t.Fatalf("cursor should list nothing, got %d", len(l))
	}
	_, err = s.SaveCheckpoint("claude-code", Checkpoint{Goal: "deploy", Next: "use token ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"})
	if !errors.As(err, &e) || e.Code != ecp.ContentRejected {
		t.Fatalf("secrets must be refused, got %v", err)
	}
}
