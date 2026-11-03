package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/zakeripouya/EngramContextProtocol/internal/bootstrap"
	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/hosts"
	"github.com/zakeripouya/EngramContextProtocol/internal/llm"
	"github.com/zakeripouya/EngramContextProtocol/internal/sleep"
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
	sleepHours := fs.String("sleep-hours", "01:00-07:00", "when the nightly cycle may run")
	noAgents := fs.Bool("no-agents", false, "don't connect agent hosts")
	noSchedule := fs.Bool("no-schedule", false, "don't install the nightly scheduler (cron or launchd)")
	noImport := fs.Bool("no-import", false, "don't import the current git repository")
	hooks := fs.Bool("permission-hooks", false, "install the Claude Code permission-assist hooks (shadow mode)")
	embedURL := fs.String("embed-url", "", "an OpenAI-compatible embeddings server to save in the config")
	embedModel := fs.String("embed-model", "", "embedding model name to save in the config")
	llmURL := fs.String("llm-url", "", "an OpenAI-compatible chat server for the sleep cycle")
	llmModel := fs.String("llm-model", "", "chat model name for the sleep cycle")
	if err := fs.Parse(args); err != nil {
		return err
	}
	stat, _ := os.Stdin.Stat()
	p := &prompter{yes: *yes, interactive: stat != nil && stat.Mode()&os.ModeCharDevice != 0, in: bufio.NewReader(os.Stdin)}
	exe, err := os.Executable()
	if err != nil {
		exe = "engram"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	step := func(n int, title string) { fmt.Printf("\n%d. %s\n", n, title) }

	fmt.Println("Setting up Engram. Every step can be redone later; nothing is sent anywhere.")

	// 1. Database and settings.
	step(1, "Database")
	st, err := store.Open(dbPath())
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Println("   ", dbPath())
	cfg := map[string]string{}
	if *embedURL != "" {
		cfg["ENGRAM_EMBED_URL"] = *embedURL
	}
	if *embedModel != "" {
		cfg["ENGRAM_EMBED_MODEL"] = *embedModel
	}
	if *llmURL != "" {
		cfg["ENGRAM_LLM_URL"] = *llmURL
	}
	if *llmModel != "" {
		cfg["ENGRAM_LLM_MODEL"] = *llmModel
	}
	if len(cfg) > 0 {
		if err := saveConfig(cfg); err != nil {
			return err
		}
		for k, v := range cfg {
			_ = os.Setenv(k, v)
		}
		fmt.Println("    settings saved to", configPath())
	}

	// 2. Models.
	step(2, "Models")
	emb, note := embed.New(context.Background(), embed.FromEnv())
	fmt.Println("    embeddings:", note)
	if _, hashing := emb.(*embed.Hashing); hashing {
		fmt.Println("    For recall by meaning, run an embedding server, for example:")
		fmt.Println("      ollama pull nomic-embed-text   (https://ollama.com), then: engram reembed")
	}
	st.SetEmbedder(emb)
	if chat, note := llm.New(context.Background(), llm.FromEnv()); chat != nil {
		fmt.Println("    sleep-cycle model:", note)
	} else {
		fmt.Println("    sleep-cycle model:", note, "(distillation is skipped until one answers; for example: ollama pull llama3.2)")
	}

	// 3. Agents.
	step(3, "Agents")
	home, _ := os.UserHomeDir()
	var connected []string
	if *noAgents {
		fmt.Println("    skipped (--no-agents)")
	} else {
		found := 0
		for _, h := range hosts.Detect(home) {
			if !h.Found {
				continue
			}
			found++
			if !p.ask(fmt.Sprintf("    Connect %s and let it read and write your personal memory?", h.Title), true) {
				continue
			}
			msg, err := hosts.Connect(h, exe)
			if err != nil {
				fmt.Printf("    %s: %v\n", h.Title, err)
				continue
			}
			if err := st.Pair(h.Name, []string{"me"}, []string{"me"}, ""); err != nil {
				return err
			}
			connected = append(connected, h.Name)
			fmt.Printf("    %s; paired %s with read and write on me\n", msg, h.Name)
		}
		if found == 0 {
			fmt.Println("    No agent hosts found. Later: engram setup claude-code | claude-desktop | codex | cursor")
		}
	}

	// 4. Sleep hours and scheduler.
	step(4, "Sleep cycle")
	window := p.text("    Sleep hours (local time) when Engram may reorganize itself", *sleepHours)
	h, err := sleep.ParseHours(window, "")
	if err != nil {
		return err
	}
	if err := sleep.SaveHours(st, h); err != nil {
		return err
	}
	fmt.Println("    sleep hours:", h.String())
	if *noSchedule {
		fmt.Println("    scheduler skipped (--no-schedule); see: engram sleep schedule")
	} else if p.ask("    Install a scheduler that runs the cycle when it is due?", true) {
		msg, err := installScheduler(exe)
		if err != nil {
			fmt.Println("    could not install it:", err, "(see: engram sleep schedule)")
		} else {
			fmt.Println("   ", msg)
		}
	}

	// 5. Starting knowledge.
	step(5, "Starting knowledge")
	if _, err := bootstrap.LoadPack("developer"); err == nil && p.ask("    Install the Developer pack (branches, PRs, tickets, CI and how they connect)?", true) {
		pk, _ := bootstrap.LoadPack("developer")
		res, err := bootstrap.InstallPack(st, pk)
		if err != nil {
			return err
		}
		fmt.Printf("    installed %d memories and %d pathways into %s\n", res.Memories, res.Edges, res.Namespace)
		for _, c := range connected {
			if err := st.Pair(c, []string{res.Namespace}, nil, ""); err != nil {
				return err
			}
		}
	}
	if cwd, err := os.Getwd(); err == nil && !*noImport {
		if _, err := os.Stat(filepath.Join(cwd, ".git")); err == nil &&
			p.ask(fmt.Sprintf("    Import the git history of %s into your memory?", filepath.Base(cwd)), true) {
			res, err := bootstrap.ImportGit(context.Background(), st, bootstrap.GitOptions{Repo: cwd, Namespace: "me", MaxCommits: 2000})
			if err != nil {
				fmt.Println("    import failed:", err)
			} else {
				fmt.Printf("    imported %d commits, %d files and %d conventions\n", res.Commits, res.Files, len(res.Conventions))
			}
		}
	}

	// 6. Permission assist (opt-in).
	step(6, "Permission assist (shadow mode)")
	if slicesContains(connected, "claude-code") && (*hooks || p.ask("    Let Engram watch Claude Code permission prompts and predict your answers? It never answers them.", false)) {
		changed, err := hosts.AddClaudeHooks(filepath.Join(home, ".claude", "settings.json"), exe)
		switch {
		case err != nil:
			fmt.Println("    could not add the hooks:", err)
		case changed:
			fmt.Println("    hooks added to ~/.claude/settings.json; see engram permit report in a few weeks")
		default:
			fmt.Println("    the hooks were already installed")
		}
	} else {
		fmt.Println("    off (turn on later with engram init --permission-hooks)")
	}

	fmt.Println("\nDone. Next:")
	if len(connected) > 0 {
		fmt.Println("  - Restart your agents, then tell one something durable, such as \"remember that branches start with the ticket number\".")
	}
	fmt.Println("  - engram dashboard        see your memory, the inbox and health")
	fmt.Println("  - engram serve --http …   share it with tools over HTTP (engram token create NAME)")
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
