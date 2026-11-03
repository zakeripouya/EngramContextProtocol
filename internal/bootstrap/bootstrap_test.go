package bootstrap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	t.Cleanup(func() { st.Close() })
	st.SetEmbedder(embed.NewHashing(embed.DefaultHashDims))
	if err := st.Pair("claude-code", []string{"team/ink", "pack/developer"}, []string{"team/ink"}, ""); err != nil {
		t.Fatal(err)
	}
	return st
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A small repository whose history follows a ticket-key convention.
func makeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := filepath.Join(t.TempDir(), "ink")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 6; i++ {
		write("api/payouts.go", fmt.Sprintf("package api // %d", i))
		write("api/payouts_test.go", fmt.Sprintf("package api // test %d", i))
		if i%2 == 0 {
			write("web/README.md", fmt.Sprintf("v%d", i))
		}
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "-m", fmt.Sprintf("INKIN-%d: adjust payout rounding", 200+i))
		git(t, dir, "branch", fmt.Sprintf("INKIN-%d-payout-fix", 200+i))
	}
	return dir
}

func TestImportGit(t *testing.T) {
	st := newStore(t)
	repo := makeRepo(t)
	res, err := ImportGit(context.Background(), st, GitOptions{Repo: repo, Namespace: "team/ink"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Commits != 6 || res.Files != 3 {
		t.Fatalf("want 6 commits and 3 recurring files, got %+v", res)
	}
	if res.CoChange < 1 {
		t.Fatalf("payouts.go and its test always change together; want a co-change edge, got %+v", res)
	}
	joined := strings.Join(res.Conventions, " | ")
	if !strings.Contains(joined, "Branch names start with the ticket key, like INKIN") ||
		!strings.Contains(joined, "Commit messages start with the ticket key") {
		t.Fatalf("conventions not detected: %v", res.Conventions)
	}

	// The M3 done-check in miniature: an agent asking how to name a branch finds the convention.
	hits, err := st.Recall("claude-code", store.RecallInput{Query: "how should I name my branch", Hops: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || !strings.Contains(hits[0].Memory.Content, "Branch names start with the ticket key") {
		t.Fatalf("convention not recalled first: %+v", hits)
	}
	if hits[0].Memory.Provenance.Author != "system" || hits[0].Memory.Provenance.Service != "git" {
		t.Fatalf("imported memories must carry system provenance: %+v", hits[0].Memory.Provenance)
	}

	again, err := ImportGit(context.Background(), st, GitOptions{Repo: repo, Namespace: "team/ink"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Commits != 0 || again.Skipped != 6 || len(again.Conventions) != 0 {
		t.Fatalf("a second import must not duplicate anything: %+v", again)
	}
}

func TestDetectConventionsNeedsEvidence(t *testing.T) {
	if got := DetectConventions([]string{"INKIN-1: a", "fix stuff"}, nil); len(got) != 0 {
		t.Fatalf("too few examples should detect nothing, got %v", got)
	}
	subjects := []string{"feat: a", "fix: b", "chore(deps): c", "feat!: d", "docs: e", "random"}
	if got := DetectConventions(subjects, nil); len(got) != 1 || !strings.Contains(got[0], "Conventional Commits") {
		t.Fatalf("want Conventional Commits, got %v", got)
	}
}

func TestImportRecordsAndJira(t *testing.T) {
	st := newStore(t)
	res, err := ImportRecords(st, strings.NewReader(`[
		{"key": "n1", "kind": "fact", "content": "Production runs on a server in Europe", "tags": ["hosting"]},
		{"content": "Deploys go straight to prod; there is no staging yet"}]`), "team/ink", "")
	if err != nil || res.Imported != 2 {
		t.Fatalf("got %+v, %v", res, err)
	}
	if _, err := ImportRecords(st, strings.NewReader(`[{"kind": "decision", "content": "x"}]`), "team/ink", ""); err == nil {
		t.Fatal("imports must not create decisions")
	}

	jira := `{"issues": [{"key": "INKIN-212", "fields": {"summary": "Payout job fails on refunds",
		"created": "2026-10-01T09:30:00.000-0400", "issuetype": {"name": "Bug"}, "status": {"name": "Done"}, "labels": ["payments"]}}]}`
	jr, err := ImportJira(st, strings.NewReader(jira), "team/ink")
	if err != nil || jr.Imported != 1 {
		t.Fatalf("got %+v, %v", jr, err)
	}
	hits, err := st.Recall("claude-code", store.RecallInput{Query: "payout refunds bug"})
	if err != nil || len(hits) == 0 || !strings.HasPrefix(hits[0].Memory.Content, "INKIN-212 (Bug, Done)") {
		t.Fatalf("jira issue not recalled: %+v %v", hits, err)
	}
	if !strings.HasPrefix(hits[0].Memory.ValidFrom, "2026-10-01T13:30:00Z") {
		t.Fatalf("issue creation time should become validFrom in UTC, got %s", hits[0].Memory.ValidFrom)
	}
}

func TestDeveloperPack(t *testing.T) {
	st := newStore(t)
	p, err := LoadPack("developer")
	if err != nil {
		t.Fatal(err)
	}
	res, err := InstallPack(st, p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Namespace != "pack/developer" || res.Memories != len(p.Memories) || res.Edges != len(p.Edges) {
		t.Fatalf("unexpected install result %+v", res)
	}
	again, err := InstallPack(st, p)
	if err != nil || again.Memories != 0 {
		t.Fatalf("reinstalling must not duplicate memories: %+v %v", again, err)
	}
	hits, err := st.Recall("claude-code", store.RecallInput{Query: "database migration risks", Namespaces: []string{"pack/developer"}, Hops: 1})
	if err != nil || len(hits) == 0 {
		t.Fatalf("pack knowledge not recalled: %v", err)
	}
	found := false
	for _, h := range hits {
		if strings.Contains(h.Memory.Content, "staging first") {
			found = true
		}
	}
	if !found {
		t.Fatalf("migration safety fact should be reachable: %+v", hits)
	}
	n, err := st.RemoveNamespace("pack/developer")
	if err != nil || n != len(p.Memories) {
		t.Fatalf("uninstall removed %d, %v", n, err)
	}
}
