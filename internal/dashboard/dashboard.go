// Package dashboard is Engram's local web dashboard and static HTML report
// (milestone M10; design, "Dashboard"). It shows the mind's state (health, the
// graph, the last sleep cycle, triggers, sessions, agents, permission agreement)
// and holds the approvals inbox: distilled facts to keep or reject and conflicts
// to resolve.
//
// It is served by the user's own engram process on loopback. Every API request
// must carry the random token printed at start in an X-Engram-Token header, and
// the Host header must be a loopback name, so other websites can neither read it
// (DNS rebinding) nor change it (cross-site requests cannot set that header
// without a CORS preflight, which is never granted).
package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/zakeripouya/EngramContextProtocol/internal/bootstrap"
	"github.com/zakeripouya/EngramContextProtocol/internal/health"
	"github.com/zakeripouya/EngramContextProtocol/internal/permit"
	"github.com/zakeripouya/EngramContextProtocol/internal/sleep"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

//go:embed ui
var uiFS embed.FS

// NewToken returns a random access token.
func NewToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Handler serves the dashboard UI and its API.
func Handler(st *store.Store, token string) http.Handler {
	ui, _ := fs.Sub(uiFS, "ui")
	api := http.NewServeMux()
	a := &app{st: st}
	api.HandleFunc("GET /api/overview", a.overview)
	api.HandleFunc("GET /api/inbox", a.inbox)
	api.HandleFunc("POST /api/inbox/review", a.review)
	api.HandleFunc("POST /api/inbox/resolve", a.resolve)
	api.HandleFunc("GET /api/graph", a.graph)
	api.HandleFunc("GET /api/memories", a.memories)
	api.HandleFunc("GET /api/memory/{id}", a.memory)
	api.HandleFunc("GET /api/sleep", a.sleepRuns)
	api.HandleFunc("POST /api/sleep/undo", a.undo)
	api.HandleFunc("GET /api/health", a.health)
	api.HandleFunc("POST /api/selftest", a.selftest)
	api.HandleFunc("GET /api/triggers", a.triggers)
	api.HandleFunc("POST /api/triggers/{name}/{action}", a.triggerAction)
	api.HandleFunc("GET /api/sessions", a.sessions)
	api.HandleFunc("GET /api/clients", a.clients)
	api.HandleFunc("POST /api/clients/{name}/revoke", a.revoke)
	api.HandleFunc("GET /api/permits", a.permits)
	api.HandleFunc("GET /api/packs", a.packs)
	api.HandleFunc("POST /api/packs/{name}/remove", a.removePack)
	api.HandleFunc("POST /api/packs/{name}/install", a.installBuiltin)
	api.HandleFunc("GET /api/export", a.export)

	mux := http.NewServeMux()
	mux.Handle("/api/", guard(token, api))
	mux.Handle("/", http.FileServerFS(ui))
	return secureHeaders(mux)
}

func secureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func guard(token string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			writeErr(w, http.StatusForbidden, errors.New("the dashboard only answers on localhost"))
			return
		}
		got := r.Header.Get("X-Engram-Token")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("missing or wrong token; open the URL printed by engram dashboard"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		h.ServeHTTP(w, r)
	})
}

type app struct{ st *store.Store }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (a *app) send(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, v)
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// Overview is the dashboard's first page.
type Overview struct {
	Stats      store.Stats            `json:"stats"`
	Namespaces []store.NamespaceCount `json:"namespaces"`
	Health     health.Report          `json:"health"`
	LastSleep  *sleep.Report          `json:"lastSleep,omitempty"`
	Inbox      int                    `json:"inbox"`
	Sessions   int                    `json:"openSessions"`
	Triggers   int                    `json:"triggers"`
	Failures   int                    `json:"recentFailures"`
	Clients    int                    `json:"clients"`
}

// BuildOverview gathers the overview (shared with the static report).
func BuildOverview(st *store.Store) (Overview, error) {
	var o Overview
	var err error
	if o.Stats, err = st.Stats(); err != nil {
		return o, err
	}
	if o.Namespaces, err = st.NamespaceCounts(); err != nil {
		return o, err
	}
	recall := -1.0
	if runs, err := st.Runs(1); err == nil && len(runs) > 0 && runs[0].Report != nil {
		var r sleep.Report
		if json.Unmarshal(runs[0].Report, &r) == nil {
			o.LastSleep = &r
			if r.SelfTest.Tested > 0 {
				recall = float64(r.SelfTest.Found) / float64(r.SelfTest.Tested)
			}
		}
	}
	if o.Health, err = health.Check(st, recall, false); err != nil {
		return o, err
	}
	if inbox, err := st.Inbox(); err == nil {
		o.Inbox = len(inbox)
	}
	if s, err := st.AllSessions("open"); err == nil {
		o.Sessions = len(s)
	}
	if ts, err := st.Triggers(); err == nil {
		o.Triggers = len(ts)
	}
	if evs, err := st.TriggerEvents("", 50); err == nil {
		for _, e := range evs {
			if e.Status == "failed" {
				o.Failures++
			}
		}
	}
	if cs, err := st.Clients(); err == nil {
		o.Clients = len(cs)
	}
	return o, nil
}

func (a *app) overview(w http.ResponseWriter, r *http.Request) {
	o, err := BuildOverview(a.st)
	a.send(w, o, err)
}

func (a *app) inbox(w http.ResponseWriter, r *http.Request) {
	items, err := a.st.Inbox()
	a.send(w, map[string]any{"items": items}, err)
}

