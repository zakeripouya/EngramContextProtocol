package bootstrap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Scanning finds the projects a user already has and absorbs them: git history
// (ImportGit), the instructions they already give agents (CLAUDE.md, AGENTS.md,
// Cursor rules), their docs, and their stack. Everything is read from files git
// tracks, so .gitignore is respected and untracked secrets are never opened;
// every chunk still passes the secret filter before it is stored.

// Strengths for absorbed memories: agent rules are explicit conventions the user
// wrote, so they start a little above other imports, and below user decisions (0.5).
const (
	rulesStrength = 0.4
	docStrength   = ImportStrength
	repoStrength  = 0.35
	maxDocFiles   = 150
	maxDocBytes   = 200 << 10
	maxChunk      = 1400
)

// RepoPlan describes what absorbing one repository would store. It is what
// `engram absorb --dry-run --json` prints and what the setup screen shows.
type RepoPlan struct {
	Path       string   `json:"path"`
	Name       string   `json:"name"`
	Namespace  string   `json:"namespace"`
	Commits    int      `json:"commits"`
	LastCommit string   `json:"lastCommit,omitempty"`
	Stack      []string `json:"stack"`
	Rules      []string `json:"rules"`
	Docs       []string `json:"docs"`
	Sections   int      `json:"sections"`
	Absorbed   bool     `json:"absorbed"` // absorbed before; running again only adds what changed
}

// AbsorbOptions controls Absorb.
type AbsorbOptions struct {
	Since      string // git log --since
	MaxCommits int    // default 2000
	NoGit      bool
	NoDocs     bool
	Progress   func(stage string, done, total int)
}

// AbsorbResult reports what Absorb stored.
type AbsorbResult struct {
	Repo      string    `json:"repo"`
	Namespace string    `json:"namespace"`
	Git       GitResult `json:"git"`
	Rules     int       `json:"rules"`
	Sections  int       `json:"sections"`
	Updated   int       `json:"updated"`
	Archived  int       `json:"archived"`
	Linked    int       `json:"linked"`
	Secrets   int       `json:"skippedSecrets"`
	Stack     []string  `json:"stack"`
}

// skipDirs are never descended into while looking for repositories.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "Library": true, "Applications": true, "Pictures": true,
	"Movies": true, "Music": true, "Photos": true, "target": true, "dist": true, "build": true,
	"venv": true, "__pycache__": true, "pkg": true, "AppData": true, "snap": true,
}

// FindRepos returns the git repositories under root, searching at most depth
// directories down and skipping hidden, dependency and media folders.
func FindRepos(root string, depth int) ([]string, error) {
	if depth <= 0 {
		depth = 4
	}
	root, err := filepath.Abs(expandHome(root))
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(root); err != nil {
		return nil, err
	}
	var repos []string
	base := strings.Count(root, string(os.PathSeparator))
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return fs.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if p != root && (strings.HasPrefix(name, ".") || skipDirs[name]) {
			return fs.SkipDir
		}
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			repos = append(repos, p)
			return fs.SkipDir // nested repos (submodules, vendored clones) belong to their parent
		}
		if strings.Count(p, string(os.PathSeparator))-base >= depth || len(repos) >= 300 {
			return fs.SkipDir
		}
		return nil
	})
	sort.Strings(repos)
	return repos, err
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

var nsUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// ProposeNamespace turns a folder name into a namespace: "INK" → "ink",
// "EasyScript" → "easyscript", "EngramContextProtocol" → "engram-context-protocol".
func ProposeNamespace(name string) string {
	words := splitCamel(name)
	var ns string
	if len(words) >= 3 {
		ns = strings.ToLower(strings.Join(words, "-"))
	} else {
		ns = strings.ToLower(name)
	}
	ns = nsUnsafe.ReplaceAllString(strings.ReplaceAll(ns, " ", "-"), "-")
	ns = strings.Trim(ns, "-._")
	if ns == "" {
		ns = "project"
	}
	return ns
}

