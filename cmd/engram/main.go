// Command engram is the Engram reference server and its management CLI.
//
//	engram serve [--client NAME]               run the MCP server over stdio
//	engram pair NAME --read NS --write NS      approve a client (the user does this, never an agent)
//	engram clients                             list paired clients and their grants
//	engram revoke NAME                         remove all of a client's grants
//	engram stats                               counts of memories, edges and clients
//	engram setup claude-code|claude-desktop|codex|cursor    print connection instructions
//
// The database lives at $ENGRAM_DB, or ~/.engram/engram.db by default.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zakeripouya/EngramContextProtocol/conformance"
	"github.com/zakeripouya/EngramContextProtocol/internal/bootstrap"
	"github.com/zakeripouya/EngramContextProtocol/internal/dashboard"
	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/ecpserver"
	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/health"
	"github.com/zakeripouya/EngramContextProtocol/internal/llm"
	"github.com/zakeripouya/EngramContextProtocol/internal/permit"
	"github.com/zakeripouya/EngramContextProtocol/internal/server"
	"github.com/zakeripouya/EngramContextProtocol/internal/sleep"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
	"github.com/zakeripouya/EngramContextProtocol/internal/trigger"
)

const usage = `engram: shared memory for AI agents (ECP %s reference server, v%s)

Usage:
  engram init [--scan DIR] [--plain|--yes] [--dry-run --json]
                                          set up Engram: models, agents, projects, sleep hours; full-screen in a terminal
  engram absorb [DIR|REPO ...] [--ns NS] [--dry-run] [--json]
                                          absorb projects: git history, agent rules (CLAUDE.md, AGENTS.md, Cursor), docs, stack
  engram serve [--client NAME]            serve over stdio (MCP, or native ECP when the client speaks it)
  engram pair NAME --read NS --write NS   approve a client; namespaces are comma-separated
  engram clients                          list paired clients and their grants
  engram revoke NAME                      remove all of a client's grants
  engram stats                            show memory counts
  engram reembed                          embed memories that have no vector yet and wire their neighbors
  engram decay                            weaken learned pathways and prune the weakest
  engram import git PATH [--ns NS] [--since "6 months ago"]
                                          import a repository's history, file coupling and conventions
  engram import jira FILE [--ns NS]       import a saved Jira search export (JSON)
  engram import json FILE [--ns NS]       import generic records [{"kind","content","tags","validFrom"}]
  engram pack list | search [TERM]        installed, built-in and registry packs
  engram pack install NAME[@V]|FILE       install a built-in, registry or local pack (signatures checked)
  engram pack update [NAME] | remove NAME update from registries (learned weights kept), or uninstall
  engram pack export NS --name N [--sign] export a namespace as an instance pack (secrets and private kinds excluded)
  engram pack keygen | sign | trust | keys | registry | publish   see engram pack help
  engram sleep hours 01:00-07:00 [--tz Zone]
                                          set your sleep hours (the cycle only runs inside them)
  engram sleep run [--light] [--if-due]   run a consolidation cycle now, or only if it is due
  engram sleep report [--json]            show the latest morning report and its undoable changes
  engram sleep undo CHANGE_ID             undo one change from a cycle
  engram sleep schedule                   print the cron or launchd line that runs the cycle
  engram snapshot list | restore FILE     list snapshots, or restore one (stop agents first)
  engram health [--json]                  check learning dynamics against healthy bounds and drift
  engram selftest                         run golden, programmatic and mutation tests on a sandbox copy
  engram golden add "QUERY" MEMORY_ID     add a query that must keep recalling a memory
  engram golden list                      list golden queries
  engram serve --http ADDR                serve native ECP (/ecp) and MCP (/mcp) over HTTP, e.g. 127.0.0.1:7079
  engram token create CLIENT [--service]  issue a bearer token for HTTP clients (shown once)
  engram token list | revoke CLIENT|PREFIX
  engram conformance --url URL --token T  check any ECP server against the spec over HTTP
  engram conformance -- COMMAND [ARGS]    ... or over stdio, starting the server with COMMAND
  engram setup HOST                       print connection instructions: claude-code, claude-desktop, codex, cursor
  engram sessions [open|done|all]         list tasks agents checkpointed
  engram session show ID                  show one task's checkpoint
  engram trigger add NAME [--kind webhook|github|job] [--ns NAMESPACE]
                                          create an event source; prints its secret once
  engram trigger list | events [NAME] | pause NAME | resume NAME | remove NAME
  engram listen [--addr 127.0.0.1:7077]   receive webhooks and job results for triggers
  engram job run NAME -- COMMAND [ARGS]   run an existing job unchanged and record its outcome
  engram job report NAME --status ok|failed [--summary TEXT]
                                          record the outcome of a job that ran elsewhere
  engram hook claude-code                 Claude Code hook (permission assist, shadow mode); reads stdin
  engram permit report [--json]           how often Engram's predictions matched your permission decisions
  engram permit explain TOOL INPUT        show the tier and prediction for one tool call
  engram permit on | off                  turn shadow logging on or off (kill switch)
  engram dashboard [--addr 127.0.0.1:7078] open the local dashboard (prints a link with an access token)
  engram report --html [--out FILE]       write the static HTML report (also written after each sleep cycle)
  engram version                          print the version

Database: $ENGRAM_DB, or ~/.engram/engram.db
Embeddings: $ENGRAM_EMBED = auto (default) | openai | hash | off
            $ENGRAM_EMBED_URL (default http://localhost:11434, Ollama), $ENGRAM_EMBED_MODEL
            (default nomic-embed-text), $ENGRAM_EMBED_KEY for hosted providers
Chat model for the sleep cycle: $ENGRAM_LLM = auto (default) | openai | off,
            $ENGRAM_LLM_URL (default http://localhost:11434), $ENGRAM_LLM_MODEL (default llama3.2), $ENGRAM_LLM_KEY
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "engram:", err)
		os.Exit(1)
	}
}

func dbPath() string {
	if p := os.Getenv("ENGRAM_DB"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "engram.db"
	}
	return filepath.Join(home, ".engram", "engram.db")
}

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			*l = append(*l, x)
		}
	}
	return nil
}

var durationRE = regexp.MustCompile(`^(\d+)([dh])$`)

func expiry(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	m := durationRE.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("--expires takes a duration like 30d or 12h")
	}
	n, _ := strconv.Atoi(m[1])
	d := time.Duration(n) * time.Hour
	if m[2] == "d" {
		d *= 24
	}
	return time.Now().UTC().Add(d).Format(time.RFC3339), nil
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Printf(usage, ecp.ProtocolVersion, server.Version)
		return nil
	}
	loadConfig()
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		return runInit(rest)
	case "version", "--version":
		fmt.Printf("engram %s (ECP %s)\n", server.Version, ecp.ProtocolVersion)
		return nil
	case "setup":
		return setup(rest)
	case "snapshot":
		return runSnapshot(rest)
	case "conformance":
		return runConformance(rest)
	}

	st, err := store.Open(dbPath())
	if err != nil {
		return err
	}
	defer st.Close()
	if cmd == "serve" || cmd == "reembed" || cmd == "import" || cmd == "absorb" || cmd == "pack" || cmd == "sleep" || cmd == "selftest" || cmd == "listen" || cmd == "job" {
		e, note := embed.New(context.Background(), embed.FromEnv())
		st.SetEmbedder(e)
		fmt.Fprintln(os.Stderr, "engram:", note) // stderr: stdout carries the MCP protocol
	}

	switch cmd {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		client := fs.String("client", "", "identity of the host that launches this server, e.g. claude-code")
		httpAddr := fs.String("http", "", "serve native ECP (/ecp) and MCP (/mcp) over HTTP on this address instead of stdio, e.g. 127.0.0.1:7079")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		native := ecpserver.New(st, ecpserver.Options{Version: server.Version, SnapshotDir: snapshotDir(),
			NewChat: func() llm.Chat { return chatFromEnv(false) }})
		go native.Run(ctx)
		if *httpAddr != "" {
			if host, _, err := net.SplitHostPort(*httpAddr); err == nil && host != "127.0.0.1" && host != "localhost" && host != "::1" {
				fmt.Fprintln(os.Stderr, "engram: warning: serving beyond this machine; put TLS in front (every request still needs a token)")
			}
			fmt.Fprintf(os.Stderr, "engram: serving ECP at http://%s/ecp and MCP at http://%s/mcp (bearer tokens: engram token create CLIENT)\n", *httpAddr, *httpAddr)
			hs := &http.Server{Addr: *httpAddr, Handler: native.HTTPHandler(), ReadHeaderTimeout: 10 * time.Second}
			go func() { <-ctx.Done(); _ = hs.Close() }()
			if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
		// stdio: the first message decides the protocol. An ECP initialize gets native
		// ECP; anything else is MCP (spec, "MCP compatibility profile").
		in := bufio.NewReaderSize(os.Stdin, 64*1024)
		first, err := in.ReadBytes('\n')
		if err != nil && len(first) == 0 {
			return nil // the host closed stdin without a message
		}
		if ecpserver.IsECPInitialize(first) {
			return native.ServeStdio(ctx, first, in, os.Stdout, *client)
		}
		srv := server.New(st, *client)
		wctx, cancel := context.WithCancel(ctx)
		watched := make(chan struct{})
		go func() { srv.Watch(wctx, 2*time.Second); close(watched) }()
		err = srv.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(io.MultiReader(bytes.NewReader(first), in)), Writer: os.Stdout})
		cancel()
		<-watched // drop this connection's subscriptions before exiting
		return err

	case "token":
		return runToken(st, rest)

	case "pair":
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return fmt.Errorf("usage: engram pair NAME --read NS --write NS [--expires 30d]")
		}
		name := rest[0]
		fs := flag.NewFlagSet("pair", flag.ContinueOnError)
		var read, write listFlag
		fs.Var(&read, "read", "namespaces the client may read")
		fs.Var(&write, "write", "namespaces the client may write")
		exp := fs.String("expires", "", "grant lifetime, e.g. 30d or 12h")
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		if len(read) == 0 && len(write) == 0 {
			return fmt.Errorf("give at least one --read or --write namespace, e.g. --read me --write me")
		}
		e, err := expiry(*exp)
		if err != nil {
			return err
		}
		if err := st.Pair(name, read, write, e); err != nil {
			return err
		}
		fmt.Printf("Paired %s: read [%s], write [%s]", name, read.String(), write.String())
		if *exp != "" {
			fmt.Printf(", expires in %s", *exp)
		}
		fmt.Println()
		return nil

	case "clients":
		grants, err := st.Grants("")
		if err != nil {
			return err
		}
		if len(grants) == 0 {
			fmt.Println("No clients are paired yet. Example: engram pair claude-code --read me --write me")
		}
		for _, g := range grants {
			var rights []string
			if g.CanRead {
				rights = append(rights, "read")
			}
			if g.CanWrite {
				rights = append(rights, "write")
			}
			exp := g.ExpiresAt
			if exp == "" {
				exp = "no expiry"
			}
			fmt.Printf("%-24s %-24s %-11s %s\n", g.Client, g.Namespace, strings.Join(rights, "+"), exp)
		}
		return nil

	case "revoke":
		if len(rest) != 1 {
			return fmt.Errorf("usage: engram revoke NAME")
		}
		n, err := st.Revoke(rest[0])
		if err != nil {
			return err
		}
		fmt.Printf("Removed %d grant(s) for %s\n", n, rest[0])
		return nil

	case "stats":
		s, err := st.Stats()
		if err != nil {
			return err
		}
		kinds, err := st.EdgeCounts()
		if err != nil {
			return err
		}
		fmt.Printf("memories    %d\ncurrent     %d\nedges       %d (similar %d, coactive %d, supersedes %d)\nclients     %d\nnamespaces  %s\n",
			s.Memories, s.Current, s.Edges, kinds["similar"], kinds["coactive"], kinds["supersedes"],
			s.Clients, strings.Join(s.Namespaces, ", "))
		return nil

	case "reembed":
		r, err := st.Reembed(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("Embedded %d memories with %s and linked %d neighbors\n", r.Embedded, r.Model, r.Linked)
		return nil

	case "import":
		return runImport(st, rest)

	case "absorb":
		return runAbsorb(st, rest)

	case "pack":
		return runPack(st, rest)

	case "sleep":
		return runSleep(st, rest)

	case "sessions":
		status := "open"
		if len(rest) > 0 {
			status = rest[0]
		}
		if status == "all" {
			status = ""
		}
		list, err := st.AllSessions(status)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("No sessions.")
		}
		for _, c := range list {
			who := c.LastClient
			if c.DelegatedTo != "" {
				who += " → " + c.DelegatedTo
			}
			fmt.Printf("%s  %-4s  %-10s %-20s %s\n", c.SessionID, c.Status, c.Namespace, who, store.Short(c.Goal))
		}
		return nil

	case "session":
		if len(rest) != 2 || rest[0] != "show" {
			return fmt.Errorf("usage: engram session show ID")
		}
		c, err := st.Session(rest[1])
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(c, "", "  ")
		fmt.Println(string(b))
		return nil

	case "trigger":
		return runTrigger(st, rest)

	case "listen":
		fs := flag.NewFlagSet("listen", flag.ContinueOnError)
		addr := fs.String("addr", "127.0.0.1:7077", "address to listen on; keep it on loopback unless a reverse proxy adds TLS")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if host, _, err := net.SplitHostPort(*addr); err == nil && host != "127.0.0.1" && host != "localhost" && host != "::1" {
			fmt.Fprintln(os.Stderr, "engram: warning: listening beyond this machine; put TLS in front (every request is still authenticated)")
		}
		fmt.Fprintf(os.Stderr, "engram: listening on http://%s (POST /hooks/NAME, POST /jobs/NAME)\n", *addr)
		hs := &http.Server{Addr: *addr, Handler: trigger.Handler(st), ReadHeaderTimeout: 10 * time.Second}
		return hs.ListenAndServe()

	case "job":
		return runJob(st, rest)

	case "hook":
		// Shadow mode: never print a decision, never fail the host. Claude Code asks the
		// user exactly as it would without Engram.
		if len(rest) != 1 {
			return nil
		}
		if _, err := permit.HandleHook(st, rest[0], os.Stdin); err != nil && os.Getenv("ENGRAM_DEBUG") != "" {
			fmt.Fprintln(os.Stderr, "engram hook:", err)
		}
		return nil

	case "permit":
		return runPermit(st, rest)

	case "dashboard":
		fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
		addr := fs.String("addr", "127.0.0.1:7078", "loopback address to serve on")
		container := fs.Bool("container", false, "inside a container: bind any interface (still answers only requests addressed to localhost, so publish the port on 127.0.0.1)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		host, _, err := net.SplitHostPort(*addr)
		if err != nil {
			return err
		}
		if !*container && host != "127.0.0.1" && host != "localhost" && host != "::1" {
			return fmt.Errorf("the dashboard only serves on loopback (127.0.0.1, localhost or ::1); in a container use --container")
		}
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			return err
		}
		token := dashboard.NewToken()
		shown := ln.Addr().String()
		if *container {
			_, port, _ := net.SplitHostPort(shown)
			shown = "localhost:" + port // the published port on the host
		}
		fmt.Printf("Engram dashboard: http://%s/#token=%s\n", shown, token)
		fmt.Println("The link carries a one-time token; keep it private. Press Ctrl+C to stop.")
		hs := &http.Server{Handler: dashboard.Handler(st, token), ReadHeaderTimeout: 10 * time.Second}
		return hs.Serve(ln)

	case "report":
		fs := flag.NewFlagSet("report", flag.ContinueOnError)
		_ = fs.Bool("html", true, "write HTML (the only format)")
		out := fs.String("out", "", "file to write; default report.html next to the database")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		path, err := writeHTMLReport(st, *out)
		if err != nil {
			return err
		}
		fmt.Println("Wrote", path)
		return nil

	case "health":
		asJSON := len(rest) > 0 && rest[0] == "--json"
		r, err := health.Check(st, lastDecisionRecall(st), false)
		if err != nil {
			return err
		}
		if asJSON {
			b, _ := json.MarshalIndent(r, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		printHealth(r)
		return nil

	case "selftest":
		results, err := health.SelfTest(context.Background(), st)
		if err != nil {
			return err
		}
		failed := 0
		for _, r := range results {
			mark := "pass"
			if !r.Passed {
				mark, failed = "FAIL", failed+1
			}
			fmt.Printf("  %-4s  %-12s %s", mark, r.Kind, r.Name)
			if r.Detail != "" {
				fmt.Printf("  (%s)", r.Detail)
			}
			fmt.Println()
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d self-tests failed", failed, len(results))
		}
		fmt.Printf("All %d self-tests passed (on a sandbox copy; your memory was not changed)\n", len(results))
		return nil

	case "golden":
		if len(rest) >= 3 && rest[0] == "add" {
			id, err := st.AddGolden(rest[1], rest[2])
			if err != nil {
				return err
			}
			fmt.Printf("Added golden query %d\n", id)
			return nil
		}
		if len(rest) == 1 && rest[0] == "list" {
			gs, err := st.GoldenSuite()
			if err != nil {
				return err
			}
			for _, g := range gs {
				fmt.Printf("%4d  %-40q → %s\n", g.ID, g.Query, g.ExpectID)
			}
			return nil
		}
		return fmt.Errorf(`usage: engram golden add "QUERY" MEMORY_ID | engram golden list`)

	case "decay":
		r, err := st.Decay()
		if err != nil {
			return err
		}
		fmt.Printf("Weakened %d learned pathways, pruned %d\n", r.Decayed, r.Pruned)
		return nil
	}
	return fmt.Errorf("unknown command %q; run engram help", cmd)
}

func setup(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: engram setup claude-code|claude-desktop|codex|cursor")
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "engram"
	}
	switch args[0] {
	case "claude-code":
		fmt.Println("1. Approve Claude Code (you do this, not the agent):")
		fmt.Println("     engram pair claude-code --read me --write me")
		fmt.Println("2. Connect it:")
		fmt.Printf("     claude mcp add engram -- %s serve --client claude-code\n", exe)
		fmt.Println("3. Optional: permission assist in shadow mode. Engram only watches and predicts; you still")
		fmt.Println("   answer every prompt. Add to ~/.claude/settings.json, then run engram permit report after a while:")
		hookCmd := exe + " hook claude-code"
		entry := []map[string]any{{"matcher": "*", "hooks": []map[string]any{{"type": "command", "command": hookCmd, "timeout": 5}}}}
		b, _ := json.MarshalIndent(map[string]any{"hooks": map[string]any{
			"PermissionRequest": entry, "PostToolUse": entry, "PostToolUseFailure": entry}}, "     ", "  ")
		fmt.Println("     " + string(b))
	case "claude-desktop":
		fmt.Println(`Add this to claude_desktop_config.json under "mcpServers":`)
		fmt.Printf(`  "engram": {"command": %q, "args": ["serve", "--client", "claude-desktop"]}`+"\n", exe)
		fmt.Println("Then approve it:  engram pair claude-desktop --read me --write me")
	case "codex":
		fmt.Println("1. Approve Codex (you do this, not the agent):")
		fmt.Println("     engram pair codex --read me --write me")
		fmt.Println("2. Add this to ~/.codex/config.toml:")
		fmt.Println("     [mcp_servers.engram]")
		fmt.Printf("     command = %q\n", exe)
		fmt.Println(`     args = ["serve", "--client", "codex"]`)
	case "cursor":
		fmt.Println(`Add this to ~/.cursor/mcp.json (or .cursor/mcp.json in a project) under "mcpServers":`)
		fmt.Printf(`  "engram": {"command": %q, "args": ["serve", "--client", "cursor"]}`+"\n", exe)
		fmt.Println("Then approve it:  engram pair cursor --read me --write me")
	default:
		return fmt.Errorf("unknown host %q; supported: claude-code, claude-desktop, codex, cursor", args[0])
	}
	return nil
}

func runImport(st *store.Store, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: engram import git|jira|json PATH [--ns NAMESPACE]")
	}
	kind, path := args[0], args[1]
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	ns := fs.String("ns", "me", "namespace to import into, e.g. team/ink")
	since := fs.String("since", "", `git only: limit history, e.g. "6 months ago"`)
	max := fs.Int("max", 2000, "git only: maximum commits, newest first")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	switch kind {
	case "git":
		res, err := bootstrap.ImportGit(context.Background(), st, bootstrap.GitOptions{
			Repo: path, Namespace: *ns, Since: *since, MaxCommits: *max,
		})
		if err != nil {
			return err
		}
		fmt.Printf("Imported %d commits (%d already known), %d files, %d co-change pathways into %s\n",
			res.Commits, res.Skipped, res.Files, res.CoChange, *ns)
		for _, c := range res.Conventions {
			fmt.Println("  convention:", c)
		}
		return nil
	case "jira", "json":
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		var res bootstrap.ImportResult
		if kind == "jira" {
			res, err = bootstrap.ImportJira(st, f, *ns)
		} else {
			res, err = bootstrap.ImportRecords(st, f, *ns, "import")
		}
		if err != nil {
			return err
		}
		fmt.Printf("Imported %d records (%d already known) into %s\n", res.Imported, res.Skipped, *ns)
		return nil
	}
	return fmt.Errorf("unknown import source %q; use git, jira or json", kind)
}

func snapshotDir() string { return filepath.Join(filepath.Dir(dbPath()), "snapshots") }

func runSleep(st *store.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: engram sleep hours|run|report|undo|schedule")
	}
	switch args[0] {
	case "hours":
		fs := flag.NewFlagSet("hours", flag.ContinueOnError)
		tz := fs.String("tz", "Local", "IANA time zone, e.g. America/New_York")
		if len(args) < 2 {
			if h, ok, err := sleep.LoadHours(st); err != nil {
				return err
			} else if ok {
				fmt.Println("Sleep hours:", h)
				return nil
			}
			return fmt.Errorf("usage: engram sleep hours 01:00-07:00 [--tz America/New_York]")
		}
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		h, err := sleep.ParseHours(args[1], *tz)
		if err != nil {
			return err
		}
		if err := sleep.SaveHours(st, h); err != nil {
			return err
		}
		fmt.Println("Sleep hours set to", h)
		return nil

	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		light := fs.Bool("light", false, "decay, downscale and dedupe only")
		ifDue := fs.Bool("if-due", false, "run only inside sleep hours, when agents are idle, once a night")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *ifDue {
			ok, why, err := sleep.Due(st, time.Now())
			if err != nil {
				return err
			}
			if !ok {
				fmt.Println("Not running:", why)
				return nil
			}
		}
		var chat llm.Chat
		if !*light {
			chat = chatFromEnv(true)
		}
		rep, err := sleep.Run(context.Background(), st, sleep.Options{Light: *light, SnapshotDir: snapshotDir(), Chat: chat})
		if err != nil {
			return err
		}
		printReport(rep)
		recall := -1.0
		if rep.SelfTest.Tested > 0 {
			recall = float64(rep.SelfTest.Found) / float64(rep.SelfTest.Tested)
		}
		h, err := health.Check(st, recall, true)
		if err != nil {
			return err
		}
		fmt.Println()
		printHealth(h)
		if path, err := writeHTMLReport(st, ""); err == nil {
			fmt.Println("\nReport:", path)
		}
		if h.Worst == health.Breach && rep.Snapshot != "" {
			fmt.Printf("\nA health bound was breached. To roll back tonight's cycle: engram snapshot restore %s\n", rep.Snapshot)
		}
		return nil

	case "report":
		fs := flag.NewFlagSet("report", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "print the raw report")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		runs, err := st.Runs(1)
		if err != nil {
			return err
		}
		if len(runs) == 0 || runs[0].Report == nil {
			fmt.Println("No sleep cycle has run yet. Try: engram sleep run")
			return nil
		}
		if *asJSON {
			fmt.Println(string(runs[0].Report))
			return nil
		}
		var rep sleep.Report
		if err := json.Unmarshal(runs[0].Report, &rep); err != nil {
			return err
		}
		printReport(rep)
		changes, err := st.Changes(rep.RunID)
		if err != nil {
			return err
		}
		if len(changes) > 0 {
			fmt.Println("\nChanges (undo with: engram sleep undo ID)")
			for _, c := range changes {
				state := ""
				if c.UndoneAt != "" {
					state = " (undone)"
				}
				fmt.Printf("  %4d  %-9s %s%s\n", c.ID, c.Kind, c.Detail, state)
			}
		}
		return nil

	case "undo":
		if len(args) != 2 {
			return fmt.Errorf("usage: engram sleep undo CHANGE_ID")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Errorf("change ids are numbers; see engram sleep report")
		}
		c, err := st.UndoChange(id)
		if err != nil {
			return err
		}
		fmt.Printf("Undid %s change %d: %s\n", c.Kind, c.ID, c.MemoryID)
		return nil

	case "schedule":
		exe, _ := os.Executable()
		fmt.Println("Run the cycle every 15 minutes; it only does work inside your sleep hours, when agents are idle, once a night.")
		fmt.Println("\ncron (Linux, or macOS with cron):")
		fmt.Printf("  */15 * * * * %s sleep run --if-due >> %s 2>&1\n", exe, filepath.Join(filepath.Dir(dbPath()), "sleep.log"))
		fmt.Println("\nlaunchd (macOS): save as ~/Library/LaunchAgents/dev.engram.sleep.plist, then launchctl load it:")
		fmt.Printf(`  <?xml version="1.0" encoding="UTF-8"?>
  <plist version="1.0"><dict>
    <key>Label</key><string>dev.engram.sleep</string>
    <key>ProgramArguments</key><array><string>%s</string><string>sleep</string><string>run</string><string>--if-due</string></array>
    <key>StartInterval</key><integer>900</integer>
  </dict></plist>
