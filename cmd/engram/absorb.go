package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zakeripouya/EngramContextProtocol/internal/bootstrap"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// runAbsorb is `engram absorb`: absorb one project or every project under a
// folder (git history, agent rules, docs and stack). With --dry-run it only
// reports what it would store, which is how an agent can drive setup: read the
// plan, ask the user, then run the real command.
func runAbsorb(st *store.Store, args []string) error {
	fs := flag.NewFlagSet("absorb", flag.ContinueOnError)
	ns := fs.String("ns", "", "namespace for a single project (default: from the folder name)")
	depth := fs.Int("depth", 4, "how many folders deep to look for projects")
	since := fs.String("since", "", "only absorb history since, e.g. \"1 year ago\"")
	maxCommits := fs.Int("max-commits", 2000, "newest commits to absorb per project")
	noGit := fs.Bool("no-git", false, "skip git history")
	noDocs := fs.Bool("no-docs", false, "skip agent rules and docs")
	dryRun := fs.Bool("dry-run", false, "show what would be absorbed and store nothing")
	asJSON := fs.Bool("json", false, "print JSON")
	grant := fs.String("grant", "", "clients to give read and write on the new namespaces (default: clients that can write your personal memory)")
	noGrant := fs.Bool("no-grant", false, "don't change any client's grants")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	ctx := context.Background()
	o := bootstrap.AbsorbOptions{Since: *since, MaxCommits: *maxCommits, NoGit: *noGit, NoDocs: *noDocs}

	var repos []string
	for _, p := range paths {
		abs, err := filepath.Abs(expandTilde(p))
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
			repos = append(repos, abs)
			continue
		}
		found, err := bootstrap.FindRepos(abs, *depth)
		if err != nil {
			return err
		}
		repos = append(repos, found...)
	}
	if len(repos) == 0 {
		return fmt.Errorf("no git repositories found under %s", strings.Join(paths, ", "))
	}
	if *ns != "" && len(repos) > 1 {
		return fmt.Errorf("--ns names one project's namespace, but %d projects were found; absorb them one at a time to name each", len(repos))
	}
	var plans []bootstrap.RepoPlan
	if *ns != "" {
		if !nsRegexp.MatchString(*ns) || *ns == "me" {
			return fmt.Errorf("invalid namespace %q", *ns)
		}
		plan, err := bootstrap.PlanRepo(ctx, st, repos[0], *ns, o)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	} else {
		plans = bootstrap.PlanRepos(ctx, st, repos, o)
	}

	if *dryRun {
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(plans)
		}
		fmt.Println("Dry run: nothing is stored.")
		for _, p := range plans {
			fmt.Printf("\n%s → %s  (%s)\n", p.Name, p.Namespace, tildePath(p.Path))
			fmt.Printf("  %d commits, last %s; %d doc sections\n", p.Commits, p.LastCommit, p.Sections)
			if len(p.Rules) > 0 {
				fmt.Println("  agent rules:", strings.Join(p.Rules, ", "))
			}
			if len(p.Docs) > 0 {
				fmt.Printf("  docs: %d files (%s)\n", len(p.Docs), strings.Join(p.Docs[:min(4, len(p.Docs))], ", "))
			}
			if len(p.Stack) > 0 {
				fmt.Println("  stack:", strings.Join(p.Stack, ", "))
			}
			if p.Absorbed {
				fmt.Println("  absorbed before: only changes would be added")
			}
		}
		return nil
	}

	var clients []string
	if !*noGrant {
		if *grant != "" {
			clients = splitList(*grant)
		} else {
			grants, err := st.Grants("")
			if err != nil {
				return err
			}
			for _, g := range grants {
				if g.Namespace == "me" && g.CanWrite {
					clients = append(clients, g.Client)
				}
			}
		}
	}
	tty := isTerminal(os.Stderr)
	var results []bootstrap.AbsorbResult
	for _, plan := range plans {
		o.Progress = func(stage string, done, total int) {
			if tty && !*asJSON {
				fmt.Fprintf(os.Stderr, "\r\033[K%s: %s %d/%d", plan.Name, stage, done, total)
			}
		}
		res, err := bootstrap.Absorb(ctx, st, plan, o)
		if tty && !*asJSON {
			fmt.Fprint(os.Stderr, "\r\033[K")
		}
		if err != nil {
			return fmt.Errorf("%s: %w", plan.Name, err)
		}
		for _, c := range clients {
			if err := st.Pair(c, []string{plan.Namespace}, []string{plan.Namespace}, ""); err != nil {
				return err
			}
		}
		results = append(results, res)
		if !*asJSON {
			fmt.Printf("%s: %s\n", plan.Name, describeAbsorb(plan, res))
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}
	if len(clients) > 0 {
		fmt.Printf("Read and write on the new namespaces granted to %s.\n", strings.Join(clients, ", "))
	}
	return nil
}

// reorderFlags lets flags follow the paths (engram absorb ~/code --dry-run),
// which the standard flag package would otherwise treat as more paths.
func reorderFlags(args []string, valuedNames ...string) []string {
	if len(valuedNames) == 0 {
		valuedNames = []string{"ns", "depth", "since", "max-commits", "grant"}
	}
	valued := map[string]bool{}
	for _, n := range valuedNames {
		valued["-"+n], valued["--"+n] = true, true
	}
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			if valued[a] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
