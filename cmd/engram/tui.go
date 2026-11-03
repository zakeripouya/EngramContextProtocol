package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/zakeripouya/EngramContextProtocol/internal/bootstrap"
	"github.com/zakeripouya/EngramContextProtocol/internal/hosts"
	"github.com/zakeripouya/EngramContextProtocol/internal/sleep"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// The interactive setup: one full-screen flow (welcome, models, agents,
// projects, extras, review, apply, done) over the same setupChoices and
// applySetup that the plain prompts use.

type screen int

const (
	scWelcome screen = iota
	scModels
	scAgents
	scProjects
	scExtras
	scReview
	scApply
	scDone
)

var crumbs = []struct {
	s    screen
	name string
}{{scModels, "Models"}, {scAgents, "Agents"}, {scProjects, "Projects"}, {scExtras, "Extras"}, {scReview, "Review"}, {scApply, "Absorb"}}

var (
	cAccent  = lipgloss.Color("#a78bfa")
	cPink    = lipgloss.Color("#f0abfc")
	cGood    = lipgloss.Color("#4ade80")
	cWarn    = lipgloss.Color("#fbbf24")
	cBad     = lipgloss.Color("#f87171")
	cDim     = lipgloss.AdaptiveColor{Light: "#8b8b9a", Dark: "#6b6b7b"}
	cText    = lipgloss.AdaptiveColor{Light: "#1f1f29", Dark: "#e4e4ee"}
	sTitle   = lipgloss.NewStyle().Bold(true).Foreground(cText)
	sAccent  = lipgloss.NewStyle().Foreground(cAccent)
	sPink    = lipgloss.NewStyle().Foreground(cPink)
	sBold    = lipgloss.NewStyle().Bold(true).Foreground(cText)
	sDim     = lipgloss.NewStyle().Foreground(cDim)
	sGood    = lipgloss.NewStyle().Foreground(cGood)
	sWarn    = lipgloss.NewStyle().Foreground(cWarn)
	sBad     = lipgloss.NewStyle().Foreground(cBad)
	sKey     = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	sCursor  = lipgloss.NewStyle().Foreground(cPink).Bold(true)
	sCard    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#5b3fa8")).Padding(0, 1)
	sChip    = lipgloss.NewStyle().Foreground(lipgloss.Color("#1e1530")).Background(cAccent).Padding(0, 1).Bold(true)
	nsRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)
)

type (
	tickMsg   struct{}
	modelsMsg modelInfo
	scanMsg   struct {
		root  string
		plans []bootstrap.RepoPlan
		err   error
	}
	eventMsg setupEvent
	statsMsg store.Stats
	doneMsg  struct {
		sum setupSummary
		err error
	}
)

type tuiModel struct {
	ctx        context.Context
	st         *store.Store
	exe, home  string
	absorbOpts bootstrap.AbsorbOptions

	w, h   int
	screen screen
	frame  int
	net    *neuralNet
	spin   spinner.Model
	bar    progress.Model

	models *modelInfo

	hosts   []hosts.Host
	hostSel []bool
	hostCur int

	root        textinput.Model
	editingRoot bool
	scanning    bool
	scanErr     string
	plans       []bootstrap.RepoPlan
	sel         []bool
	cur         int
	ns          textinput.Model
	editingNS   bool
	nsErr       string

	hours        textinput.Model
	editingHours bool
	hoursErr     string
	scheduler    bool
	pack         bool
	hooks        bool
	extraCur     int

	events   chan tea.Msg
	log      []setupEvent
	live     *setupEvent
	stats    store.Stats
	base     store.Stats
	summary  setupSummary
	applyErr error

	Applied bool // setup ran (to completion or error); false when the user quit first
}

