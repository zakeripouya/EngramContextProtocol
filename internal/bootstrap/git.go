// Package bootstrap imports existing history so a new Engram starts out knowing
// something (milestone M3; docs/design.md, "Bootstrapping from history").
//
// Imported memories enter at low strength with provenance service "git", "jira"
// or "import" and author "system", so they never outrank what the user actually
// says, and they earn strength only through use.
package bootstrap

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// ImportStrength is the starting strength of imported memories (user facts start at 0.5).
const ImportStrength = 0.3

// GitOptions controls a git import.
type GitOptions struct {
	Repo       string // path to a local clone
	Namespace  string // e.g. team/ink
	Since      string // passed to git log --since, e.g. "6 months ago"; empty = all
	MaxCommits int    // newest first; default 2000
	MinCoFiles int    // a file pair must change together this many times to be linked; default 3
}

// GitResult reports what a git import created.
type GitResult struct {
	Commits     int      `json:"commits"`
	Skipped     int      `json:"skipped"` // already imported
	Files       int      `json:"files"`
	CoChange    int      `json:"coChangeEdges"`
	Conventions []string `json:"conventions"`
}

type commit struct {
	hash, author, subject string
	when                  time.Time
	files                 []string
}

const logFormat = "\x1e%H\x1f%an\x1f%aI\x1f%s"

func runGit(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func readCommits(ctx context.Context, o GitOptions) ([]commit, error) {
	args := []string{"log", "--no-merges", "--name-only", "--format=" + logFormat, fmt.Sprintf("-n%d", o.MaxCommits)}
	if o.Since != "" {
		args = append(args, "--since="+o.Since)
	}
	out, err := runGit(ctx, o.Repo, args...)
	if err != nil {
		return nil, err
	}
	var commits []commit
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		lines := strings.Split(rec, "\n")
		head := strings.Split(lines[0], "\x1f")
		if len(head) != 4 {
			continue
		}
		when, err := time.Parse(time.RFC3339, head[2])
		if err != nil {
			continue
		}
		c := commit{hash: head[0], author: head[1], when: when.UTC(), subject: head[3]}
		for _, f := range lines[1:] {
			if f = strings.TrimSpace(f); f != "" {
				c.files = append(c.files, f)
			}
		}
		commits = append(commits, c)
	}
	return commits, nil
}

func readBranches(ctx context.Context, repo string) []string {
	out, err := runGit(ctx, repo, "for-each-ref", "--format=%(refname:short)", "refs/heads", "refs/remotes")
	if err != nil {
		return nil
	}
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		n := strings.TrimSpace(sc.Text())
		if i := strings.Index(n, "/"); i >= 0 && strings.HasPrefix(n, "origin/") {
			n = n[i+1:]
		}
		if n != "" && n != "HEAD" && n != "origin" {
			names = append(names, n)
		}
	}
	return names
}

