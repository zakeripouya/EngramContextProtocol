// Package permit is the permission assist (milestone M9; design, "Permission
// assist"), in shadow mode only: Engram predicts whether the user would approve a
// tool call, logs the prediction next to what the user actually decided, and
// reports agreement. It never answers a permission prompt.
//
// Eligibility is decided by fixed rules, never by a model: only read-only or
// easily reversible actions inside the project can ever be predicted "allow".
// Deletes, pushes, deploys, secrets, network calls and anything outside the
// project are "never", and anything the rules do not recognize is "unknown";
// both always mean ask.
package permit

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Tiers.
const (
	Eligible = "eligible"
	Never    = "never"
	Unknown  = "unknown"
)

// Classification is the rule verdict for one tool call.
type Classification struct {
	Tier     string `json:"tier"`
	Category string `json:"category"` // precedents are counted per category, such as "Bash:go test"
	Summary  string `json:"summary"`
	Reason   string `json:"reason"`
}

var readOnlyTools = map[string]bool{"Read": true, "Glob": true, "Grep": true, "LS": true, "NotebookRead": true, "TodoWrite": true}
var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

var sensitivePath = regexp.MustCompile(`(?i)(^|/)(\.env(\..*)?|\.git(/|$)|\.ssh(/|$)|\.aws(/|$)|\.gnupg(/|$)|id_rsa|id_ed25519|.*\.pem|.*\.key|credentials(\.json)?|secrets?(\.|/|$)|\.npmrc|\.pypirc|\.netrc)`)

// Commands that are never auto-approved, matched against each simple command.
var neverCommand = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`^(sudo|doas|su)\b`), "elevated privileges"},
	{regexp.MustCompile(`^(rm|rmdir|shred|unlink|truncate|dd|mkfs\S*)\b`), "deletes or overwrites data"},
	{regexp.MustCompile(`^git\s+(push|reset\s+--hard|clean|filter-branch|filter-repo|update-ref|reflog\s+expire|gc\s+--prune)`), "rewrites or publishes git history"},
	{regexp.MustCompile(`^git\s+(checkout|restore)\s+(--\s|\.|-f)`), "discards local changes"},
	{regexp.MustCompile(`^git\s+branch\s+-D`), "deletes a branch"},
	{regexp.MustCompile(`^(curl|wget|ssh|scp|sftp|rsync|nc|ncat|netcat|telnet|ftp)\b`), "network call"},
	{regexp.MustCompile(`^(npm|pnpm|yarn)\s+(publish|unpublish|deprecate|login|adduser|token)`), "publishes a package"},
	{regexp.MustCompile(`^(cargo\s+publish|twine\s+upload|gem\s+push|docker\s+(push|login)|gh\s+(release|pr\s+merge|repo\s+delete|secret|api))`), "publishes or changes remote state"},
	{regexp.MustCompile(`^(kubectl|helm|terraform|pulumi|flyctl|fly|vercel|netlify|heroku|aws|gcloud|az|doctl)\b`), "infrastructure or deploys"},
	{regexp.MustCompile(`(?i)\b(deploy|release|migrate|migration)\b`), "deploys or migrations"},
	{regexp.MustCompile(`(?i)\b(drop\s+(table|database)|truncate\s+table|delete\s+from)\b`), "destructive database statement"},
	{regexp.MustCompile(`^(chmod|chown|chgrp)\b`), "changes permissions"},
	{regexp.MustCompile(`^(env|printenv|export|set)\b`), "may expose secrets"},
	{regexp.MustCompile(`^(eval|exec|source|\.|bash|sh|zsh|python3?\s+-c|node\s+-e|perl\s+-e)\b`), "runs arbitrary code"},
	{regexp.MustCompile(`^(kill|pkill|killall|shutdown|reboot|systemctl|launchctl|crontab)\b`), "changes the system"},
	{regexp.MustCompile(`^(mail|sendmail|mutt)\b`), "sends a message"},
	{regexp.MustCompile(`^(stripe|paypal)\b`), "payments"},
}