func newTUI(ctx context.Context, st *store.Store, exe, home, scanRoot, sleepHours string, hooks bool, o bootstrap.AbsorbOptions) *tuiModel {
	m := &tuiModel{ctx: ctx, st: st, exe: exe, home: home, absorbOpts: o, net: newNeuralNet(time.Now().UnixNano()),
		scheduler: true, pack: true, hooks: hooks, w: 80, h: 24}
	m.spin = spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(sAccent))
	m.bar = progress.New(progress.WithGradient("#7c3aed", "#f0abfc"), progress.WithoutPercentage())
	m.bar.Width = 28
	for _, h := range hosts.Detect(home) {
		m.hosts = append(m.hosts, h)
		m.hostSel = append(m.hostSel, h.Found)
	}
	m.root = textinput.New()
	m.root.Prompt = ""
	m.root.SetValue(tildePath(scanRoot))
	m.root.CharLimit = 512
	m.ns = textinput.New()
	m.ns.Prompt = ""
	m.ns.CharLimit = 64
	m.hours = textinput.New()
	m.hours.Prompt = ""
	m.hours.SetValue(sleepHours)
	m.hours.CharLimit = 32
	m.base, _ = st.Stats()
	m.stats = m.base
	return m
}

func (m *tuiModel) Init() tea.Cmd {
	return tea.Batch(tick(), m.spin.Tick, m.detectCmd(), m.scanCmd(m.root.Value()))
}

func tick() tea.Cmd {
	return tea.Tick(70*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *tuiModel) detectCmd() tea.Cmd {
	return func() tea.Msg { return modelsMsg(detectModels(m.ctx)) }
}

func (m *tuiModel) scanCmd(root string) tea.Cmd {
	m.scanning, m.scanErr = true, ""
	return func() tea.Msg {
		repos, err := bootstrap.FindRepos(expandTilde(root), 4)
		if err != nil {
			return scanMsg{root: root, err: err}
		}
		return scanMsg{root: root, plans: bootstrap.PlanRepos(m.ctx, m.st, repos, m.absorbOpts)}
	}
}

func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return strings.Replace(p, "~", homeDir(), 1)
	}
	return p
}

func (m *tuiModel) waitEvent() tea.Cmd {
	return func() tea.Msg { return <-m.events }
}

func (m *tuiModel) statsCmd() tea.Cmd {
	return func() tea.Msg {
		s, _ := m.st.Stats()
		return statsMsg(s)
	}
}

// choices turns what is on screen into setupChoices.
func (m *tuiModel) choices() setupChoices {
	ch := setupChoices{SleepHours: m.hours.Value(), Scheduler: m.scheduler, DeveloperPack: m.pack, Absorb: m.absorbOpts}
	for i, h := range m.hosts {
		if m.hostSel[i] && h.Found {
			ch.Hosts = append(ch.Hosts, h)
		}
	}
	for i, p := range m.plans {
		if m.sel[i] {
			ch.Repos = append(ch.Repos, p)
		}
	}
	ch.Hooks = m.hooks && m.claudeCodeSelected()
	return ch
}

func (m *tuiModel) claudeCodeSelected() bool {
	for i, h := range m.hosts {
		if h.Name == "claude-code" && h.Found && m.hostSel[i] {
			return true
		}
	}
	return false
}

