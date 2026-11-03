package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/zakeripouya/EngramContextProtocol/internal/bootstrap"
	"github.com/zakeripouya/EngramContextProtocol/internal/hosts"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// configPath is the optional settings file next to the database: KEY=VALUE lines
// of ENGRAM_* variables, applied when the variable isn't already set.
func configPath() string { return filepath.Join(filepath.Dir(dbPath()), "config") }

func loadConfig() {
	f, err := os.Open(configPath())
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`)
		if !ok || !strings.HasPrefix(k, "ENGRAM_") || k == "ENGRAM_DB" {
			continue
		}
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
}

func saveConfig(values map[string]string) error {
	existing := map[string]string{}
	if b, err := os.ReadFile(configPath()); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(k, "#") {
				existing[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	for k, v := range values {
		existing[k] = v
	}
	keys := make([]string, 0, len(existing))
	for k := range existing {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Engram settings (written by engram init). Environment variables override these.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, existing[k])
	}
	return os.WriteFile(configPath(), []byte(b.String()), 0o600)
}

type prompter struct {
	yes         bool
	interactive bool
	in          *bufio.Reader
}

func (p *prompter) ask(question string, def bool) bool {
	if p.yes || !p.interactive {
		return def
	}
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	fmt.Printf("%s [%s] ", question, hint)
	line, _ := p.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def
	case "y", "yes":
		return true
	}
	return false
}

func (p *prompter) text(question, def string) string {
	if p.yes || !p.interactive {
		return def
	}
	fmt.Printf("%s [%s] ", question, def)
	line, _ := p.in.ReadString('\n')
	if s := strings.TrimSpace(line); s != "" {
		return s
	}
	return def
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "accept the recommended choice at every step, without prompts")
	plain := fs.Bool("plain", false, "use plain yes/no prompts instead of the full-screen setup")
	scan := fs.String("scan", "", "find projects under this folder and offer to absorb them (default: around the current repo)")
	dryRun := fs.Bool("dry-run", false, "show what setup would do (agents, models, projects) and change nothing")
	asJSON := fs.Bool("json", false, "with --dry-run: print the plan as JSON, for agents driving setup")
	since := fs.String("since", "", "only absorb history since, e.g. \"1 year ago\"")
	maxCommits := fs.Int("max-commits", 2000, "newest commits to absorb per project")
	sleepHours := fs.String("sleep-hours", "01:00-07:00", "when the nightly cycle may run")
	noAgents := fs.Bool("no-agents", false, "don't connect agent hosts")
	noSchedule := fs.Bool("no-schedule", false, "don't install the nightly scheduler (cron or launchd)")
	noImport := fs.Bool("no-import", false, "don't absorb any projects")
	hooks := fs.Bool("permission-hooks", false, "install the Claude Code permission-assist hooks (shadow mode)")
	embedURL := fs.String("embed-url", "", "an OpenAI-compatible embeddings server to save in the config")
	embedModel := fs.String("embed-model", "", "embedding model name to save in the config")
	llmURL := fs.String("llm-url", "", "an OpenAI-compatible chat server for the sleep cycle")
	llmModel := fs.String("llm-model", "", "chat model name for the sleep cycle")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	absorbOpts := bootstrap.AbsorbOptions{Since: *since, MaxCommits: *maxCommits}
	home, _ := os.UserHomeDir()
	exe, err := os.Executable()
	if err != nil {
		exe = "engram"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	scanRoot := *scan
	if scanRoot == "" {
		scanRoot = defaultScanRoot()
	}

	if *dryRun {
		return initDryRun(ctx, home, scanRoot, *scan != "" || !*noImport, *asJSON, absorbOpts)
	}

	cfg := map[string]string{}
	for k, v := range map[string]string{"ENGRAM_EMBED_URL": *embedURL, "ENGRAM_EMBED_MODEL": *embedModel,
		"ENGRAM_LLM_URL": *llmURL, "ENGRAM_LLM_MODEL": *llmModel} {
		if v != "" {
			cfg[k] = v
		}
	}
	if err := os.MkdirAll(filepath.Dir(dbPath()), 0o700); err != nil {
		return err
	}
	if len(cfg) > 0 {
		if err := saveConfig(cfg); err != nil {
			return err
		}
		for k, v := range cfg {
			_ = os.Setenv(k, v)
		}
	}
	st, err := store.Open(dbPath())
	if err != nil {
		return err
	}
	defer st.Close()

	if !*yes && !*plain && isTerminal(os.Stdin) && isTerminal(os.Stdout) && !*noAgents && !*noImport {
		_, err := runTUI(ctx, st, exe, home, scanRoot, *sleepHours, *hooks, absorbOpts)
		return err
	}
	return plainInit(ctx, st, exe, home, plainOptions{yes: *yes, scan: *scan, noAgents: *noAgents, noSchedule: *noSchedule,
		noImport: *noImport, hooks: *hooks, sleepHours: *sleepHours, configSaved: len(cfg) > 0, absorb: absorbOpts})
}

func isTerminal(f *os.File) bool {
	stat, err := f.Stat()
	return err == nil && stat.Mode()&os.ModeCharDevice != 0
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// initDryRun prints what setup would do without creating or changing anything.
func initDryRun(ctx context.Context, home, scanRoot string, withProjects, asJSON bool, o bootstrap.AbsorbOptions) error {
	var st *store.Store
	if _, err := os.Stat(dbPath()); err == nil {
		if s, err := store.Open(dbPath()); err == nil {
			st = s
			defer st.Close()
		}
	}
	models := detectModels(ctx)
	type hostOut struct {
		Name  string `json:"name"`
		Title string `json:"title"`
		Found bool   `json:"installed"`
	}
	out := struct {
		Database string               `json:"database"`
		Exists   bool                 `json:"databaseExists"`
		Models   modelInfo            `json:"models"`
		Hosts    []hostOut            `json:"agents"`
		ScanRoot string               `json:"scanRoot"`
		Repos    []bootstrap.RepoPlan `json:"projects"`
		Selected []string             `json:"preselected"`
		Next     string               `json:"next"`
	}{Database: dbPath(), Exists: st != nil, Models: models, ScanRoot: scanRoot, Repos: []bootstrap.RepoPlan{}, Selected: []string{}}
	for _, h := range hosts.Detect(home) {
		out.Hosts = append(out.Hosts, hostOut{h.Name, h.Title, h.Found})
	}
	if withProjects {
		repos, err := bootstrap.FindRepos(scanRoot, 4)
		if err != nil {
			return err
		}
		out.Repos = bootstrap.PlanRepos(ctx, st, repos, o)
		for _, p := range out.Repos {
			if recentlyActive(p) {
				out.Selected = append(out.Selected, p.Path)
			}
		}
	}
	out.Next = "engram init --yes --no-import, then engram absorb PATH [--ns NAME] for each project to keep"
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Println("Dry run: nothing is created or changed.")
	fmt.Printf("\nDatabase     %s", tildePath(out.Database))
	if !out.Exists {
		fmt.Print(" (would be created)")
	}
	fmt.Printf("\nEmbeddings   %s\nSleep model  %s\n\nAgents\n", models.Embeddings, models.Chat)
	for _, h := range out.Hosts {
		state := "not installed"
		if h.Found {
			state = "would connect"
		}
		fmt.Printf("  %-15s %s\n", h.Title, state)
	}
	if withProjects {
		fmt.Printf("\nProjects under %s\n", tildePath(scanRoot))
		if len(out.Repos) == 0 {
			fmt.Println("  none found")
		}
		for _, p := range out.Repos {
			mark := " "
			if recentlyActive(p) {
				mark = "*"
			}
			fmt.Printf(" %s %-24s → %-24s %5d commits  %3d docs  %s\n", mark, truncate(p.Name, 24), truncate(p.Namespace, 24),
				p.Commits, len(p.Rules)+len(p.Docs), strings.Join(p.Stack, ", "))
		}
		fmt.Println("  (* = active in the last year, selected by default)")
	}
	return nil
}

type plainOptions struct {
	yes, noAgents, noSchedule, noImport, hooks, configSaved bool
	scan, sleepHours                                        string
	absorb                                                  bootstrap.AbsorbOptions
}

// plainInit is setup as numbered yes/no questions, for scripts, --yes, --plain
// and terminals the full-screen setup can't use.
func plainInit(ctx context.Context, st *store.Store, exe, home string, o plainOptions) error {
	p := &prompter{yes: o.yes, interactive: isTerminal(os.Stdin), in: bufio.NewReader(os.Stdin)}
	fmt.Println("Setting up Engram. Every step can be redone later; nothing is sent anywhere.")
	fmt.Println("\nDatabase\n   ", dbPath())
	if o.configSaved {
		fmt.Println("    settings saved to", configPath())
	}
	fmt.Println("\nModels")
	models := detectModels(ctx)
	fmt.Println("    embeddings:", models.Embeddings)
	if models.Hashing {
		fmt.Println("    For recall by meaning, run an embedding server, for example:")
		fmt.Println("      ollama pull nomic-embed-text   (https://ollama.com), then: engram reembed")
	}
	st.SetEmbedder(models.embedder)
	if models.ChatOK {
		fmt.Println("    sleep-cycle model:", models.Chat)
	} else {
		fmt.Println("    sleep-cycle model:", models.Chat, "(distillation is skipped until one answers; for example: ollama pull llama3.2)")
	}

	ch := setupChoices{SleepHours: o.sleepHours, Absorb: o.absorb}
	fmt.Println("\nChoices")
	if !o.noAgents {
		for _, h := range hosts.Detect(home) {
			if h.Found && p.ask(fmt.Sprintf("    Connect %s and let it read and write your personal memory?", h.Title), true) {
				ch.Hosts = append(ch.Hosts, h)
			}
		}
	}
	ch.SleepHours = p.text("    Sleep hours (local time) when Engram may reorganize itself", o.sleepHours)
	ch.Scheduler = !o.noSchedule && p.ask("    Install a scheduler that runs the cycle when it is due?", true)
	if _, err := bootstrap.LoadPack("developer"); err == nil {
		ch.DeveloperPack = p.ask("    Install the Developer pack (branches, PRs, tickets, CI and how they connect)?", true)
	}
	if !o.noImport {
		var plans []bootstrap.RepoPlan
		if o.scan != "" {
			repos, err := bootstrap.FindRepos(o.scan, 4)
			if err != nil {
				return err
			}
			plans = bootstrap.PlanRepos(ctx, st, repos, o.absorb)
			fmt.Printf("    found %d projects under %s\n", len(plans), tildePath(o.scan))
		} else if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
			if plan, err := bootstrap.PlanRepo(ctx, st, strings.TrimSpace(string(out)), "", o.absorb); err == nil {
				plans = append(plans, plan)
			}
		}
		for _, plan := range plans {
			q := fmt.Sprintf("    Absorb %s (%d commits, %d docs) into namespace %s?", plan.Name, plan.Commits, len(plan.Rules)+len(plan.Docs), plan.Namespace)
			if p.ask(q, o.scan == "" || recentlyActive(plan)) {
				ch.Repos = append(ch.Repos, plan)
			}
		}
	}
	claude := false
	for _, h := range ch.Hosts {
		claude = claude || h.Name == "claude-code"
	}
	ch.Hooks = claude && (o.hooks || p.ask("    Let Engram watch Claude Code permission prompts and predict your answers? It never answers them.", false))

	progressShown := false
	tty := isTerminal(os.Stdout)
	sum, err := applySetup(ctx, st, exe, home, ch, func(e setupEvent) {
		if e.Kind == "progress" {
			if tty {
				fmt.Printf("\r\033[K    %s: %s %d/%d", e.Item, e.Msg, e.Done, e.Total)
				progressShown = true
			}
			return
		}
		if progressShown {
			fmt.Print("\r\033[K")
			progressShown = false
		}
		switch e.Kind {
		case "step":
			fmt.Printf("\n%s\n", e.Step)
		case "warn", "fail":
			fmt.Printf("    ! %s %s\n", e.Item, e.Msg)
		default:
			if e.Item != "" {
				fmt.Printf("    %s: %s\n", e.Item, e.Msg)
			} else {
				fmt.Printf("    %s\n", e.Msg)
			}
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("\nDone: %d memories and %d pathways added in %.1fs. Next:\n", sum.Memories, sum.Pathways, sum.Elapsed.Seconds())
	if len(sum.Connected) > 0 {
		fmt.Println("  - Restart your agents, then ask one \"what do you know about this project?\"")
	}
	fmt.Println("  - engram dashboard        see your memory, the inbox and health")
	fmt.Println("  - engram absorb DIR       absorb more projects (only changes are added on later runs)")
	return nil
}

func slicesContains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// installScheduler runs "engram sleep run --if-due" every 15 minutes: a launchd
// agent on macOS, a crontab line elsewhere. It is idempotent.
func installScheduler(exe string) (string, error) {
	logPath := filepath.Join(filepath.Dir(dbPath()), "sleep.log")
	env := ""
	if v := os.Getenv("ENGRAM_DB"); v != "" {
		env = "ENGRAM_DB=" + v + " "
	}
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		plist := filepath.Join(home, "Library", "LaunchAgents", "dev.engram.sleep.plist")
		envBlock := ""
		if v := os.Getenv("ENGRAM_DB"); v != "" {
			envBlock = fmt.Sprintf("<key>EnvironmentVariables</key><dict><key>ENGRAM_DB</key><string>%s</string></dict>", v)
		}
		content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.engram.sleep</string>
  <key>ProgramArguments</key><array><string>%s</string><string>sleep</string><string>run</string><string>--if-due</string></array>
  <key>StartInterval</key><integer>900</integer>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  %s
</dict></plist>
`, exe, logPath, logPath, envBlock)
		if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
			return "", err
		}
		_ = exec.Command("launchctl", "unload", plist).Run()
		if out, err := exec.Command("launchctl", "load", "-w", plist).CombinedOutput(); err != nil {
			return "", fmt.Errorf("launchctl: %v %s", err, out)
		}
		return "launchd agent installed: " + plist, nil
	case "windows":
		return "", fmt.Errorf("automatic scheduling isn't supported on Windows yet; use Task Scheduler to run \"engram sleep run --if-due\" every 15 minutes")
	}
	if _, err := exec.LookPath("crontab"); err != nil {
		return "", fmt.Errorf("crontab not found")
	}
	line := fmt.Sprintf("*/15 * * * * %s%s sleep run --if-due >> %s 2>&1", env, exe, logPath)
	current, _ := exec.Command("crontab", "-l").Output() // empty when there is no crontab yet
	if strings.Contains(string(current), "sleep run --if-due") {
		return "a cron entry already exists", nil
	}
	next := strings.TrimRight(string(current), "\n")
	if next != "" {
		next += "\n"
	}
	next += "# Engram nightly sleep cycle (engram init)\n" + line + "\n"
	cmd := exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(next)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("crontab: %v %s", err, out)
	}
	return "cron entry installed (every 15 minutes; it only works inside your sleep hours)", nil
}
