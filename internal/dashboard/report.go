package dashboard

import (
	"html/template"
	"io"
	"strconv"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/permit"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// The static report is a read-only daily snapshot: one self-contained HTML file
// with no scripts, regenerated after each sleep cycle and on demand.

type reportData struct {
	Generated string
	O         Overview
	Inbox     []store.InboxItem
	Triggers  []store.Trigger
	Events    []store.TriggerEvent
	Sessions  []store.Checkpoint
	Permits   permit.Report
}

// WriteReport renders the static HTML report.
func WriteReport(st *store.Store, w io.Writer) error {
	var d reportData
	var err error
	d.Generated = store.Now().Local().Format("Mon 2 Jan 2006, 15:04")
	if d.O, err = BuildOverview(st); err != nil {
		return err
	}
	if d.Inbox, err = st.Inbox(); err != nil {
		return err
	}
	if d.Triggers, err = st.Triggers(); err != nil {
		return err
	}
	if d.Events, err = st.TriggerEvents("", 15); err != nil {
		return err
	}
	if d.Sessions, err = st.AllSessions("open"); err != nil {
		return err
	}
	if d.Permits, err = permit.BuildReport(st, 5000); err != nil {
		return err
	}
	return reportTmpl.Execute(w, d)
}

var reportTmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"date": func(s string) string {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return s
		}
		return t.Local().Format("2 Jan 15:04")
	},
	"pct": fmtPct,
	"f2":  func(x float64) string { return fmtF(x) },
	"na":  func(s string) bool { return s == "not yet measurable" },
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Engram Report</title>
<style>
:root { --bg:#f6f6f3; --panel:#fff; --ink:#1b1c1f; --muted:#6b6e76; --line:#e3e3de; --accent:#5b4ef0; --ok:#1f8a5b; --warn:#b7791f; --bad:#c2413b; color-scheme: light; }
@media (prefers-color-scheme: dark) { :root { --bg:#111215; --panel:#1a1b1f; --ink:#ececef; --muted:#9a9ca4; --line:#2b2d33; --accent:#8f86ff; --ok:#4cc38a; --warn:#e0a640; --bad:#ef6b63; color-scheme: dark; } }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--ink); font:14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Inter, system-ui, sans-serif; }
main { max-width:960px; margin:0 auto; padding:32px 16px 48px; }
h1 { font-size:22px; margin:0; } h2 { font-size:13px; color:var(--muted); text-transform:uppercase; letter-spacing:.6px; margin:28px 0 10px; }
.lede { color:var(--muted); margin:4px 0 20px; }
.grid { display:grid; gap:12px; grid-template-columns:repeat(auto-fill, minmax(160px,1fr)); }
.tile, .card { background:var(--panel); border:1px solid var(--line); border-radius:10px; padding:14px 16px; }
.k { color:var(--muted); font-size:12px; } .v { font-size:24px; font-weight:650; font-variant-numeric:tabular-nums; }
.card + .card { margin-top:10px; } .meta { color:var(--muted); font-size:12px; }
table { width:100%; border-collapse:collapse; background:var(--panel); border:1px solid var(--line); border-radius:10px; overflow:hidden; }
th, td { text-align:left; padding:8px 12px; border-bottom:1px solid var(--line); vertical-align:top; } th { font-size:12px; color:var(--muted); }
tr:last-child td { border-bottom:0; } .num { text-align:right; font-variant-numeric:tabular-nums; }
.wrap { overflow-x:auto; }
.pill { display:inline-block; padding:0 8px; border-radius:10px; font-size:12px; border:1px solid var(--line); color:var(--muted); }
.ok { color:var(--ok); } .warn { color:var(--warn); } .breach, .failed { color:var(--bad); }
.mono { font-family:ui-monospace, SFMono-Regular, Menlo, monospace; font-size:12px; }
</style>
</head>
<body><main>
<h1>Engram report</h1>
<p class="lede">Generated {{.Generated}} · read-only snapshot · open <span class="mono">engram dashboard</span> to act on it</p>

<div class="grid">
  <div class="tile"><div class="k">Memories</div><div class="v">{{.O.Stats.Current}}</div><div class="meta">{{len .O.Namespaces}} namespaces</div></div>
  <div class="tile"><div class="k">Pathways</div><div class="v">{{.O.Stats.Edges}}</div></div>
  <div class="tile"><div class="k">Health</div><div class="v {{.O.Health.Worst}}">{{.O.Health.Worst}}</div></div>
  <div class="tile"><div class="k">Inbox</div><div class="v">{{.O.Inbox}}</div><div class="meta">waiting for you</div></div>
  <div class="tile"><div class="k">Open tasks</div><div class="v">{{.O.Sessions}}</div></div>
  <div class="tile"><div class="k">Trigger failures</div><div class="v">{{.O.Failures}}</div><div class="meta">in the last 50 events</div></div>
</div>

<h2>Last sleep cycle</h2>
{{with .O.LastSleep}}<div class="card">
  <div class="meta">{{date .FinishedAt}} · {{if .Light}}light{{else}}full{{end}} cycle</div>
  <div>Merged {{.Merged}}, archived {{.Archived}}, downscaled {{.Downscaled}}, pruned {{.Pruned}} pathways, distilled {{len .Distilled}} facts from {{.Hubs}} hubs.</div>
  {{if .SelfTest.Tested}}<div>Recall self-test: found {{.SelfTest.Found}} of {{.SelfTest.Tested}} of your decisions.</div>{{end}}
  {{range .Notes}}<div class="meta">{{.}}</div>{{end}}
</div>
{{range .Distilled}}<div class="card"><div class="meta">Distilled</div><div>{{.Content}}</div></div>{{end}}
{{else}}<div class="card meta">No sleep cycle yet.</div>{{end}}

<h2>Health</h2>
<div class="wrap"><table><tr><th>Status</th><th>Bound</th><th class="num">Value</th><th>Healthy when</th></tr>
{{range .O.Health.Bounds}}<tr><td><span class="pill {{.Status}}">{{if na .Status}}n/a{{else}}{{.Status}}{{end}}</span></td><td>{{.Name}}</td><td class="num">{{if na .Status}}-{{else}}{{f2 .Value}}{{end}}</td><td>{{.Limit}}</td></tr>{{end}}
{{range .O.Health.Drift}}<tr><td><span class="pill {{.Status}}">{{.Status}}</span></td><td>{{.Name}}</td><td class="num">{{f2 .Value}}</td><td>{{.Limit}}</td></tr>{{end}}
</table></div>

<h2>Waiting for you ({{len .Inbox}})</h2>
{{range .Inbox}}<div class="card"><div class="meta">{{.Kind}} · {{date .At}}</div><div>{{.Title}}</div>{{if eq .Kind "conflict"}}{{range .Sources}}<div class="meta">· {{.Content}}</div>{{end}}{{end}}</div>
{{else}}<div class="card meta">Nothing to review.</div>{{end}}

<h2>Open tasks</h2>
{{range .Sessions}}<div class="card"><div class="meta">{{.Namespace}} · {{.LastClient}} · {{date .UpdatedAt}}</div><div>{{.Goal}}</div>{{if .Next}}<div class="meta">Next: {{.Next}}</div>{{end}}</div>
{{else}}<div class="card meta">No open tasks.</div>{{end}}

<h2>Triggers</h2>
{{if .Triggers}}<div class="wrap"><table><tr><th>Trigger</th><th>Kind</th><th class="num">Fired</th><th class="num">Failed</th><th>Last</th></tr>
{{range .Triggers}}<tr><td>{{.Name}}{{if .Paused}} <span class="pill">paused</span>{{end}}</td><td>{{.Kind}}</td><td class="num">{{.Fired}}</td><td class="num">{{.Failed}}</td><td>{{date .LastFiredAt}}</td></tr>{{end}}
</table></div>
{{if .Events}}<div class="wrap" style="margin-top:10px"><table><tr><th>When</th><th>Trigger</th><th>Status</th><th>Summary</th></tr>
{{range .Events}}<tr><td>{{date .At}}</td><td>{{.Trigger}}</td><td><span class="pill {{.Status}}">{{.Status}}</span></td><td>{{.Summary}}</td></tr>{{end}}
</table></div>{{end}}
{{else}}<div class="card meta">No triggers.</div>{{end}}

<h2>Permission assist (shadow mode)</h2>
<div class="wrap"><table><tr><th>Tier</th><th class="num">Requests</th><th class="num">Approved</th><th class="num">Denied</th><th class="num">Would allow</th><th class="num">Wrong allows</th><th>Ready?</th></tr>
{{range .Permits.Tiers}}<tr><td>{{.Tier}}</td><td class="num">{{.Requests}}</td><td class="num">{{.Approved}}</td><td class="num">{{.Denied}}</td><td class="num">{{.PredictedAllow}}</td><td class="num">{{.AllowDenied}}</td><td>{{.ReadinessReason}}</td></tr>{{end}}
</table></div>
</main></body></html>
`))

func fmtPct(x float64) string { return strconv.FormatFloat(x*100, 'f', 0, 64) + "%" }
func fmtF(x float64) string   { return strconv.FormatFloat(x, 'f', 2, 64) }