func (m *tuiModel) startApply() tea.Cmd {
	m.screen = scApply
	m.events = make(chan tea.Msg, 256)
	ch := m.choices()
	go func() {
		sum, err := applySetup(m.ctx, m.st, m.exe, m.home, ch, func(e setupEvent) { m.events <- eventMsg(e) })
		m.events <- doneMsg{sum: sum, err: err}
	}()
	return m.waitEvent()
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.bar.Width = max(10, min(36, m.w-52))
		return m, nil
	case tickMsg:
		m.frame++
		m.net.Step()
		if m.screen != scApply && m.frame%22 == 0 {
			m.net.Fire(-1, 1)
		}
		cmds := []tea.Cmd{tick()}
		if m.screen == scApply && m.frame%5 == 0 {
			cmds = append(cmds, m.statsCmd())
		}
		return m, tea.Batch(cmds...)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case modelsMsg:
		info := modelInfo(msg)
		m.models = &info
		m.st.SetEmbedder(info.embedder)
		m.net.Fire(0, 1)
		return m, nil
	case scanMsg:
		if msg.root != m.root.Value() {
			return m, nil // a newer scan is running
		}
		m.scanning = false
		if msg.err != nil {
			m.scanErr = msg.err.Error()
			m.plans, m.sel = nil, nil
			return m, nil
		}
		m.plans = msg.plans
		m.sel = make([]bool, len(m.plans))
		for i, p := range m.plans {
			m.sel[i] = recentlyActive(p)
		}
		m.cur = 0
		return m, nil
	case eventMsg:
		e := setupEvent(msg)
		switch e.Kind {
		case "progress":
			m.live = &e
			if e.Done%20 == 0 {
				m.net.Fire(-1, 0.9)
			}
		default:
			m.live = nil
			m.log = append(m.log, e)
			m.net.Fire(-1, 1)
		}
		return m, m.waitEvent()
	case statsMsg:
		m.stats = store.Stats(msg)
		return m, nil
	case doneMsg:
		m.summary, m.applyErr, m.live = msg.sum, msg.err, nil
		m.stats, _ = m.st.Stats()
		m.Applied = true
		m.screen = scDone
		for i := 0; i < 4; i++ {
			m.net.Fire(-1, 1)
		}
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *tuiModel) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	// Text fields take every other key while they are open.
	switch {
	case m.editingRoot:
		switch k {
		case "enter":
			m.editingRoot = false
			m.root.Blur()
			return m, m.scanCmd(m.root.Value())
		case "esc":
			m.editingRoot = false
			m.root.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.root, cmd = m.root.Update(msg)
		return m, cmd
	case m.editingNS:
		switch k {
		case "enter":
			v := strings.TrimSpace(m.ns.Value())
			if !nsRegexp.MatchString(v) || v == "me" {
				m.nsErr = "lowercase letters, digits, . _ - and / only (and not \"me\")"
				return m, nil
			}
			m.plans[m.cur].Namespace = v
			m.editingNS, m.nsErr = false, ""
			m.ns.Blur()
			return m, nil
		case "esc":
			m.editingNS, m.nsErr = false, ""
			m.ns.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.ns, cmd = m.ns.Update(msg)
		return m, cmd
	case m.editingHours:
		switch k {
		case "enter":
			if _, err := sleep.ParseHours(m.hours.Value(), ""); err != nil {
				m.hoursErr = err.Error()
				return m, nil
			}
			m.editingHours, m.hoursErr = false, ""
			m.hours.Blur()
			return m, nil
		case "esc":
			m.editingHours, m.hoursErr = false, ""
			m.hours.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.hours, cmd = m.hours.Update(msg)
		return m, cmd
	}

	back := func() {
		if m.screen > scWelcome && m.screen <= scReview {
			m.screen--
		}
	}
	switch m.screen {
	case scWelcome:
		switch k {
		case "enter", " ":
			m.screen = scModels
		case "q", "esc":
			return m, tea.Quit
		}
	case scModels:
		switch k {
		case "enter":
			if m.models != nil {
				m.screen = scAgents
			}
		case "esc":
			back()
		case "q":
			return m, tea.Quit
		}
	case scAgents:
		switch k {
		case "up", "k":
			m.hostCur = max(0, m.hostCur-1)
		case "down", "j":
			m.hostCur = min(len(m.hosts)-1, m.hostCur+1)
		case " ", "x":
			if len(m.hosts) > 0 && m.hosts[m.hostCur].Found {
				m.hostSel[m.hostCur] = !m.hostSel[m.hostCur]
			}
		case "enter":
			m.screen = scProjects
		case "esc":
			back()
		case "q":
			return m, tea.Quit
		}
	case scProjects:
		switch k {
		case "up", "k":
			m.cur = max(0, m.cur-1)
		case "down", "j":
			m.cur = min(len(m.plans)-1, m.cur+1)
		case "pgup":
			m.cur = max(0, m.cur-8)
		case "pgdown":
			m.cur = min(len(m.plans)-1, m.cur+8)
		case " ", "x":
			if len(m.plans) > 0 {
				m.sel[m.cur] = !m.sel[m.cur]
			}
		case "a":
			all := true
			for _, s := range m.sel {
				all = all && s
			}
			for i := range m.sel {
				m.sel[i] = !all
			}
		case "e", "n":
			if len(m.plans) > 0 {
				m.editingNS = true
				m.ns.SetValue(m.plans[m.cur].Namespace)
				m.ns.CursorEnd()
				return m, m.ns.Focus()
			}
		case "/", "f":
			m.editingRoot = true
			m.root.CursorEnd()
			return m, m.root.Focus()
		case "enter":
			if !m.scanning {
				m.screen = scExtras
			}
		case "esc":
			back()
		case "q":
			return m, tea.Quit
		}
	case scExtras:
		switch k {
		case "up", "k":
			m.extraCur = max(0, m.extraCur-1)
		case "down", "j":
			m.extraCur = min(3, m.extraCur+1)
		case " ", "x":
			switch m.extraCur {
			case 0:
				m.editingHours = true
				m.hours.CursorEnd()
				return m, m.hours.Focus()
			case 1:
				m.scheduler = !m.scheduler
			case 2:
				m.pack = !m.pack
			case 3:
				if m.claudeCodeSelected() {
					m.hooks = !m.hooks
				}
			}
		case "enter":
			m.screen = scReview
		case "esc":
			back()
		case "q":
			return m, tea.Quit
		}
	case scReview:
		switch k {
		case "enter", "y":
			return m, m.startApply()
		case "esc":
			back()
		case "q":
			return m, tea.Quit
		}
	case scDone:
		switch k {
		case "enter", "q", "esc", " ":
			return m, tea.Quit
		}
	}
	return m, nil
}

// ---- views ----

func (m *tuiModel) View() string {
	width := max(40, min(m.w, 100))
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(indent(m.net.View(), 2))
	b.WriteString("\n\n")
	b.WriteString("  " + wordmark() + sDim.Render("  shared memory for your agents") + "\n")
	if m.screen != scWelcome {
		b.WriteString("  " + m.breadcrumb() + "\n")
	}
	b.WriteString("\n")
	var body, help string
	switch m.screen {
	case scWelcome:
		body, help = m.viewWelcome(), "enter start · q quit"
	case scModels:
		body, help = m.viewModels(), "enter continue · esc back · q quit"
	case scAgents:
		body, help = m.viewAgents(), "↑↓ move · space toggle · enter continue · esc back"
	case scProjects:
		body = m.viewProjects(width)
		switch {
		case m.editingRoot:
			help = "type a folder · enter scan · esc cancel"
		case m.editingNS:
			help = "type a namespace · enter save · esc cancel"
		default:
			help = "↑↓ move · space toggle · a all · e rename · / folder · enter continue · esc back"
		}
	case scExtras:
		body, help = m.viewExtras(), "↑↓ move · space toggle or edit · enter continue · esc back"
		if m.editingHours {
			help = "e.g. 01:00-07:00 · enter save · esc cancel"
		}
	case scReview:
		body, help = m.viewReview(), "enter absorb · esc back · q quit"
	case scApply:
		body, help = m.viewApply(), "working… ctrl+c stops"
	case scDone:
		body, help = m.viewDone(), "enter exit"
	}
	b.WriteString(indent(body, 2))
	b.WriteString("\n\n  " + renderHelp(help) + "\n")
	return b.String()
}

func wordmark() string {
	colors := []string{"#7c3aed", "#8b5cf6", "#a78bfa", "#c084fc", "#e879f9", "#f0abfc"}
	var b strings.Builder
	for i, r := range "engram" {
		b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colors[i])).Render(string(r)))
	}
	return b.String()
}

