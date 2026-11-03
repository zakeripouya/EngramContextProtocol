package bootstrap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const designDoc = `---
title: design
---
# INK design

Intro paragraph that is long enough to keep as a section of the design.

## Payments

Fees are a flat 4.5% plus five dollars, collected through Stripe Connect. See api/payouts.go for the code.

` + "```" + `
# not a heading inside a fence
` + "```" + `

## Independent artists

Every independent artist gets a shop row with an independent-artist flag, not a separate table.

<!-- a comment that should vanish -->
`

// A workspace with two projects, one dependency checkout that must be skipped,
// and docs, agent rules, a manifest and a leaked secret.
func makeWorkspace(t *testing.T) (root, ink string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root = t.TempDir()
	ink = filepath.Join(root, "INK")
	for _, d := range []string{ink, filepath.Join(root, "side", "EasyScript"), filepath.Join(root, "node_modules", "dep"), filepath.Join(root, ".cache", "clone")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		git(t, d, "init", "-q", "-b", "main")
	}
	writeFile(t, ink, "CLAUDE.md", "# Conventions\n\nOne component per file on both backend and frontend, always.\n")
	writeFile(t, ink, "README.md", "# INK\n\nTattoo studio management platform for shops and independent artists.\n")
	writeFile(t, ink, "docs/design.md", designDoc)
	writeFile(t, ink, "docs/keys.md", "# Keys\n\nThe staging key is AKIAABCDEFGHIJKLMNOP and must not leak anywhere.\n")
	writeFile(t, ink, "CHANGELOG.md", "# Changelog\n\nLots of release notes that are not worth absorbing at all.\n")
	writeFile(t, ink, "package.json", `{"dependencies":{"next":"15.0.0","react":"19.0.0","stripe":"1"},"devDependencies":{"typescript":"5"}}`)
	for i := 1; i <= 4; i++ {
		writeFile(t, ink, "api/payouts.go", fmt.Sprintf("package api // %d", i))
		writeFile(t, ink, "apps/web/page.tsx", fmt.Sprintf("export const v = %d", i))
		writeFile(t, ink, "apps/web/shop.tsx", fmt.Sprintf("export const s = %d", i))
		git(t, ink, "add", "-A")
		git(t, ink, "commit", "-q", "-m", fmt.Sprintf("INKIN-%d: change payouts", i))
	}
	es := filepath.Join(root, "side", "EasyScript")
	writeFile(t, es, "main.c", "int main(void) { return 0; }")
	git(t, es, "add", "-A")
	git(t, es, "commit", "-q", "-m", "first")
	return root, ink
}

func TestProposeNamespace(t *testing.T) {
	for in, want := range map[string]string{"INK": "ink", "EasyScript": "easyscript",
		"EngramContextProtocol": "engram-context-protocol", "my_cool app": "my-cool-app", "...": "project"} {
		if got := ProposeNamespace(in); got != want {
			t.Errorf("ProposeNamespace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChunkMarkdown(t *testing.T) {
	chunks := chunkMarkdown([]byte(designDoc))
	var labels []string
	for _, c := range chunks {
		labels = append(labels, headingLabel(c.Heading))
		if strings.Contains(c.Text, "comment that should vanish") || strings.Contains(c.Text, "title: design") {
			t.Errorf("front matter or comment kept: %q", c.Text)
		}
	}
	want := "INK design|INK design › Payments|INK design › Independent artists"
	if got := strings.Join(labels, "|"); got != want {
		t.Fatalf("sections = %q, want %q", got, want)
	}
	if !strings.Contains(chunks[1].Text, "# not a heading inside a fence") {
		t.Error("a # line inside a code fence was treated as a heading")
	}
	long := strings.Repeat("word ", 1000)
	if parts := splitLong(long); len(parts) < 3 {
		t.Errorf("a 5000-character section split into %d parts", len(parts))
	}
}

func TestScanPlanAndAbsorb(t *testing.T) {
	root, ink := makeWorkspace(t)
	ctx := context.Background()
	st := newStore(t)

	repos, err := FindRepos(root, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 || filepath.Base(repos[0]) != "INK" || filepath.Base(repos[1]) != "EasyScript" {
		t.Fatalf("repos = %v (node_modules must be skipped)", repos)
	}
	plans := PlanRepos(ctx, st, repos, AbsorbOptions{})
	p := plans[0]
	if p.Namespace != "ink" || p.Commits != 4 || p.Absorbed {
		t.Fatalf("plan = %+v", p)
	}
	if strings.Join(p.Rules, ",") != "CLAUDE.md" || strings.Join(p.Docs, ",") != "README.md,docs/design.md,docs/keys.md" {
		t.Fatalf("rules %v docs %v (CHANGELOG must be skipped, README first)", p.Rules, p.Docs)
	}
	for _, want := range []string{"TypeScript", "Next.js", "React", "Stripe"} {
		if !strings.Contains(strings.Join(p.Stack, ","), want) {
			t.Errorf("stack %v is missing %s", p.Stack, want)
		}
	}
	if plans[1].Stack[0] != "C" {
		t.Errorf("EasyScript stack = %v", plans[1].Stack)
	}

	var stages []string
	res, err := Absorb(ctx, st, p, AbsorbOptions{Progress: func(stage string, done, total int) {
		if len(stages) == 0 || stages[len(stages)-1] != stage {
			stages = append(stages, stage)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Git.Commits != 4 || res.Rules != 1 || res.Sections != 4 || res.Secrets != 1 {
		t.Fatalf("absorb = %+v", res)
	}
	if strings.Join(stages, ",") != "history,docs" {
		t.Errorf("progress stages = %v", stages)
	}
	if res.Linked < 6 {
		t.Errorf("linked only %d edges", res.Linked)
	}
	// The payments section mentions api/payouts.go, which history made a concept.
	keys, _ := st.SystemKeys("doc:INK:docs/design.md#ink-design-payments")
	fileKeys, _ := st.SystemKeys("git-file:INK:api/payouts.go")
	if len(keys) != 1 || len(fileKeys) != 1 {
		t.Fatalf("keys %v files %v", keys, fileKeys)
	}
	for _, id := range keys {
		if mems, _ := st.Get("claude-code", []string{id}); len(mems) > 0 {
			t.Fatalf("claude-code has no grant on ink but read %v", mems)
		}
	}

	again, err := Absorb(ctx, st, p, AbsorbOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Git.Commits != 0 || again.Sections != 0 || again.Rules != 0 || again.Updated != 0 || again.Archived != 0 || again.Linked != 0 {
		t.Fatalf("second absorb changed things: %+v", again)
	}

	// Edit one section and drop another: one update, one retirement.
	edited := strings.Replace(designDoc, "flat 4.5% plus five dollars", "flat 5%", 1)
	edited = edited[:strings.Index(edited, "## Independent artists")]
	writeFile(t, ink, "docs/design.md", edited)
	p2, _ := PlanRepo(ctx, st, ink, "ink", AbsorbOptions{})
	if !p2.Absorbed {
		t.Error("plan does not know the repo was absorbed")
	}
	third, err := Absorb(ctx, st, p2, AbsorbOptions{NoGit: true})
	if err != nil {
		t.Fatal(err)
	}
	if third.Updated != 1 || third.Archived != 1 {
		t.Fatalf("after editing: %+v", third)
	}
}