func (a *app) review(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Approve bool   `json:"approve"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	a.send(w, map[string]bool{"ok": true}, a.st.ReviewDistilled(in.ID, in.Approve))
}

func (a *app) resolve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Keep string `json:"keep"`
		Drop string `json:"drop"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	a.send(w, map[string]bool{"ok": true}, a.st.ResolveConflict(in.Keep, in.Drop))
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func (a *app) graph(w http.ResponseWriter, r *http.Request) {
	g, err := a.st.GraphView(r.URL.Query().Get("ns"), atoi(r.URL.Query().Get("limit"), 200))
	a.send(w, g, err)
}

func (a *app) memories(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if query := strings.TrimSpace(q.Get("q")); query != "" {
		in := store.RecallInput{Query: query, K: 30, Hops: 2}
		if ns := q.Get("ns"); ns != "" {
			in.Namespaces = []string{ns}
		}
		if k := q.Get("kind"); k != "" {
			in.Kinds = []string{k}
		}
		res, err := a.st.RecallSystem(in)
		a.send(w, map[string]any{"scored": res}, err)
		return
	}
	mems, err := a.st.ListMemories(q.Get("ns"), q.Get("kind"), "", atoi(q.Get("limit"), 100))
	a.send(w, map[string]any{"memories": mems}, err)
}

func (a *app) memory(w http.ResponseWriter, r *http.Request) {
	d, err := a.st.Detail(r.PathValue("id"))
	a.send(w, d, err)
}

func (a *app) sleepRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := a.st.Runs(14)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var changes []store.Change
	if len(runs) > 0 {
		changes, err = a.st.Changes(runs[0].ID)
	}
	a.send(w, map[string]any{"runs": runs, "changes": changes}, err)
}

func (a *app) undo(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c, err := a.st.UndoChange(in.ID)
	a.send(w, c, err)
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	o, err := BuildOverview(a.st)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	raw, err := a.st.HealthHistory(30)
	var history []health.Metrics
	for _, b := range raw {
		var m health.Metrics
		if json.Unmarshal(b, &m) == nil {
			history = append(history, m)
		}
	}
	golden, _ := a.st.GoldenSuite()
	a.send(w, map[string]any{"report": o.Health, "history": history, "golden": golden}, err)
}

func (a *app) selftest(w http.ResponseWriter, r *http.Request) {
	res, err := health.SelfTest(context.WithoutCancel(r.Context()), a.st)
	a.send(w, map[string]any{"results": res}, err)
}

func (a *app) triggers(w http.ResponseWriter, r *http.Request) {
	ts, err := a.st.Triggers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	evs, err := a.st.TriggerEvents("", 50)
	a.send(w, map[string]any{"triggers": ts, "events": evs}, err)
}

func (a *app) triggerAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	if action != "pause" && action != "resume" {
		writeErr(w, http.StatusNotFound, errors.New("unknown action"))
		return
	}
	a.send(w, map[string]bool{"ok": true}, a.st.PauseTrigger(name, action == "pause"))
}

func (a *app) sessions(w http.ResponseWriter, r *http.Request) {
	s, err := a.st.AllSessions("")
	a.send(w, map[string]any{"sessions": s}, err)
}

func (a *app) clients(w http.ResponseWriter, r *http.Request) {
	c, err := a.st.Clients()
	a.send(w, map[string]any{"clients": c}, err)
}

func (a *app) revoke(w http.ResponseWriter, r *http.Request) {
	n, err := a.st.Revoke(r.PathValue("name"))
	a.send(w, map[string]int64{"revoked": n}, err)
}

func (a *app) permits(w http.ResponseWriter, r *http.Request) {
	rep, err := permit.BuildReport(a.st, 5000)
	a.send(w, rep, err)
}

// PackView is an installed or available pack with its reweighting statistics.
type PackView struct {
	store.PackInfo
	Installed bool             `json:"installed"`
	Stats     *store.PackStats `json:"stats,omitempty"`
}

func (a *app) packs(w http.ResponseWriter, r *http.Request) {
	installed, err := a.st.Packs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := []PackView{}
	have := map[string]bool{}
	for _, p := range installed {
		have[p.Name] = true
		stats, _ := a.st.PackStatsFor(p.Name)
		out = append(out, PackView{PackInfo: p, Installed: true, Stats: &stats})
	}
	for _, name := range bootstrap.BuiltinPacks() {
		if have[name] {
			continue
		}
		if p, err := bootstrap.LoadPack(name); err == nil {
			out = append(out, PackView{PackInfo: store.PackInfo{Name: p.Name, Version: p.Version, Kind: "template",
				Source: "builtin", Verified: true, Description: p.Description}})
		}
	}
	writeJSON(w, map[string]any{"packs": out})
}

func (a *app) removePack(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok, _ := a.st.InstalledPack(name); !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no installed pack %q", name))
		return
	}
	n, err := a.st.RemoveNamespace("pack/" + name)
	if err == nil {
		err = a.st.ForgetPack(name)
	}
	a.send(w, map[string]int{"removed": n}, err)
}

// installBuiltin installs a pack shipped in the binary. Registry and file packs
// are installed from the terminal, where signature and trust decisions are made.
func (a *app) installBuiltin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !slices.Contains(bootstrap.BuiltinPacks(), name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("only built-in packs install from the dashboard; use engram pack install for others"))
		return
	}
	p, err := bootstrap.LoadPack(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	res, err := bootstrap.InstallPack(a.st, p)
	a.send(w, res, err)
}

func (a *app) export(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := bootstrap.Export(a.st, q.Get("ns"), bootstrap.ExportOptions{Name: q.Get("name"), Version: q.Get("version")})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.json"`, p.Name, p.Version))
	b, _ := json.MarshalIndent(p, "", "  ")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(b, '\n'))
}
