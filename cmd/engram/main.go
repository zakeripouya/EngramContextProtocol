// Command engram is the Engram reference server and its management CLI.
//
//	engram serve [--client NAME]               run the MCP server over stdio
//	engram pair NAME --read NS --write NS      approve a client (the user does this, never an agent)
//	engram clients                             list paired clients and their grants
//	engram revoke NAME                         remove all of a client's grants
//	engram stats                               counts of memories, edges and clients
//	engram setup claude-code|claude-desktop    print connection instructions
//
// The database lives at $ENGRAM_DB, or ~/.engram/engram.db by default.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/embed"
	"github.com/zakeripouya/EngramContextProtocol/internal/server"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

const usage = `engram: shared memory for AI agents (ECP %s reference server, v%s)

Usage:
  engram serve [--client NAME]            run the MCP server over stdio
  engram pair NAME --read NS --write NS   approve a client; namespaces are comma-separated
  engram clients                          list paired clients and their grants
  engram revoke NAME                      remove all of a client's grants
  engram stats                            show memory counts
  engram reembed                          embed memories that have no vector yet and wire their neighbors
  engram decay                            weaken learned pathways and prune the weakest
  engram setup claude-code|claude-desktop print connection instructions
  engram version                          print the version

Database: $ENGRAM_DB, or ~/.engram/engram.db
Embeddings: $ENGRAM_EMBED = auto (default) | openai | hash | off
            $ENGRAM_EMBED_URL (default http://localhost:11434, Ollama), $ENGRAM_EMBED_MODEL
            (default nomic-embed-text), $ENGRAM_EMBED_KEY for hosted providers
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
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version":
		fmt.Printf("engram %s (ECP %s)\n", server.Version, ecp.ProtocolVersion)
		return nil
	case "setup":
		return setup(rest)
	}

	st, err := store.Open(dbPath())
	if err != nil {
		return err
	}
	defer st.Close()
	if cmd == "serve" || cmd == "reembed" {
		e, note := embed.New(context.Background(), embed.FromEnv())
		st.SetEmbedder(e)
		fmt.Fprintln(os.Stderr, "engram:", note) // stderr: stdout carries the MCP protocol
	}

	switch cmd {
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		client := fs.String("client", "", "identity of the host that launches this server, e.g. claude-code")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return server.New(st, *client).Run(ctx, &mcp.StdioTransport{})

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
		return fmt.Errorf("usage: engram setup claude-code|claude-desktop")
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
	case "claude-desktop":
		fmt.Println(`Add this to claude_desktop_config.json under "mcpServers":`)
		fmt.Printf(`  "engram": {"command": %q, "args": ["serve", "--client", "claude-desktop"]}`+"\n", exe)
		fmt.Println("Then approve it:  engram pair claude-desktop --read me --write me")
	default:
		return fmt.Errorf("unknown host %q; supported: claude-code, claude-desktop", args[0])
	}
	return nil
}