func splitCamel(s string) []string {
	var words []string
	var cur []rune
	rs := []rune(s)
	for i, r := range rs {
		if r == '-' || r == '_' || r == ' ' || r == '.' {
			if len(cur) > 0 {
				words = append(words, string(cur))
			}
			cur = nil
			continue
		}
		if i > 0 && unicode.IsUpper(r) && len(cur) > 0 &&
			(unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]))) {
			words = append(words, string(cur))
			cur = nil
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		words = append(words, string(cur))
	}
	return words
}

// PlanRepos describes each repository without storing anything. Namespaces are
// proposed from folder names and made unique.
func PlanRepos(ctx context.Context, st *store.Store, paths []string, o AbsorbOptions) []RepoPlan {
	used := map[string]int{}
	var plans []RepoPlan
	for _, p := range paths {
		plan, err := PlanRepo(ctx, st, p, "", o)
		if err != nil {
			continue
		}
		used[plan.Namespace]++
		if n := used[plan.Namespace]; n > 1 {
			plan.Namespace = fmt.Sprintf("%s-%d", plan.Namespace, n)
		}
		plans = append(plans, plan)
	}
	return plans
}

// PlanRepo describes one repository. An empty namespace is proposed from its name.
func PlanRepo(ctx context.Context, st *store.Store, path, namespace string, o AbsorbOptions) (RepoPlan, error) {
	abs, err := filepath.Abs(expandHome(path))
	if err != nil {
		return RepoPlan{}, err
	}
	files, err := trackedFiles(ctx, abs)
	if err != nil {
		return RepoPlan{}, err
	}
	name := filepath.Base(abs)
	if namespace == "" {
		namespace = ProposeNamespace(name)
	}
	plan := RepoPlan{Path: abs, Name: name, Namespace: namespace, Stack: DetectStack(abs, files)}
	if o.MaxCommits <= 0 {
		o.MaxCommits = 2000
	}
	args := []string{"rev-list", "--count", "--no-merges", "HEAD"}
	if o.Since != "" {
		args = append(args, "--since="+o.Since)
	}
	if out, err := runGit(ctx, abs, args...); err == nil {
		n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		plan.Commits = min(n, o.MaxCommits)
	}
	if out, err := runGit(ctx, abs, "log", "-1", "--format=%cs"); err == nil {
		plan.LastCommit = strings.TrimSpace(string(out))
	}
	rules, docs := pickDocs(files)
	plan.Rules, plan.Docs = rules, docs
	for _, f := range append(append([]string{}, rules...), docs...) {
		if b, err := readDoc(abs, f); err == nil {
			plan.Sections += len(chunkMarkdown(b))
		}
	}
	if st != nil {
		plan.Absorbed, _ = st.KeyExists("repo:" + name)
	}
	return plan, nil
}