// ImportGit reads a repository's history into the store: one episode per commit,
// one concept per frequently changed file, "about" edges from commits to files,
// "coactive" edges between files that change together, and facts for naming
// conventions it can detect. Running it again imports only new commits.
func ImportGit(ctx context.Context, st *store.Store, o GitOptions) (GitResult, error) {
	if o.Namespace == "" {
		o.Namespace = "me"
	}
	if o.MaxCommits <= 0 {
		o.MaxCommits = 2000
	}
	if o.MinCoFiles <= 0 {
		o.MinCoFiles = 3
	}
	repoName := filepath.Base(filepath.Clean(o.Repo))
	commits, err := readCommits(ctx, o)
	if err != nil {
		return GitResult{}, err
	}
	var res GitResult

	// Files that changed in at least two commits become concepts.
	touches := map[string]int{}
	for _, c := range commits {
		for _, f := range c.files {
			touches[f]++
		}
	}
	fileIDs := map[string]string{}
	for f, n := range touches {
		if n < 2 {
			continue
		}
		id, created, err := st.InsertSystem(store.SystemMemory{
			Key: "git-file:" + repoName + ":" + f, Kind: "concept", Namespace: o.Namespace, Service: "git",
			Content: fmt.Sprintf("File %s in %s (changed in %d commits)", f, repoName, n),
			Tags:    []string{"git", "file", repoName}, Strength: ImportStrength,
		})
		if err != nil {
			return res, err
		}
		fileIDs[f] = id
		if created {
			res.Files++
		}
	}

	// One episode per commit, linked to the files it touched.
	pairs := map[[2]string]int{}
	for _, c := range commits {
		short := c.hash[:min(8, len(c.hash))]
		files := c.files
		listed := files
		if len(listed) > 6 {
			listed = append(append([]string{}, listed[:6]...), fmt.Sprintf("and %d more", len(files)-6))
		}
		content := fmt.Sprintf("Commit %s in %s by %s: %s", short, repoName, c.author, c.subject)
		if len(listed) > 0 {
			content += " (files: " + strings.Join(listed, ", ") + ")"
		}
		id, created, err := st.InsertSystem(store.SystemMemory{
			Key: "git-commit:" + c.hash, Kind: "episode", Namespace: o.Namespace, Service: "git",
			Content: content, Tags: []string{"git", "commit", repoName}, Strength: ImportStrength,
			ValidFrom: c.when,
		})
		if err != nil {
			return res, err
		}
		if !created {
			res.Skipped++
			continue
		}
		res.Commits++
		for _, f := range files {
			if fid, ok := fileIDs[f]; ok {
				if err := st.AddEdge(id, fid, "about", 1); err != nil {
					return res, err
				}
			}
		}
		if len(files) <= 30 { // huge commits (formatting, vendoring) say little about coupling
			for i := 0; i < len(files); i++ {
				for j := i + 1; j < len(files); j++ {
					a, b := files[i], files[j]
					if a > b {
						a, b = b, a
					}
					pairs[[2]string{a, b}]++
				}
			}
		}
	}

	// Files that repeatedly change together get a learned pathway, weighted by how often.
	for p, n := range pairs {
		a, okA := fileIDs[p[0]]
		b, okB := fileIDs[p[1]]
		if !okA || !okB || n < o.MinCoFiles {
			continue
		}
		w := float64(n) / float64(min(touches[p[0]], touches[p[1]]))
		if err := st.AddEdge(a, b, "coactive", min(0.6, w*0.6)); err != nil {
			return res, err
		}
		res.CoChange++
	}

	// Conventions the history shows clearly become facts.
	subjects := make([]string, len(commits))
	for i, c := range commits {
		subjects[i] = c.subject
	}
	for _, conv := range DetectConventions(subjects, readBranches(ctx, o.Repo)) {
		_, created, err := st.InsertSystem(store.SystemMemory{
			Key: "git-convention:" + repoName + ":" + conv, Kind: "fact", Namespace: o.Namespace, Service: "git",
			Content: conv + " (in " + repoName + ")", Tags: []string{"git", "convention", repoName},
			Strength: ImportStrength + 0.1,
		})
		if err != nil {
			return res, err
		}
		if created {
			res.Conventions = append(res.Conventions, conv)
		}
	}
	return res, nil
}

var (
	ticketKey      = regexp.MustCompile(`^([A-Z][A-Z0-9]+)-\d+`)
	conventionalRE = regexp.MustCompile(`^(feat|fix|chore|docs|refactor|test|perf|build|ci|style)(\([^)]*\))?!?: `)
)

// DetectConventions finds naming conventions followed by at least 60% of
// commit subjects or branch names (with at least 5 examples).
func DetectConventions(subjects, branches []string) []string {
	var out []string
	share := func(items []string, re *regexp.Regexp) (float64, string) {
		if len(items) < 5 {
			return 0, ""
		}
		counts := map[string]int{}
		hits := 0
		for _, s := range items {
			if m := re.FindStringSubmatch(s); m != nil {
				hits++
				if len(m) > 1 {
					counts[m[1]]++
				}
			}
		}
		best, bestN := "", 0
		for k, n := range counts {
			if n > bestN || (n == bestN && k < best) {
				best, bestN = k, n
			}
		}
		return float64(hits) / float64(len(items)), best
	}
	if f, key := share(branches, ticketKey); f >= 0.6 {
		out = append(out, fmt.Sprintf("Branch names start with the ticket key, like %s-123-short-description", key))
	}
	if f, key := share(subjects, ticketKey); f >= 0.6 {
		out = append(out, fmt.Sprintf("Commit messages start with the ticket key, like %s-123: what changed", key))
	}
	if f, _ := share(subjects, conventionalRE); f >= 0.6 {
		out = append(out, "Commit messages follow Conventional Commits (feat:, fix:, chore: ...)")
	}
	sort.Strings(out)
	return out
}
