package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/bootstrap"
	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/hosts"
	"github.com/zakeripouya/EngramContextProtocol/internal/llm"
	"github.com/zakeripouya/EngramContextProtocol/internal/sleep"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// The setup core: what engram init decides (setupChoices) and how it applies
// them (applySetup). The plain prompts and the interactive screen both fill in
// the same choices and watch the same events, so they can't drift apart.

type modelInfo struct {
	Embeddings     string `json:"embeddings"`
	Hashing        bool   `json:"hashingFallback"` // no embedding server answered; recall by meaning is weak
	Chat           string `json:"chat"`
	ChatOK         bool   `json:"chatAvailable"`
	embedder       embed.Embedder
	detectDuration time.Duration
}

func detectModels(ctx context.Context) modelInfo {
	start := time.Now()
	emb, note := embed.New(ctx, embed.FromEnv())
	_, hashing := emb.(*embed.Hashing)
	chat, chatNote := llm.New(ctx, llm.FromEnv())
	return modelInfo{Embeddings: note, Hashing: hashing, Chat: chatNote, ChatOK: chat != nil, embedder: emb,
		detectDuration: time.Since(start)}
}

type setupChoices struct {
	Hosts         []hosts.Host         `json:"-"`
	SleepHours    string               `json:"sleepHours"`
	Scheduler     bool                 `json:"scheduler"`
	DeveloperPack bool                 `json:"developerPack"`
	Repos         []bootstrap.RepoPlan `json:"repos"`
	Hooks         bool                 `json:"permissionHooks"`
	Absorb        bootstrap.AbsorbOptions
}

// setupEvent is one line of progress. Kind is "step" (a new section starts),
// "progress" (Done of Total within Item), "ok", "warn" or "fail".
type setupEvent struct {
	Kind, Step, Item, Msg string
	Done, Total           int
}

type setupSummary struct {
	Connected  []string
	Namespaces []string
	Memories   int
	Pathways   int
	Commits    int
	Sections   int
	Rules      int
	Secrets    int
	Warnings   int
	Elapsed    time.Duration
}

