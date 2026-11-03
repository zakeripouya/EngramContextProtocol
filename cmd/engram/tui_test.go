package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zakeripouya/EngramContextProtocol/internal/bootstrap"
	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

func gitRepo(t *testing.T, dir string, commits int) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=d@x", "GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=d@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Rules\n\nOne component per file, always, on every layer.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Project\n\nA project that does useful things for its users.\n"), 0o644)
	for i := 0; i < commits; i++ {
		_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(fmt.Sprintf("package main // %d", i)), 0o644)
		run("add", "-A")
		run("commit", "-q", "-m", fmt.Sprintf("change %d", i))
	}
}

func send(t *testing.T, m *tuiModel, msgs ...tea.Msg) tea.Cmd {
	t.Helper()
	var cmd tea.Cmd
	for _, msg := range msgs {
		_, cmd = m.Update(msg)
		_ = m.View() // every state must render
	}
	return cmd
}

func key(s string) tea.Msg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestSetupScreenFlow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	gitRepo(t, filepath.Join(root, "INK"), 3)
	gitRepo(t, filepath.Join(root, "EasyScript"), 2)
	t.Setenv("ENGRAM_DB", filepath.Join(t.TempDir(), "engram.db"))
	st, err := store.Open(dbPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	m := newTUI(ctx, st, "engram", t.TempDir(), root, "01:00-07:00", false, bootstrap.AbsorbOptions{})
	for i := range m.hostSel {
		m.hostSel[i] = false // never touch the real agent configs from a test
	}

	send(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})
	send(t, m, modelsMsg(modelInfo{Embeddings: "using the offline hashing embedder", Hashing: true, embedder: embed.NewHashing(embed.DefaultHashDims)}))
	send(t, m, m.scanCmd(m.root.Value())())
	if len(m.plans) != 2 || m.plans[0].Name != "EasyScript" || !m.sel[0] || !m.sel[1] {
		t.Fatalf("plans %+v selected %v", m.plans, m.sel)
	}

	send(t, m, key("enter"), key("enter"), key("enter"))
	if m.screen != scProjects {
		t.Fatalf("screen = %d, want projects", m.screen)
	}
	// Rename EasyScript's namespace, rejecting an invalid name first.
	send(t, m, key("e"))
	for range "easyscript" {
		send(t, m, key("backspace"))
	}
	send(t, m, key("Bad Name"), key("enter"))
	if m.nsErr == "" || !m.editingNS {
		t.Fatal("an invalid namespace was accepted")
	}
	for range "Bad Name" {
		send(t, m, key("backspace"))
	}
	send(t, m, key("es"), key("enter"))
	if m.plans[0].Namespace != "es" || m.editingNS {
		t.Fatalf("namespace = %q", m.plans[0].Namespace)
	}
	// Deselect INK, go back and forth, then on to extras and turn the scheduler off.
	send(t, m, key("down"), key("space"), key("esc"), key("enter"), key("enter"))
	if m.screen != scExtras || m.sel[1] {
		t.Fatalf("screen %d, INK selected %v", m.screen, m.sel[1])
	}
	send(t, m, key("down"), key("space"), key("enter"))
	if m.screen != scReview {
		t.Fatalf("screen = %d, want review", m.screen)
	}
	if v := m.View(); !strings.Contains(v, "EasyScript → ") || !strings.Contains(v, "es") || strings.Contains(v, "INK → ") {
		t.Fatalf("review shows the wrong projects:\n%s", v)
	}
	ch := m.choices()
	if ch.Scheduler || !ch.DeveloperPack || len(ch.Repos) != 1 || len(ch.Hosts) != 0 {
		t.Fatalf("choices = %+v", ch)
	}

	// Apply, delivering the events the way Bubble Tea would.
	cmd := send(t, m, key("enter"))
	for i := 0; cmd != nil && m.screen == scApply; i++ {
		if i > 10000 {
			t.Fatal("setup never finished")
		}
		cmd = send(t, m, cmd())
	}
	if m.screen != scDone || m.applyErr != nil || !m.Applied {
		t.Fatalf("screen %d err %v", m.screen, m.applyErr)
	}
	if m.summary.Memories == 0 || strings.Join(m.summary.Namespaces, ",") != "es" {
		t.Fatalf("summary = %+v", m.summary)
	}
	if v := m.View(); !strings.Contains(v, "share one memory") {
		t.Fatalf("done screen:\n%s", v)
	}
	s, _ := st.Stats()
	if strings.Join(s.Namespaces, ",") != "es,pack/developer" {
		t.Fatalf("namespaces = %v", s.Namespaces)
	}
	// A narrow terminal still renders every screen.
	send(t, m, tea.WindowSizeMsg{Width: 40, Height: 12})
	for sc := scWelcome; sc <= scDone; sc++ {
		m.screen = sc
		_ = m.View()
	}
}

func TestNeuralNetSpreads(t *testing.T) {
	n := newNeuralNet(1)
	if len(n.nodes) < 15 {
		t.Fatalf("only %d nodes parsed", len(n.nodes))
	}
	for i, adj := range n.adj {
		if len(adj) == 0 {
			t.Errorf("node %d at %v is not connected", i, n.nodes[i])
		}
	}
	n.Fire(0, 1)
	for i := 0; i < 12; i++ {
		n.Step()
	}
	lit := 0
	for _, a := range n.act {
		if a > 0.05 {
			lit++
		}
	}
	if lit < 4 {
		t.Errorf("a pulse reached only %d nodes", lit)
	}
	if v := n.View(); !strings.Contains(v, "●") {
		t.Error("nothing drawn")
	}
}

func TestAbsorbCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	gitRepo(t, filepath.Join(root, "INK"), 2)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetEmbedder(embed.NewHashing(embed.DefaultHashDims))
	if err := st.Pair("claude-code", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := runAbsorb(st, []string{root, "--dry-run", "--json"}); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.Stats(); s.Memories != 0 {
		t.Fatal("dry run stored memories")
	}
	if err := runAbsorb(st, []string{filepath.Join(root, "INK"), "--ns", "team/ink"}); err != nil {
		t.Fatal(err)
	}
	grants, _ := st.Grants("claude-code")
	ok := false
	for _, g := range grants {
		ok = ok || (g.Namespace == "team/ink" && g.CanRead && g.CanWrite)
	}
	if !ok {
		t.Fatalf("claude-code was not granted the new namespace: %+v", grants)
	}
	if err := runAbsorb(st, []string{root, "--ns", "me"}); err == nil {
		t.Fatal("absorbing into the personal namespace should be refused")
	}
}