// Commands that can be eligible: read-only inspection, tests and local builds.
var eligibleCommand = []*regexp.Regexp{
	regexp.MustCompile(`^(ls|pwd|cat|head|tail|wc|file|stat|tree|du|df|which|type|echo|date|uname|whoami|diff|cmp|sort|uniq|cut|tr|jq|yq|basename|dirname|realpath|true)\b`),
	regexp.MustCompile(`^(grep|egrep|fgrep|rg|ag|fd)\b`),
	regexp.MustCompile(`^find\b`),
	regexp.MustCompile(`^git\s+(status|diff|log|show|branch|blame|ls-files|rev-parse|describe|remote\s+-v|stash\s+list|grep|shortlog)\b`),
	regexp.MustCompile(`^go\s+(test|build|vet|fmt|list|version|env|doc|mod\s+(tidy|download|verify|why|graph))\b`),
	regexp.MustCompile(`^gofmt\b`),
	regexp.MustCompile(`^(npm|pnpm|yarn|bun)\s+(test|run\s+(test|build|lint|typecheck|check|format)|ls|list|outdated)\b`),
	regexp.MustCompile(`^(npx|pnpm\s+exec|yarn\s+exec)\s+(tsc|eslint|prettier|vitest|jest|playwright\s+test)\b`),
	regexp.MustCompile(`^(pytest|python3?\s+-m\s+(pytest|unittest|mypy|ruff|black)|ruff|black|mypy|tox)\b`),
	regexp.MustCompile(`^cargo\s+(test|build|check|clippy|fmt|doc|tree)\b`),
	regexp.MustCompile(`^make(\s+(test|build|check|lint|fmt|all))?\s*$`),
	regexp.MustCompile(`^(mvn|gradle|\./gradlew)\s+(test|build|compile|check|verify)\b`),
	regexp.MustCompile(`^(dotnet\s+(test|build)|swift\s+(test|build)|xcodebuild\s+test|bundle\s+exec\s+rspec|rspec|rake\s+test|mix\s+test|deno\s+(test|check|lint|fmt))\b`),
	regexp.MustCompile(`^(mkdir|touch)\b`),
}

var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*\s+`)

// splitCommand splits a shell line into simple commands on ;, &&, || and |.
// It returns ok=false when the line has constructs the rules cannot see through.
func splitCommand(line string) (parts []string, ok bool) {
	if strings.ContainsAny(line, "`") || strings.Contains(line, "$(") || strings.Contains(line, "<(") || strings.Contains(line, "\n") {
		return nil, false
	}
	var cur strings.Builder
	quote := rune(0)
	flush := func() {
		if p := strings.TrimSpace(cur.String()); p != "" {
			parts = append(parts, p)
		}
		cur.Reset()
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == ';' || r == '|' || r == '&':
			if r == '&' && (i+1 >= len(rs) || rs[i+1] != '&') {
				if i > 0 && (rs[i-1] == '>' || rs[i-1] == '<') || i+1 < len(rs) && rs[i+1] == '>' {
					cur.WriteRune(r) // 2>&1, &>
					continue
				}
				return nil, false // background job
			}
			flush()
			if i+1 < len(rs) && (rs[i+1] == '&' || rs[i+1] == '|') {
				i++
			}
		default:
			cur.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, false
	}
	flush()
	return parts, len(parts) > 0
}

// redirectsOutside reports whether a command writes with > or >> to a path outside the project.
var redirect = regexp.MustCompile(`(^|[^0-9&])>{1,2}\s*([^\s&|;]+)`)

func head(cmd string) string {
	f := strings.Fields(cmd)
	n := 1
	if len(f) > 1 && (f[0] == "git" || f[0] == "go" || f[0] == "npm" || f[0] == "pnpm" || f[0] == "yarn" || f[0] == "cargo" ||
		f[0] == "make" || f[0] == "docker" || f[0] == "gh" || f[0] == "python" || f[0] == "python3" || f[0] == "bun" || f[0] == "deno") {
		n = 2
		if len(f) > 2 && (f[1] == "run" || f[1] == "-m") {
			n = 3
		}
	}
	return strings.Join(f[:min(n, len(f))], " ")
}