func (m *tuiModel) breadcrumb() string {
	var parts []string
	for _, c := range crumbs {
		switch {
		case c.s == m.screen || (m.screen == scDone && c.s == scApply):
			parts = append(parts, sChip.Render(c.name))
		case c.s < m.screen:
			parts = append(parts, sGood.Render("✓ ")+sDim.Render(c.name))
		default:
			parts = append(parts, sDim.Render(c.name))
		}
	}
	return strings.Join(parts, sDim.Render("  ›  "))
}

func renderHelp(h string) string {
	var out []string
	for _, part := range strings.Split(h, " · ") {
		if k, rest, ok := strings.Cut(part, " "); ok {
			out = append(out, sKey.Render(k)+" "+sDim.Render(rest))
		} else {
			out = append(out, sDim.Render(part))
		}
	}
	return strings.Join(out, sDim.Render("  ·  "))
}

func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

func check(on bool) string {
	if on {
		return sAccent.Render("◉")
	}
	return sDim.Render("○")
}

func cursor(on bool) string {
	if on {
		return sCursor.Render("❯ ")
	}
	return "  "
}

func (m *tuiModel) viewWelcome() string {
	return sTitle.Render("Let's give your agents one memory.") + "\n\n" +
		"Engram will connect the agents on this machine, find your projects, and\n" +
		"absorb what they already know: git history, the rules you wrote for agents\n" +
		"(CLAUDE.md, AGENTS.md, Cursor rules), your docs and your stack.\n\n" +
		sDim.Render("Everything stays on this machine. You review every choice before anything is stored,\n"+
			"and every step can be redone or undone later.")
}