`, exe)
		return nil
	}
	return fmt.Errorf("unknown sleep command %q", args[0])
}

func printReport(r sleep.Report) {
	kind := "Full"
	if r.Light {
		kind = "Light"
	}
	fmt.Printf("%s sleep cycle %d, %s\n", kind, r.RunID, r.StartedAt)
	fmt.Printf("  pathways decayed %d, pruned %d; memories downscaled %d\n", r.Decayed, r.Pruned, r.Downscaled)
	fmt.Printf("  merged %d near-duplicates, archived %d faded memories\n", r.Merged, r.Archived)
	if !r.Light {
		fmt.Printf("  hubs found %d\n", r.Hubs)
		for _, d := range r.Distilled {
			mark := "kept"
			if d.MemoryID == "" {
				mark = "discarded"
			}
			fmt.Printf("  distilled (%s, %s): %s\n", d.Verdict, mark, d.Fact)
		}
	}
	if r.SelfTest.Tested > 0 {
		fmt.Printf("  self-test: recall found %d of %d decisions from their situations\n", r.SelfTest.Found, r.SelfTest.Tested)
		for _, m := range r.SelfTest.Misses {
			fmt.Println("    missed:", m)
		}
	}
	for _, n := range r.Notes {
		fmt.Println("  note:", n)
	}
	if r.Snapshot != "" {
		fmt.Println("  snapshot:", r.Snapshot)
	}
}

func runSnapshot(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		files, _ := filepath.Glob(filepath.Join(snapshotDir(), "engram-*.db"))
		if len(files) == 0 {
			fmt.Println("No snapshots yet; each sleep cycle makes one.")
		}
		for _, f := range files {
			fmt.Println(f)
		}
		return nil
	}
	if args[0] != "restore" || len(args) != 2 {
		return fmt.Errorf("usage: engram snapshot list | restore FILE")
	}
	src := args[1]
	if _, err := os.Stat(src); err != nil {
		return err
	}
	db := dbPath()
	backup := db + ".before-restore"
	if err := copyFile(db, backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(db + suffix)
	}
	if err := copyFile(src, db); err != nil {
		return err
	}
	fmt.Printf("Restored %s\nThe previous database was kept at %s\n", src, backup)
	return nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func lastDecisionRecall(st *store.Store) float64 {
	runs, err := st.Runs(1)
	if err != nil || len(runs) == 0 || runs[0].Report == nil {
		return -1
	}
	var r sleep.Report
	if json.Unmarshal(runs[0].Report, &r) != nil || r.SelfTest.Tested == 0 {
		return -1
	}
	return float64(r.SelfTest.Found) / float64(r.SelfTest.Tested)
}

func printHealth(r health.Report) {
	fmt.Println("Health:", r.Worst)
	for _, b := range append(append([]health.Bound{}, r.Bounds...), r.Drift...) {
		val := fmt.Sprintf("%.2f", b.Value)
		if b.Status == health.Unknown {
			val = "-"
		}
		fmt.Printf("  %-8s %-40s %-6s limit: %s", b.Status, b.Name, val, b.Limit)
		if b.Detail != "" {
			fmt.Printf("  (%s)", b.Detail)
		}
		fmt.Println()
	}
	for _, u := range r.Unknown {
		fmt.Println("  not yet measurable:", u)
	}
}

func runTrigger(st *store.Store, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: engram trigger add NAME [--kind webhook|github|job] [--ns NAMESPACE]")
		}
		fs := flag.NewFlagSet("trigger add", flag.ContinueOnError)
		kind := fs.String("kind", "webhook", "webhook, github or job")
		ns := fs.String("ns", "", "namespace for its events; default flow/NAME")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		t, err := st.AddTrigger(args[1], *kind, *ns)
		if err != nil {
			return err
		}
		path := "/hooks/" + t.Name
		if t.Kind == store.TriggerJob {
			path = "/jobs/" + t.Name
		}
		fmt.Printf("Created %s trigger %q writing to %s.\n", t.Kind, t.Name, t.Namespace)
		fmt.Printf("Secret (shown once): %s\n", t.Secret)
		fmt.Printf("Endpoint (with engram listen running): http://127.0.0.1:7077%s\n", path)
		if t.Kind == store.TriggerGitHub {
			fmt.Println("In GitHub: Settings, Webhooks: content type application/json, this secret, and the events you want.")
		} else {
			fmt.Printf("Example: curl -X POST -H 'Authorization: Bearer %s' -d '{\"title\":\"Deploy failed\",\"status\":\"failed\"}' http://127.0.0.1:7077%s\n", t.Secret, path)
		}
		fmt.Printf("Agents see its events once allowed to read it: engram pair claude-code --read %s\n", t.Namespace)
		return nil
	case "list":
		ts, err := st.Triggers()
		if err != nil {
			return err
		}
		if len(ts) == 0 {
			fmt.Println("No triggers. Create one with: engram trigger add NAME")
		}
		for _, t := range ts {
			state := "active"
			if t.Paused {
				state = "paused"
			}
			fmt.Printf("%-24s %-8s %-28s %-7s fired %d (failed %d)  last %s\n", t.Name, t.Kind, t.Namespace, state, t.Fired, t.Failed, t.LastFiredAt)
		}
		return nil
	case "events":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		evs, err := st.TriggerEvents(name, 30)
		if err != nil {
			return err
		}
		for _, e := range evs {
			fmt.Printf("%s  %-20s %-12s notified %d  %s\n", e.At, e.Trigger, e.Status, e.Notified, e.Summary)
		}
		return nil
	case "pause", "resume":
		if len(args) != 2 {
			return fmt.Errorf("usage: engram trigger %s NAME", args[0])
		}
		return st.PauseTrigger(args[1], args[0] == "pause")
	case "remove":
		if len(args) != 2 {
			return fmt.Errorf("usage: engram trigger remove NAME")
		}
		return st.RemoveTrigger(args[1])
	}
	return fmt.Errorf("unknown trigger command %q", args[0])
}

func runJob(st *store.Store, args []string) error {
	if len(args) >= 3 && args[0] == "run" {
		name, argv := args[1], args[2:]
		if argv[0] == "--" {
			argv = argv[1:]
		}
		res, err := trigger.RunJob(context.Background(), st, name, argv, os.Stdout, os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "engram: could not record the job:", err)
		} else {
			fmt.Fprintf(os.Stderr, "engram: recorded job %s (exit %d, %s), notified %d subscriber(s)\n",
				name, res.ExitCode, res.Duration.Round(time.Millisecond), len(res.Notified))
		}
		if res.ExitCode != 0 {
			st.Close()
			os.Exit(res.ExitCode) // pass the job's exit code through to cron
		}
		return nil
	}
	if len(args) >= 2 && args[0] == "report" {
		fs := flag.NewFlagSet("job report", flag.ContinueOnError)
		status := fs.String("status", "", "ok or failed")
		summary := fs.String("summary", "", "one line about the outcome")
		output := fs.String("output", "", "details, such as the end of the log")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *status != "ok" && *status != "failed" {
			return fmt.Errorf("--status must be ok or failed")
		}
		name := args[1]
		if _, err := st.GetTrigger(name); err != nil {
			if _, err := st.AddTrigger(name, store.TriggerJob, ""); err != nil {
				return err
			}
		}
		res, err := trigger.Record(st, trigger.Event{Trigger: name, Title: trigger.JobTitle(name, *status, *summary, 0),
			Body: *output, Status: *status, Tags: []string{"job"}})
		if err != nil {
			return err
		}
		fmt.Printf("Recorded %s, notified %d subscriber(s)\n", res.MemoryID, len(res.Notified))
		return nil
	}
	return fmt.Errorf("usage: engram job run NAME -- COMMAND [ARGS] | engram job report NAME --status ok|failed [--summary TEXT]")
}

func runPermit(st *store.Store, args []string) error {
	if len(args) == 0 {
		args = []string{"report"}
	}
	switch args[0] {
	case "on", "off":
		v := "true"
		if args[0] == "off" {
			v = "false"
		}
		if err := st.SetSetting(permit.SettingEnabled, v); err != nil {
			return err
		}
		fmt.Println("Permission assist logging is", args[0]+".")
		return nil
	case "explain":
		if len(args) != 3 {
			return fmt.Errorf(`usage: engram permit explain Bash "git push --force" | Edit path/to/file`)
		}
		tool, arg := args[1], args[2]
		input := map[string]any{"file_path": arg}
		switch tool {
		case "Bash":
			input = map[string]any{"command": arg}
		case "Grep", "Glob":
			input = map[string]any{"pattern": arg}
		case "WebFetch":
			input = map[string]any{"url": arg}
		}
		cwd, _ := os.Getwd()
		p, err := permit.Predict(st, "claude-code", tool, input, cwd)
		if err != nil {
			return err
		}
		fmt.Printf("tier:       %s\ncategory:   %s\nprediction: %s (confidence %.2f from %d precedents)\nwhy:        %s\n",
			p.Tier, p.Category, p.Decision, p.Confidence, p.Precedents, p.Reason)
		return nil
	case "report":
		rep, err := permit.BuildReport(st, 5000)
		if err != nil {
			return err
		}
		if len(args) > 1 && args[1] == "--json" {
			b, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		state := "on"
		if !rep.Enabled {
			state = "off"
		}
		fmt.Printf("Permission assist: shadow mode, logging %s. Engram never answers prompts in this version.\n", state)
		fmt.Println("Rule:", rep.Threshold)
		fmt.Println()
		fmt.Printf("%-9s %8s %8s %8s %8s %10s %10s %10s\n", "tier", "requests", "approved", "denied", "allow", "precision", "wrong", "avoided")
		for _, t := range rep.Tiers {
			fmt.Printf("%-9s %8d %8d %8d %8d %10.3f %10d %9.0f%%\n", t.Tier, t.Requests, t.Approved, t.Denied,
				t.PredictedAllow, t.Precision, t.AllowDenied, t.PromptsAvoided*100)
		}
		fmt.Println()
		for _, t := range rep.Tiers {
			if t.Tier == permit.Eligible {
				fmt.Println("Ready for auto-approval:", t.ReadyForAuto, "-", t.ReadinessReason)
			}
		}
		if len(rep.Mistakes) > 0 {
			fmt.Println("\nWould have approved, but you denied:")
			for _, m := range rep.Mistakes {
				fmt.Printf("  %s  %s\n", m.At, m.Summary)
			}
		}
		return nil
	}
	return fmt.Errorf("usage: engram permit report [--json] | explain TOOL INPUT | on | off")
}

// writeHTMLReport writes the static report, readable only by the user.
func writeHTMLReport(st *store.Store, out string) (string, error) {
	if out == "" {
		out = filepath.Join(filepath.Dir(st.Path()), "report.html")
	}
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if err := dashboard.WriteReport(st, f); err != nil {
		f.Close()
		return "", err
	}
	return out, f.Close()
}

// chatFromEnv connects the configured chat model, or returns nil when none answers.
func chatFromEnv(verbose bool) llm.Chat {
	c, note := llm.New(context.Background(), llm.FromEnv())
	if verbose {
		fmt.Fprintln(os.Stderr, "engram:", note)
	}
	if c == nil {
		return nil // keep the interface nil, not a typed nil
	}
	return c
}

func runToken(st *store.Store, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: engram token create CLIENT [--service]")
		}
		fs := flag.NewFlagSet("token create", flag.ContinueOnError)
		service := fs.Bool("service", false, "a headless program (cron job, CI, webhook bridge); its writes default to author system")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		tok, err := st.CreateToken(args[1], *service)
		if err != nil {
			return err
		}
		fmt.Printf("Token for %s (shown once; store it like a password):\n  %s\n", args[1], tok)
		if gs, _ := st.Grants(args[1]); len(gs) == 0 {
			fmt.Printf("It is not paired yet, so it can read and write nothing. Approve it with:\n  engram pair %s --read me --write me\n", args[1])
		}
		return nil
	case "list":
		ts, err := st.Tokens()
		if err != nil {
			return err
		}
		if len(ts) == 0 {
			fmt.Println("No tokens. Create one with: engram token create CLIENT")
		}
		for _, t := range ts {
			state := "active"
			if t.Revoked {
				state = "revoked"
			}
			kind := "agent"
			if t.Service {
				kind = "service"
			}
			fmt.Printf("%s…  %-20s %-8s %-8s created %s  last used %s\n", t.Prefix, t.Client, kind, state, t.CreatedAt, t.LastUsed)
		}
		return nil
	case "revoke":
		if len(args) != 2 {
			return fmt.Errorf("usage: engram token revoke CLIENT|PREFIX")
		}
		n, err := st.RevokeTokens(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Revoked %d token(s)\n", n)
		return nil
	}
	return fmt.Errorf("usage: engram token create CLIENT [--service] | list | revoke CLIENT|PREFIX")
}

func runConformance(args []string) error {
	fs := flag.NewFlagSet("conformance", flag.ContinueOnError)
	url := fs.String("url", "", "the server's ECP HTTP endpoint, such as http://127.0.0.1:7079/ecp")
	token := fs.String("token", os.Getenv("ECP_TOKEN"), "bearer token (or ECP_TOKEN)")
	client := fs.String("client", "conformance", "the client identity the server should attribute writes to")
	ns := fs.String("ns", "conformance/test", "a namespace the client may read and write")
	denied := fs.String("denied", "conformance/denied", "a namespace the client may not access")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	t := conformance.Target{URL: *url, Token: *token, Command: fs.Args(), Client: *client, Namespace: *ns, Denied: *denied}
	if t.URL == "" && len(t.Command) == 0 {
		return fmt.Errorf(`usage: engram conformance --url URL --token TOKEN | engram conformance -- COMMAND [ARGS]

Pair the client first, for example:
  engram pair conformance --read conformance/test --write conformance/test
  engram conformance -- engram serve --client conformance`)
	}
	rep, err := conformance.Run(context.Background(), t)
	if err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
	} else {
		rep.Print(os.Stdout)
	}
	if n := rep.Failed(conformance.Must); n > 0 {
		return fmt.Errorf("%d MUST requirement(s) failed", n)
	}
	return nil
}