func inside(cwd, p string) bool {
	if cwd == "" || p == "" {
		return false
	}
	if p == "/dev/null" {
		return true
	}
	if !filepath.IsAbs(p) {
		if strings.HasPrefix(p, "~") {
			return false
		}
		p = filepath.Join(cwd, p)
	}
	rel, err := filepath.Rel(filepath.Clean(cwd), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// Classify applies the tier rules to one tool call.
func Classify(tool string, input map[string]any, cwd string) Classification {
	str := func(k string) string { v, _ := input[k].(string); return v }
	switch {
	case readOnlyTools[tool]:
		p := str("file_path")
		if p == "" {
			p = str("path")
		}
		if p != "" && sensitivePath.MatchString(p) {
			return Classification{Never, tool + ":sensitive", tool + " " + p, "reads a file that may hold secrets"}
		}
		if p != "" && !inside(cwd, p) {
			return Classification{Never, tool + ":outside", tool + " " + p, "outside the project"}
		}
		return Classification{Eligible, tool, strings.TrimSpace(tool + " " + p + " " + str("pattern")), "read-only"}

	case editTools[tool]:
		p := str("file_path")
		if p == "" {
			p = str("notebook_path")
		}
		if p == "" || !inside(cwd, p) {
			return Classification{Never, tool + ":outside", tool + " " + p, "writes outside the project"}
		}
		if sensitivePath.MatchString(p) {
			return Classification{Never, tool + ":sensitive", tool + " " + p, "writes a sensitive file"}
		}
		ext := filepath.Ext(p)
		if ext == "" {
			ext = filepath.Base(p)
		}
		return Classification{Eligible, tool + ":*" + ext, tool + " " + p, "edits a project file, reversible with version control"}

	case tool == "Bash":
		line := strings.TrimSpace(str("command"))
		parts, ok := splitCommand(line)
		if !ok {
			return Classification{Unknown, "Bash:complex", line, "uses substitution, background jobs or multiple lines the rules cannot inspect"}
		}
		tier, cats, why := Eligible, []string{}, "tests, builds or read-only commands"
		for _, p := range parts {
			for envAssign.MatchString(p) {
				p = envAssign.ReplaceAllString(p, "")
			}
			for _, m := range redirect.FindAllStringSubmatch(p, -1) {
				if !inside(cwd, strings.Trim(m[2], `"'`)) {
					return Classification{Never, "Bash:redirect", line, "writes outside the project"}
				}
			}
			for _, n := range neverCommand {
				if n.re.MatchString(p) {
					return Classification{Never, "Bash:" + head(p), line, n.why}
				}
			}
			for _, tok := range strings.Fields(p)[1:] {
				tok = strings.Trim(tok, `"'`)
				if strings.HasPrefix(tok, "-") || !(strings.ContainsAny(tok, "/~") || strings.HasPrefix(tok, ".")) {
					continue
				}
				if sensitivePath.MatchString(tok) {
					return Classification{Never, "Bash:sensitive", line, "touches a file that may hold secrets"}
				}
				if (filepath.IsAbs(tok) || strings.HasPrefix(tok, "~")) && !inside(cwd, tok) {
					return Classification{Never, "Bash:outside", line, "touches paths outside the project"}
				}
			}
			if strings.HasPrefix(p, "find") && regexp.MustCompile(`\s-(delete|exec|execdir|ok|fprint)\b`).MatchString(p) {
				return Classification{Never, "Bash:find", line, "find that deletes or runs commands"}
			}
			matched := false
			for _, e := range eligibleCommand {
				if e.MatchString(p) {
					matched = true
					break
				}
			}
			if !matched {
				tier, why = Unknown, "a command the rules do not recognize: "+head(p)
			}
			cats = append(cats, head(p))
		}
		return Classification{tier, "Bash:" + strings.Join(cats, " + "), line, why}

	case tool == "WebFetch" || tool == "WebSearch":
		return Classification{Never, tool, tool + " " + str("url") + str("query"), "network access"}

	case strings.HasPrefix(tool, "mcp__"):
		return Classification{Unknown, tool, tool, "MCP tools are not classified yet"}
	}
	return Classification{Unknown, tool, tool, "an unrecognized tool"}
}