func (m *tuiModel) viewModels() string {
	if m.models == nil {
		return m.spin.View() + " Looking for an embedding server and a local chat model…"
	}
	mi := m.models
	var b strings.Builder
	b.WriteString(sTitle.Render("Models") + "\n\n")
	if mi.Hashing {
		b.WriteString(sWarn.Render("!") + " Embeddings   " + mi.Embeddings + "\n")
		b.WriteString(sDim.Render("               Recall by meaning is weak without a real embedding model.") + "\n")
		b.WriteString(sDim.Render("               Later: ollama pull nomic-embed-text, then engram reembed") + "\n")
	} else {
		b.WriteString(sGood.Render("✓") + " Embeddings   " + mi.Embeddings + "\n")
	}
	if mi.ChatOK {
		b.WriteString(sGood.Render("✓") + " Sleep cycle  " + mi.Chat + "\n")
	} else {
		b.WriteString(sWarn.Render("!") + " Sleep cycle  " + mi.Chat + "\n")
		b.WriteString(sDim.Render("               Distillation is skipped until a chat model answers (e.g. ollama pull llama3.2)") + "\n")
	}
	b.WriteString("\n" + sDim.Render("Database  ") + tildePath(dbPath()))
	return b.String()
}

func (m *tuiModel) viewAgents() string {
	var b strings.Builder
	b.WriteString(sTitle.Render("Which agents should share this memory?") + "\n")
	b.WriteString(sDim.Render("Each one gets read and write access to your personal memory and the projects you absorb.") + "\n\n")
	for i, h := range m.hosts {
		line := cursor(i == m.hostCur)
		if h.Found {
			line += check(m.hostSel[i]) + " " + sBold.Render(h.Title)
		} else {
			line += sDim.Render("· " + h.Title + "  not installed")
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *tuiModel) viewProjects(width int) string {
	var b strings.Builder
	b.WriteString(sTitle.Render("Which projects should Engram absorb?") + "\n")
	folder := m.root.Value()
	if m.editingRoot {
		folder = m.root.View()
	}
	b.WriteString(sDim.Render("Folder  ") + sAccent.Render(folder))
	if m.scanning {
		b.WriteString("  " + m.spin.View() + sDim.Render(" scanning…"))
	}
	b.WriteString("\n\n")
	if m.scanErr != "" {
		return b.String() + sBad.Render(m.scanErr) + "\n" + sDim.Render("Press / to pick another folder.")
	}
	if !m.scanning && len(m.plans) == 0 {
		return b.String() + sDim.Render("No git repositories found here. Press / to pick another folder, or enter to skip.")
	}
	if len(m.plans) == 0 {
		return strings.TrimRight(b.String(), "\n")
	}
	selected := 0
	for _, s := range m.sel {
		if s {
			selected++
		}
	}
	nameW, nsW := 6, 9
	for _, p := range m.plans {
		nameW = max(nameW, min(24, len(p.Name)))
		nsW = max(nsW, min(24, len(p.Namespace)))
	}
	head := fmt.Sprintf("    %-*s  %-*s  %7s  %5s  %s", nameW, "PROJECT", nsW, "NAMESPACE", "COMMITS", "DOCS", "STACK")
	b.WriteString(sDim.Render(head) + "\n")
	rows := max(4, min(10, m.h-28))
	start := max(0, min(m.cur-rows/2, len(m.plans)-rows))
	for i := start; i < min(len(m.plans), start+rows); i++ {
		p := m.plans[i]
		ns := p.Namespace
		if m.editingNS && i == m.cur {
			ns = m.ns.View()
		}
		stack := strings.Join(p.Stack, ", ")
		room := width - (4 + nameW + 2 + nsW + 2 + 7 + 2 + 5 + 2) - 4
		if room < 8 {
			stack = ""
		} else if len(stack) > room {
			stack = stack[:room-1] + "…"
		}
		name := truncate(p.Name, nameW)
		line := fmt.Sprintf("%-*s  ", nameW, name)
		if i == m.cur {
			line = sBold.Render(line)
		}
		nsCell := fmt.Sprintf("%-*s", nsW, truncate(ns, nsW))
		if !(m.editingNS && i == m.cur) {
			nsCell = sAccent.Render(nsCell)
		}
		docs := len(p.Rules) + len(p.Docs)
		b.WriteString(cursor(i == m.cur) + check(m.sel[i]) + " " + line + nsCell +
			fmt.Sprintf("  %7d  %5d  ", p.Commits, docs) + sDim.Render(stack) + "\n")
	}
	if len(m.plans) > rows {
		b.WriteString(sDim.Render(fmt.Sprintf("    … %d projects in all", len(m.plans))) + "\n")
	}
	if m.nsErr != "" {
		b.WriteString(sBad.Render("    "+m.nsErr) + "\n")
	}
	// Details of the highlighted project.
	p := m.plans[m.cur]
	var d strings.Builder
	d.WriteString(sBold.Render(p.Name) + sDim.Render("  "+tildePath(p.Path)) + "\n")
	last := p.LastCommit
	if last == "" {
		last = "never"
	}
	d.WriteString(fmt.Sprintf("%d commits · last %s · %d doc sections", p.Commits, last, p.Sections))
	if p.Absorbed {
		d.WriteString(sGood.Render("  · absorbed before, only changes are added"))
	}
	d.WriteString("\n")
	if len(p.Rules) > 0 {
		d.WriteString(sPink.Render("agent rules ") + strings.Join(p.Rules, ", ") + "\n")
	}
	if len(p.Docs) > 0 {
		shown := p.Docs
		more := ""
		if len(shown) > 5 {
			more = fmt.Sprintf(" +%d more", len(shown)-5)
			shown = shown[:5]
		}
		d.WriteString(sAccent.Render("docs ") + strings.Join(shown, ", ") + sDim.Render(more) + "\n")
	}
	if len(p.Stack) > 0 {
		d.WriteString(sDim.Render("stack ") + strings.Join(p.Stack, ", "))
	}
	b.WriteString("\n" + sCard.Width(min(width-6, 92)).Render(strings.TrimRight(d.String(), "\n")) + "\n")
	b.WriteString(sDim.Render(fmt.Sprintf("%d of %d selected (projects with a commit in the last year are preselected)", selected, len(m.plans))))
	return b.String()
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func (m *tuiModel) viewExtras() string {
	var b strings.Builder
	b.WriteString(sTitle.Render("A few more things") + "\n\n")
	hours := sAccent.Render(m.hours.Value())
	if m.editingHours {
		hours = m.hours.View()
	}
	items := []string{
		"Sleep hours  " + hours + sDim.Render("   when Engram consolidates overnight"),
		check(m.scheduler) + " Nightly scheduler" + sDim.Render("   runs the sleep cycle when it is due (cron or launchd)"),
		check(m.pack) + " Developer pack" + sDim.Render("   how branches, PRs, tickets and CI connect"),
	}
	if m.claudeCodeSelected() {
		items = append(items, check(m.hooks)+" Permission assist"+sDim.Render("   watch Claude Code prompts and predict your answers; never answers them"))
	} else {
		items = append(items, sDim.Render("· Permission assist   needs Claude Code"))
	}
	for i, it := range items {
		b.WriteString(cursor(i == m.extraCur) + it + "\n")
	}
	if m.hoursErr != "" {
		b.WriteString(sBad.Render("  " + m.hoursErr))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *tuiModel) viewReview() string {
	ch := m.choices()
	var b strings.Builder
	b.WriteString(sTitle.Render("Ready to absorb") + "\n\n")
	var agents []string
	for _, h := range ch.Hosts {
		agents = append(agents, h.Title)
	}
	if len(agents) == 0 {
		agents = []string{sDim.Render("none")}
	}
	b.WriteString(sAccent.Render("Agents    ") + strings.Join(agents, ", ") + "\n")
	commits, sections, rules := 0, 0, 0
	for _, p := range ch.Repos {
		commits += p.Commits
		sections += p.Sections
		rules += len(p.Rules)
	}
	if len(ch.Repos) == 0 {
		b.WriteString(sAccent.Render("Projects  ") + sDim.Render("none") + "\n")
	} else {
		b.WriteString(sAccent.Render("Projects  ") + fmt.Sprintf("%d projects · %d commits · %d doc sections · %d agent rules files\n", len(ch.Repos), commits, sections, rules))
		for i, p := range ch.Repos {
			if i == 6 {
				b.WriteString(sDim.Render(fmt.Sprintf("          … and %d more\n", len(ch.Repos)-6)))
				break
			}
			b.WriteString(sDim.Render("          "+p.Name+" → ") + sAccent.Render(p.Namespace) + "\n")
		}
	}
	extra := []string{"sleep " + ch.SleepHours}
	if ch.Scheduler {
		extra = append(extra, "nightly scheduler")
	}
	if ch.DeveloperPack {
		extra = append(extra, "developer pack")
	}
	if ch.Hooks {
		extra = append(extra, "permission assist (shadow)")
	}
	b.WriteString(sAccent.Render("Extras    ") + strings.Join(extra, " · ") + "\n\n")
	if m.models != nil && m.models.Hashing && len(ch.Repos) > 0 {
		b.WriteString(sWarn.Render("!") + sDim.Render(" No embedding server: memories are stored now and can be re-embedded later with engram reembed.") + "\n")
	}
	b.WriteString(sDim.Render("Imported memories start weak and only gain strength when they help. Secrets are filtered out."))
	return b.String()
}

func (m *tuiModel) counters() string {
	mem := m.stats.Memories - m.base.Memories
	edges := m.stats.Edges - m.base.Edges
	return sPink.Render(fmt.Sprintf("%6d", mem)) + sDim.Render(" memories   ") +
		sPink.Render(fmt.Sprintf("%6d", edges)) + sDim.Render(" pathways")
}

func (m *tuiModel) renderEvent(e setupEvent) string {
	icon := sGood.Render("✓")
	switch e.Kind {
	case "warn":
		icon = sWarn.Render("!")
	case "fail":
		icon = sBad.Render("✗")
	case "step":
		return sBold.Render(e.Step)
	}
	item := ""
	if e.Item != "" {
		item = sBold.Render(e.Item) + "  "
	}
	return "  " + icon + " " + item + sDim.Render(e.Msg)
}

func (m *tuiModel) viewApply() string {
	var b strings.Builder
	b.WriteString(m.counters() + "\n\n")
	logs := m.log
	maxLines := max(6, m.h-24)
	if len(logs) > maxLines {
		logs = logs[len(logs)-maxLines:]
	}
	for _, e := range logs {
		b.WriteString(m.renderEvent(e) + "\n")
	}
	if e := m.live; e != nil && e.Total > 0 {
		pct := float64(e.Done) / float64(e.Total)
		b.WriteString("  " + m.spin.View() + " " + sBold.Render(fmt.Sprintf("%-14s", truncate(e.Item, 14))) +
			sDim.Render(fmt.Sprintf("%-8s", e.Msg)) + m.bar.ViewAs(pct) + sDim.Render(fmt.Sprintf(" %d/%d", e.Done, e.Total)) + "\n")
	} else {
		b.WriteString("  " + m.spin.View() + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *tuiModel) viewDone() string {
	var b strings.Builder
	if m.applyErr != nil {
		b.WriteString(sBad.Render("Setup stopped: ") + m.applyErr.Error() + "\n\n")
	} else {
		b.WriteString(sTitle.Render("Your agents share one memory now.") + "\n\n")
	}
	s := m.summary
	b.WriteString(sPink.Render(fmt.Sprintf("%d", s.Memories)) + sDim.Render(" memories  ·  ") +
		sPink.Render(fmt.Sprintf("%d", s.Pathways)) + sDim.Render(" pathways  ·  ") +
		sPink.Render(fmt.Sprintf("%d", len(s.Namespaces))) + sDim.Render(pluralWord(len(s.Namespaces), " project", " projects")+"  ·  ") +
		sPink.Render(fmt.Sprintf("%d", len(s.Connected))) + sDim.Render(pluralWord(len(s.Connected), " agent", " agents")) +
		sDim.Render(fmt.Sprintf("  ·  %.1fs", s.Elapsed.Seconds())) + "\n")
	if s.Secrets > 0 {
		b.WriteString(sWarn.Render("!") + sDim.Render(" "+plural(s.Secrets, "doc section was", "doc sections were")+" skipped because it looked like a secret.") + "\n")
	}
	if s.Warnings > 0 {
		b.WriteString(sWarn.Render("!") + sDim.Render(" "+plural(s.Warnings, "step needs", "steps need")+" a look:") + "\n")
		for _, e := range m.log {
			if e.Kind == "warn" {
				b.WriteString(m.renderEvent(e) + "\n")
			}
		}
	}
	b.WriteString("\n" + sBold.Render("Next") + "\n")
	if len(s.Connected) > 0 {
		b.WriteString(sAccent.Render("  1 ") + "Restart your agents, then ask one: \"what do you know about this project?\"\n")
	} else {
		b.WriteString(sAccent.Render("  1 ") + "Connect an agent: engram setup claude-code\n")
	}
	b.WriteString(sAccent.Render("  2 ") + "engram dashboard    see the map, the inbox and health\n")
	b.WriteString(sAccent.Render("  3 ") + "engram absorb DIR   absorb more projects any time (it only adds what changed)")
	return b.String()
}

// runTUI shows the interactive setup. It reports whether setup was applied.
func runTUI(ctx context.Context, st *store.Store, exe, home, scanRoot, sleepHours string, hooks bool, o bootstrap.AbsorbOptions) (bool, error) {
	m := newTUI(ctx, st, exe, home, scanRoot, sleepHours, hooks, o)
	final, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	if err != nil {
		return false, err
	}
	fm := final.(*tuiModel)
	if fm.Applied {
		// Leave a plain record in the scrollback once the screen closes.
		fmt.Println(wordmark() + " setup done")
		for _, e := range fm.log {
			fmt.Println(m.renderEvent(e))
		}
		s := fm.summary
		fmt.Printf("\n%d memories and %d pathways added in %.1fs. Next: restart your agents, then engram dashboard.\n",
			s.Memories, s.Pathways, s.Elapsed.Seconds())
		return true, fm.applyErr
	}
	if fm.screen == scApply {
		fmt.Println("Setup interrupted. What finished is kept; run engram init again to finish (it only adds what is missing).")
		return false, nil
	}
	fmt.Println("Setup cancelled; nothing was changed.")
	return false, nil
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
