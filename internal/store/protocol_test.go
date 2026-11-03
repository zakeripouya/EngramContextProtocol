package store

import (
	"errors"
	"testing"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
)

func TestFindPathRules(t *testing.T) {
	s := newStore(t)
	if err := s.Pair("cli", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Pair("other", []string{"team/x"}, []string{"team/x"}, ""); err != nil {
		t.Fatal(err)
	}
	id := func(c string) string {
		r, err := s.Observe("cli", ObserveInput{Kind: "fact", Content: c})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	a, b, c, d, e := id("alpha node"), id("bravo node"), id("charlie node"), id("delta node"), id("echo node")
	// Route 1: a-b-e, one strong link and one weak link (0.9 · 0.3 = 0.27, weakest 0.3).
	// Route 2: a-c-d-e, three medium links (0.6³ = 0.216, weakest 0.6).
	for _, x := range []struct {
		from, to string
		w        float64
	}{{a, b, 0.9}, {b, e, 0.3}, {a, c, 0.6}, {c, d, 0.6}, {d, e, 0.6}} {
		if err := s.AddEdge(x.from, x.to, "coactive", x.w); err != nil {
			t.Fatal(err)
		}
	}
	mm, err := s.FindPath("cli", a, e, "maximin")
	if err != nil || len(mm.Path) != 4 || mm.Path[1] != c || mm.WeakestLink.Weight != 0.6 {
		t.Fatalf("maximin should take the route whose weakest link is strongest: %+v %v", mm, err)
	}
	mp, err := s.FindPath("cli", a, e, "most_probable")
	if err != nil || len(mp.Path) != 3 || mp.Path[1] != b || mp.WeakestLink.Weight != 0.3 {
		t.Fatalf("most_probable should take the route with the highest product: %+v %v", mp, err)
	}
	// An archived middle node breaks the route.
	if err := s.Archive(c, ""); err != nil {
		t.Fatal(err)
	}
	if mm, _ := s.FindPath("cli", a, e, "maximin"); len(mm.Path) != 3 {
		t.Fatalf("archived memories are not on paths: %+v", mm)
	}
	var ee *ecp.Error
	if _, err := s.FindPath("other", a, e, ""); !errors.As(err, &ee) || ee.Code != ecp.ScopeDenied {
		t.Fatalf("a client that cannot read the endpoints is denied: %v", err)
	}
	if _, err := s.FindPath("cli", a, e, "shortest"); !errors.As(err, &ee) || ee.Code != ecp.InvalidParams {
		t.Fatalf("unknown rule: %v", err)
	}
	if p, _ := s.FindPath("cli", a, id("unconnected node"), ""); len(p.Path) != 0 {
		t.Fatalf("no path should be empty, got %+v", p)
	}
}

func TestTokens(t *testing.T) {
	s := newStore(t)
	tok, err := s.CreateToken("ci", true)
	if err != nil {
		t.Fatal(err)
	}
	c, service, err := s.AuthToken(tok)
	if err != nil || c != "ci" || !service {
		t.Fatalf("got %q %v %v", c, service, err)
	}
	if _, _, err := s.AuthToken(tok + "x"); !errors.Is(err, ErrBadToken) {
		t.Fatal("a wrong token must fail")
	}
	if n, _ := s.RevokeTokens("ci"); n != 1 {
		t.Fatal("want one revoked")
	}
	if _, _, err := s.AuthToken(tok); !errors.Is(err, ErrBadToken) {
		t.Fatal("a revoked token must fail")
	}
	var stored int
	_ = s.db.QueryRow(`SELECT count(*) FROM tokens WHERE hash = ?`, tok).Scan(&stored)
	if stored != 0 {
		t.Fatal("tokens must be stored hashed")
	}
}