// applySetup carries out the choices in order, reporting through emit. A failing
// agent, scheduler or repository is reported and skipped; only store errors stop it.
func applySetup(ctx context.Context, st *store.Store, exe, home string, ch setupChoices, emit func(setupEvent)) (setupSummary, error) {
	start := time.Now()
	var sum setupSummary
	before, _ := st.Stats()
	warn := func(step, item, msg string) {
		sum.Warnings++
		emit(setupEvent{Kind: "warn", Step: step, Item: item, Msg: msg})
	}

	emit(setupEvent{Kind: "step", Step: "Agents"})
	if len(ch.Hosts) == 0 {
		emit(setupEvent{Kind: "ok", Step: "Agents", Msg: "none selected (later: engram setup claude-code | claude-desktop | codex | cursor)"})
	}
	for _, h := range ch.Hosts {
		msg, err := hosts.Connect(h, exe)
		if err != nil {
			warn("Agents", h.Title, err.Error())
			continue
		}
		if err := st.Pair(h.Name, []string{"me"}, []string{"me"}, ""); err != nil {
			return sum, err
		}
		sum.Connected = append(sum.Connected, h.Name)
		emit(setupEvent{Kind: "ok", Step: "Agents", Item: h.Title, Msg: msg + "; can read and write your personal memory (me)"})
	}

	emit(setupEvent{Kind: "step", Step: "Sleep cycle"})
	hours, err := sleep.ParseHours(ch.SleepHours, "")
	if err != nil {
		return sum, err
	}
	if err := sleep.SaveHours(st, hours); err != nil {
		return sum, err
	}
	emit(setupEvent{Kind: "ok", Step: "Sleep cycle", Item: "hours", Msg: hours.String()})
	if ch.Scheduler {
		if msg, err := installScheduler(exe); err != nil {
			warn("Sleep cycle", "scheduler", err.Error()+" (see: engram sleep schedule)")
		} else {
			emit(setupEvent{Kind: "ok", Step: "Sleep cycle", Item: "scheduler", Msg: msg})
		}
	}

	if ch.DeveloperPack {
		emit(setupEvent{Kind: "step", Step: "Starter pack"})
		if pk, err := bootstrap.LoadPack("developer"); err != nil {
			warn("Starter pack", "developer", err.Error())
		} else {
			res, err := bootstrap.InstallPack(st, pk)
			if err != nil {
				return sum, err
			}
			for _, c := range sum.Connected {
				if err := st.Pair(c, []string{res.Namespace}, nil, ""); err != nil {
					return sum, err
				}
			}
			emit(setupEvent{Kind: "ok", Step: "Starter pack", Item: "developer",
				Msg: fmt.Sprintf("%d memories, %d pathways in %s", res.Memories, res.Edges, res.Namespace)})
		}
	}

	if len(ch.Repos) > 0 {
		emit(setupEvent{Kind: "step", Step: "Projects"})
	}
	for _, plan := range ch.Repos {
		o := ch.Absorb
		item := plan.Name
		o.Progress = func(stage string, done, total int) {
			emit(setupEvent{Kind: "progress", Step: "Projects", Item: item, Msg: stage, Done: done, Total: total})
		}
		res, err := bootstrap.Absorb(ctx, st, plan, o)
		if err != nil {
			warn("Projects", item, err.Error())
			continue
		}
		for _, c := range sum.Connected {
			if err := st.Pair(c, []string{plan.Namespace}, []string{plan.Namespace}, ""); err != nil {
				return sum, err
			}
		}
		sum.Namespaces = append(sum.Namespaces, plan.Namespace)
		sum.Commits += res.Git.Commits
		sum.Sections += res.Sections
		sum.Rules += res.Rules
		sum.Secrets += res.Secrets
		emit(setupEvent{Kind: "ok", Step: "Projects", Item: item, Msg: describeAbsorb(plan, res)})
	}

	emit(setupEvent{Kind: "step", Step: "Permission assist"})
	switch {
	case !ch.Hooks:
		emit(setupEvent{Kind: "ok", Step: "Permission assist", Msg: "off (later: engram init --permission-hooks)"})
	case !slicesContains(sum.Connected, "claude-code"):
		warn("Permission assist", "", "needs Claude Code connected; skipped")
	default:
		changed, err := hosts.AddClaudeHooks(filepath.Join(home, ".claude", "settings.json"), exe)
		switch {
		case err != nil:
			warn("Permission assist", "", err.Error())
		case changed:
			emit(setupEvent{Kind: "ok", Step: "Permission assist", Msg: "watching Claude Code prompts in shadow mode; see engram permit report in a few weeks"})
		default:
			emit(setupEvent{Kind: "ok", Step: "Permission assist", Msg: "already installed"})
		}
	}
	sum.Elapsed = time.Since(start)
	after, _ := st.Stats()
	sum.Memories, sum.Pathways = after.Memories-before.Memories, after.Edges-before.Edges
	return sum, nil
}

func describeAbsorb(plan bootstrap.RepoPlan, r bootstrap.AbsorbResult) string {
	g := r.Git
	if g.Commits+g.Files+len(g.Conventions)+r.Rules+r.Sections+r.Updated+r.Archived == 0 {
		return "up to date (nothing new since the last absorb) → " + plan.Namespace
	}
	var parts []string
	if g.Commits > 0 {
		parts = append(parts, plural(g.Commits, "commit", "commits"))
	}
	if g.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d already known", g.Skipped))
	}
	if r.Rules > 0 {
		parts = append(parts, plural(r.Rules, "rule", "rules"))
	}
	if r.Sections > 0 {
		parts = append(parts, plural(r.Sections, "doc section", "doc sections"))
	}
	if r.Updated > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", r.Updated))
	}
	if r.Archived > 0 {
		parts = append(parts, fmt.Sprintf("%d retired", r.Archived))
	}
	if n := len(g.Conventions); n > 0 {
		parts = append(parts, plural(n, "convention", "conventions"))
	}
	if r.Secrets > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (looked like secrets)", r.Secrets))
	}
	return strings.Join(parts, ", ") + " → " + plan.Namespace
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// defaultScanRoot guesses where the user keeps projects: the folder around the
// current repository, else a usual projects folder, else home.
func defaultScanRoot() string {
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return filepath.Dir(top)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	for _, d := range []string{"code", "Code", "src", "dev", "projects", "Projects", "repos", "workspace", "GitHub", "Documents/GitHub", "go/src"} {
		if info, err := os.Stat(filepath.Join(home, d)); err == nil && info.IsDir() {
			return filepath.Join(home, d)
		}
	}
	return home
}

// recentlyActive reports whether a plan's last commit is within a year, which
// decides whether setup selects it by default.
func recentlyActive(p bootstrap.RepoPlan) bool {
	t, err := time.Parse("2006-01-02", p.LastCommit)
	return err == nil && time.Since(t) < 365*24*time.Hour
}

func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}