func trackedFiles(ctx context.Context, repo string) ([]string, error) {
	out, err := runGit(ctx, repo, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	return files, nil
}

var rulesFiles = map[string]bool{"CLAUDE.md": true, "AGENTS.md": true, "GEMINI.md": true, ".cursorrules": true, ".windsurfrules": true}

// IsRulesFile reports whether a repository path holds instructions for agents.
func IsRulesFile(f string) bool {
	base := filepath.Base(f)
	if rulesFiles[base] || f == ".github/copilot-instructions.md" {
		return true
	}
	return strings.HasPrefix(f, ".cursor/rules/") && (strings.HasSuffix(f, ".md") || strings.HasSuffix(f, ".mdc"))
}

func isDocFile(f string) bool {
	lower := strings.ToLower(f)
	if !strings.HasSuffix(lower, ".md") && !strings.HasSuffix(lower, ".mdx") {
		return false
	}
	base := strings.ToLower(filepath.Base(f))
	for _, skip := range []string{"changelog", "license", "licence", "code_of_conduct", "pull_request_template"} {
		if strings.HasPrefix(base, skip) {
			return false
		}
	}
	for _, part := range strings.Split(lower, "/") {
		if part == "node_modules" || part == "vendor" || part == "third_party" || part == "issue_template" || part == "testdata" {
			return false
		}
	}
	depth := strings.Count(f, "/")
	switch {
	case base == "readme.md" && depth <= 2:
		return true
	case depth == 0 && (base == "contributing.md" || base == "architecture.md" || base == "design.md" || base == "security.md"):
		return true
	}
	for _, dir := range []string{"docs/", "doc/", "adr/", "adrs/", "decisions/", "design/", "rfcs/"} {
		if strings.HasPrefix(lower, dir) || strings.Contains(lower, "/"+dir) {
			return true
		}
	}
	return false
}

// pickDocs returns the agent rules files and the docs worth absorbing, rules
// first and READMEs before deeper docs, capped so a docs-heavy repo stays small.
func pickDocs(files []string) (rules, docs []string) {
	for _, f := range files {
		switch {
		case IsRulesFile(f):
			rules = append(rules, f)
		case isDocFile(f):
			docs = append(docs, f)
		}
	}
	sort.SliceStable(docs, func(i, j int) bool {
		ri, rj := strings.EqualFold(filepath.Base(docs[i]), "readme.md"), strings.EqualFold(filepath.Base(docs[j]), "readme.md")
		if ri != rj {
			return ri
		}
		di, dj := strings.Count(docs[i], "/"), strings.Count(docs[j], "/")
		if di != dj {
			return di < dj
		}
		return docs[i] < docs[j]
	})
	if len(docs) > maxDocFiles {
		docs = docs[:maxDocFiles]
	}
	return rules, docs
}

func readDoc(repo, f string) ([]byte, error) {
	p := filepath.Join(repo, filepath.FromSlash(f))
	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxDocBytes {
		return nil, fmt.Errorf("skipped %s", f)
	}
	return os.ReadFile(p)
}

// Chunk is one section of a markdown file.
type Chunk struct {
	Heading []string // heading path, outermost first
	Text    string
}

var headingRE = regexp.MustCompile(`^(#{1,4})\s+(.+?)\s*#*\s*$`)
var commentRE = regexp.MustCompile(`(?s)<!--.*?-->`)

// chunkMarkdown splits a document at its headings (levels 1–4, outside code
// fences), drops front matter, empty sections and HTML comments, and splits long
// sections at paragraph breaks.
func chunkMarkdown(b []byte) []Chunk {
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	if strings.HasPrefix(text, "---\n") {
		if end := strings.Index(text[4:], "\n---"); end >= 0 {
			text = text[4+end+4:]
		}
	}
	text = commentRE.ReplaceAllString(text, "")
	var chunks []Chunk
	var path []string
	var body strings.Builder
	flush := func() {
		t := strings.TrimSpace(body.String())
		body.Reset()
		if len(strings.Fields(t)) < 5 {
			return
		}
		for _, part := range splitLong(t) {
			chunks = append(chunks, Chunk{Heading: append([]string{}, path...), Text: part})
		}
	}
	fenced := false
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		}
		if !fenced {
			if m := headingRE.FindStringSubmatch(line); m != nil {
				flush()
				level := len(m[1])
				if level-1 < len(path) {
					path = path[:level-1]
				}
				for len(path) < level-1 {
					path = append(path, "")
				}
				path = append(path, strings.Trim(m[2], "*`_ "))
				continue
			}
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	flush()
	return chunks
}

func splitLong(t string) []string {
	if len(t) <= maxChunk {
		return []string{t}
	}
	var out []string
	var cur strings.Builder
	for _, para := range strings.Split(t, "\n\n") {
		if cur.Len() > 0 && cur.Len()+len(para) > maxChunk {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
		for len(para) > maxChunk { // one huge paragraph: cut at a line break or space
			cut := strings.LastIndexAny(para[:maxChunk], "\n ")
			if cut < maxChunk/2 {
				cut = maxChunk
			}
			out = append(out, strings.TrimSpace(para[:cut]))
			para = para[cut:]
		}
		cur.WriteString(para)
		cur.WriteString("\n\n")
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func headingLabel(h []string) string {
	var parts []string
	for _, p := range h {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " › ")
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slug(h []string) string {
	s := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(headingLabel(h)), "-"), "-")
	if len(s) > 80 {
		s = s[:80]
	}
	if s == "" {
		s = "top"
	}
	return s
}

// Absorb stores what PlanRepo describes. Running it again imports new commits,
// updates changed sections, retires sections that no longer exist, and keeps
// every learned weight.
func Absorb(ctx context.Context, st *store.Store, plan RepoPlan, o AbsorbOptions) (AbsorbResult, error) {
	res := AbsorbResult{Repo: plan.Name, Namespace: plan.Namespace, Stack: plan.Stack}
	progress := func(stage string, done, total int) {
		if o.Progress != nil {
			o.Progress(stage, done, total)
		}
	}

	// The repository itself is a concept everything else hangs off.
	repoContent := fmt.Sprintf("Repository %s (%s)", plan.Name, plan.Path)
	if len(plan.Stack) > 0 {
		repoContent += ": " + strings.Join(plan.Stack, ", ")
	}
	repoID, err := upsertSystem(st, store.SystemMemory{
		Key: "repo:" + plan.Name, Kind: "concept", Namespace: plan.Namespace, Service: "scan",
		Content: repoContent, Tags: []string{"repo", plan.Name}, Strength: repoStrength,
	}, nil)
	if err != nil {
		return res, err
	}

	if !o.NoGit && plan.Commits > 0 {
		progress("history", 0, plan.Commits)
		g, err := ImportGit(ctx, st, GitOptions{Repo: plan.Path, Namespace: plan.Namespace, Since: o.Since,
			MaxCommits: o.MaxCommits, Progress: func(d, t int) { progress("history", d, t) }})
		if err != nil {
			return res, err
		}
		res.Git = g
		progress("history", plan.Commits, plan.Commits)
	}
	if o.NoDocs {
		return res, nil
	}

	// Tracked files that history made into concepts, for linking docs that mention them.
	fileIDs := map[string]string{}
	if keys, err := st.SystemKeys("git-file:" + plan.Name + ":"); err == nil {
		for k, id := range keys {
			fileIDs[strings.TrimPrefix(k, "git-file:"+plan.Name+":")] = id
		}
	}
	seen := map[string]bool{}
	files := append(append([]string{}, plan.Rules...), plan.Docs...)
	total := max(plan.Sections, 1)
	done := 0
	progress("docs", 0, total)
	for _, f := range files {
		b, err := readDoc(plan.Path, f)
		if err != nil {
			continue
		}
		rules := IsRulesFile(f)
		docKey := "docfile:" + plan.Name + ":" + f
		seen[docKey] = true
		kindWord := "Doc"
		if rules {
			kindWord = "Agent instructions"
		}
		docID, err := upsertSystem(st, store.SystemMemory{
			Key: docKey, Kind: "concept", Namespace: plan.Namespace, Service: "scan",
			Content: fmt.Sprintf("%s %s in %s", kindWord, f, plan.Name), Tags: []string{"doc", plan.Name}, Strength: docStrength,
		}, nil)
		if err != nil {
			return res, err
		}
		if ok, err := st.AddEdgeIfAbsent(docID, repoID, "about", 1); err == nil && ok {
			res.Linked++
		}
		counts := map[string]int{}
		for _, c := range chunkMarkdown(b) {
			done++
			progress("docs", min(done, total), total)
			s := slug(c.Heading)
			counts[s]++
			key := fmt.Sprintf("doc:%s:%s#%s", plan.Name, f, s)
			if n := counts[s]; n > 1 {
				key += "~" + strconv.Itoa(n)
			}
			seen[key] = true
			label := f
			if h := headingLabel(c.Heading); h != "" {
				label += " › " + h
			}
			content := fmt.Sprintf("[%s] %s: %s", plan.Name, label, c.Text)
			if store.FindSensitive(content) != "" {
				res.Secrets++
				continue
			}
			strength, tags := docStrength, []string{"doc", plan.Name}
			if rules {
				strength, tags = rulesStrength, []string{"rules", "convention", plan.Name}
			}
			created := false
			id, err := upsertSystem(st, store.SystemMemory{
				Key: key, Kind: "fact", Namespace: plan.Namespace, Service: "scan",
				Content: content, Tags: tags, Strength: strength,
			}, func(new, updated bool) {
				created = new
				if updated {
					res.Updated++
				}
			})
			if err != nil {
				return res, err
			}
			if created {
				if rules {
					res.Rules++
				} else {
					res.Sections++
				}
			}
			if ok, err := st.AddEdgeIfAbsent(id, docID, "about", 1); err == nil && ok {
				res.Linked++
			}
			for path, fid := range fileIDs {
				if strings.Contains(path, "/") && len(path) >= 6 && strings.Contains(c.Text, path) {
					if ok, err := st.AddEdgeIfAbsent(id, fid, "about", 0.8); err == nil && ok {
						res.Linked++
					}
				}
			}
		}
	}
	progress("docs", total, total)

	// Sections and docs that are gone are retired, not deleted, so history stays.
	for _, prefix := range []string{"doc:" + plan.Name + ":", "docfile:" + plan.Name + ":"} {
		keys, err := st.SystemKeys(prefix)
		if err != nil {
			return res, err
		}
		for k, id := range keys {
			if !seen[k] && st.Archive(id, "") == nil {
				res.Archived++
			}
		}
	}
	return res, nil
}

// upsertSystem inserts a system memory, or updates its content when the key
// exists and the content changed. report receives (created, updated).
func upsertSystem(st *store.Store, m store.SystemMemory, report func(created, updated bool)) (string, error) {
	id, created, err := st.InsertSystem(m)
	if err != nil {
		return "", err
	}
	updated := false
	if !created {
		if updated, err = st.UpdateSystemMemory(id, m.Content, m.Tags); err != nil {
			return "", err
		}
	}
	if report != nil {
		report(created, updated)
	}
	return id, nil
}

// DetectStack names the languages and main frameworks a repository uses, from
// its tracked file extensions and manifests.
func DetectStack(repo string, files []string) []string {
	exts := map[string]string{".go": "Go", ".ts": "TypeScript", ".tsx": "TypeScript", ".js": "JavaScript", ".jsx": "JavaScript",
		".py": "Python", ".rs": "Rust", ".c": "C", ".h": "C", ".cpp": "C++", ".cc": "C++", ".java": "Java", ".kt": "Kotlin",
		".rb": "Ruby", ".swift": "Swift", ".cs": "C#", ".php": "PHP", ".ex": "Elixir", ".scala": "Scala", ".dart": "Dart",
		".sql": "SQL", ".v": "Verilog", ".sv": "SystemVerilog", ".asm": "Assembly", ".s": "Assembly"}
	langCount := map[string]int{}
	codeFiles := 0
	var stack []string
	add := func(s string) {
		for _, x := range stack {
			if x == s {
				return
			}
		}
		stack = append(stack, s)
	}
	var tools []string
	addTool := func(s string) {
		for _, x := range tools {
			if x == s {
				return
			}
		}
		tools = append(tools, s)
	}
	var manifests []string
	for _, f := range files {
		if lang, ok := exts[strings.ToLower(filepath.Ext(f))]; ok && !strings.Contains(f, "node_modules/") && !strings.HasPrefix(f, "vendor/") {
			langCount[lang]++
			codeFiles++
		}
		base := filepath.Base(f)
		depth := strings.Count(f, "/")
		switch {
		case strings.HasSuffix(f, ".tf"):
			addTool("Terraform")
		case base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile."):
			addTool("Docker")
		case base == "docker-compose.yml" || base == "docker-compose.yaml" || base == "compose.yaml":
			addTool("Docker Compose")
		case strings.HasPrefix(f, ".github/workflows/"):
			addTool("GitHub Actions")
		case strings.HasSuffix(f, "schema.prisma"):
			addTool("Prisma")
		case depth <= 3 && (base == "package.json" || base == "go.mod" || base == "pyproject.toml" || base == "requirements.txt" || base == "Cargo.toml"):
			manifests = append(manifests, f)
		}
	}
	type lc struct {
		lang string
		n    int
	}
	var langs []lc
	for l, n := range langCount {
		langs = append(langs, lc{l, n})
	}
	sort.Slice(langs, func(i, j int) bool {
		return langs[i].n > langs[j].n || (langs[i].n == langs[j].n && langs[i].lang < langs[j].lang)
	})
	for _, l := range langs {
		if float64(l.n) >= 0.1*float64(codeFiles) && len(stack) < 4 && l.lang != "SQL" {
			add(l.lang)
		}
	}
	nodeFrameworks := []struct{ dep, name string }{{"next", "Next.js"}, {"react-native", "React Native"}, {"expo", "Expo"},
		{"react", "React"}, {"vue", "Vue"}, {"svelte", "Svelte"}, {"@angular/core", "Angular"}, {"express", "Express"},
		{"@nestjs/core", "NestJS"}, {"fastify", "Fastify"}, {"@prisma/client", "Prisma"}, {"sequelize", "Sequelize"},
		{"typeorm", "TypeORM"}, {"drizzle-orm", "Drizzle"}, {"tailwindcss", "Tailwind CSS"}, {"stripe", "Stripe"},
		{"vite", "Vite"}, {"vitest", "Vitest"}, {"jest", "Jest"}, {"@playwright/test", "Playwright"}, {"electron", "Electron"}}
	pyFrameworks := []struct{ dep, name string }{{"django", "Django"}, {"flask", "Flask"}, {"fastapi", "FastAPI"},
		{"torch", "PyTorch"}, {"tensorflow", "TensorFlow"}, {"pandas", "pandas"}, {"scikit-learn", "scikit-learn"}, {"xgboost", "XGBoost"}}
	for _, m := range manifests {
		b, err := readDoc(repo, m)
		if err != nil {
			continue
		}
		switch filepath.Base(m) {
		case "package.json":
			var pj struct {
				Dependencies    map[string]string `json:"dependencies"`
				DevDependencies map[string]string `json:"devDependencies"`
			}
			if json.Unmarshal(b, &pj) != nil {
				continue
			}
			for _, fw := range nodeFrameworks {
				_, a := pj.Dependencies[fw.dep]
				_, d := pj.DevDependencies[fw.dep]
				if a || d {
					addTool(fw.name)
				}
			}
		case "pyproject.toml", "requirements.txt":
			lower := strings.ToLower(string(b))
			for _, fw := range pyFrameworks {
				if regexp.MustCompile(`(?m)(^|["'\s])` + regexp.QuoteMeta(fw.dep) + `\b`).MatchString(lower) {
					addTool(fw.name)
				}
			}
		case "go.mod":
			for _, dep := range []struct{ mod, name string }{{"github.com/gin-gonic/gin", "Gin"}, {"github.com/labstack/echo", "Echo"},
				{"github.com/gofiber/fiber", "Fiber"}, {"github.com/go-chi/chi", "chi"}, {"gorm.io/gorm", "GORM"},
				{"github.com/mattn/go-sqlite3", "SQLite"}, {"github.com/jackc/pgx", "Postgres (pgx)"},
				{"github.com/segmentio/kafka-go", "Kafka"}, {"github.com/IBM/sarama", "Kafka"}, {"github.com/redis/go-redis", "Redis"},
				{"github.com/modelcontextprotocol/go-sdk", "MCP"}, {"github.com/charmbracelet/bubbletea", "Bubble Tea"}} {
				if bytes.Contains(b, []byte(dep.mod)) {
					addTool(dep.name)
				}
			}
		}
	}
	return append(stack, tools...)
}
