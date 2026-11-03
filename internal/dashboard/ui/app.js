"use strict";
// Engram dashboard. Everything from the API is inserted as text, never as HTML.

const token = (() => {
  const m = location.hash.match(/token=([0-9a-f]+)/);
  if (m) {
    try { sessionStorage.setItem("engram-token", m[1]); } catch (e) {}
    history.replaceState(null, "", location.pathname);
    return m[1];
  }
  try { return sessionStorage.getItem("engram-token") || ""; } catch (e) { return ""; }
})();

async function api(path, body) {
  const opts = { headers: { "X-Engram-Token": token } };
  if (body !== undefined) {
    opts.method = "POST";
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch("/api/" + path, opts);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

// put replaces an element's children, skipping null, undefined and false
// (the DOM's replaceChildren would render them as text).
function put(el, ...kids) {
  el.replaceChildren(...kids.flat().filter((k) => k !== null && k !== undefined && k !== false));
}

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "style") el.style.cssText = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === undefined || c === null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

const fmtDate = (s) => {
  if (!s) return "";
  const d = new Date(s);
  return isNaN(d) ? s : d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
};
const pct = (x) => (x * 100).toFixed(0) + "%";
const kindColor = (k) => `var(--${["episode", "fact", "decision", "suggestion", "concept", "trait", "procedure"].includes(k) ? k : "muted"})`;
const kindTag = (k) => h("span", { class: "kind", style: `--k:${kindColor(k)}` }, k);
const pill = (text, cls) => h("span", { class: "pill " + (cls || text) }, text);

function toast(msg) {
  const t = h("div", { class: "toast", role: "status" }, msg);
  document.body.append(t);
  setTimeout(() => t.remove(), 2600);
}

function table(headers, rows, numeric = []) {
  return h("div", { class: "table-wrap" }, h("table", {},
    h("thead", {}, h("tr", {}, headers.map((x, i) => h("th", { class: numeric.includes(i) ? "num" : null }, x)))),
    h("tbody", {}, rows.map((r) => h("tr", {}, r.map((c, i) => h("td", { class: numeric.includes(i) ? "num" : null }, c)))))));
}

function memRow(m, extra) {
  return h("div", { class: "mem", onclick: () => openMemory(m.id) },
    h("div", { class: "row" }, kindTag(m.kind), h("span", { class: "muted mono" }, m.namespace), extra || null),
    h("div", { class: "content" }, m.content),
    h("div", { class: "meta muted" }, `${m.provenance?.author || ""} · ${m.provenance?.client || ""} · ${fmtDate(m.createdAt)}`));
}

// ---------------------------------------------------------------- drawer

const drawer = document.getElementById("drawer");
document.getElementById("drawer-close").addEventListener("click", () => closeDrawer());
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeDrawer(); });
function closeDrawer() { drawer.classList.remove("open"); drawer.setAttribute("aria-hidden", "true"); }

async function openMemory(id) {
  const body = document.getElementById("drawer-body");
  put(body, h("p", { class: "muted" }, "Loading…"));
  drawer.classList.add("open");
  drawer.setAttribute("aria-hidden", "false");
  try {
    const d = await api("memory/" + encodeURIComponent(id));
    const m = d.memory;
    const byId = Object.fromEntries(d.neighbors.map((n) => [n.id, n]));
    put(body, 
      h("div", { class: "row" }, kindTag(m.kind), h("span", { class: "muted mono" }, m.namespace), m.validTo ? pill("archived", "warn") : null),
      h("h1", { style: "margin-top:8px" }, m.content),
      m.decision ? h("div", { class: "card" },
        h("div", { class: "meta" }, "Decision"),
        h("div", {}, "Situation: ", m.decision.situation || ""),
        h("div", {}, "Choice: ", m.decision.choice || ""),
        m.decision.rationale ? h("div", {}, "Why: ", m.decision.rationale) : null) : null,
      table(["", ""], [
        ["id", h("span", { class: "mono" }, m.id)],
        ["strength", m.strength.toFixed(2)],
        ["author", m.provenance.author],
        ["written by", `${m.provenance.client}${m.provenance.service ? " via " + m.provenance.service : ""}`],
        ["created", fmtDate(m.createdAt)],
        ["valid from", fmtDate(m.validFrom)],
        ["last recalled", fmtDate(m.lastActivatedAt) || "never"],
        ["tags", (m.tags || []).join(", ")],
      ]),
      h("h2", {}, `Pathways (${d.edges.length})`),
      d.edges.length ? h("div", {}, d.edges.map((e) => {
        const other = e.from === m.id ? e.to : e.from;
        const n = byId[other];
        return h("div", { class: "mem", onclick: () => openMemory(other) },
          h("div", { class: "row" }, pill(e.kind, "ok"), h("span", { class: "muted" }, e.from === m.id ? "→" : "←"),
            h("div", { class: "bar", style: "width:80px" }, h("span", { style: `width:${Math.round(e.weight * 100)}%` })),
            h("span", { class: "muted mono" }, e.weight.toFixed(2))),
          h("div", { class: "content" }, n ? n.content : other));
      })) : h("div", { class: "empty" }, "No pathways yet."));
  } catch (err) {
    put(body, h("p", {}, err.message));
  }
}

// ---------------------------------------------------------------- views

const views = {
  overview: { label: "Overview", render: renderOverview },
  inbox: { label: "Inbox", render: renderInbox },
  graph: { label: "Graph", render: renderGraph },
  memories: { label: "Memories", render: renderMemories },
  sleep: { label: "Sleep", render: renderSleep },
  health: { label: "Health", render: renderHealth },
  triggers: { label: "Triggers", render: renderTriggers },
  sessions: { label: "Sessions", render: renderSessions },
  agents: { label: "Agents", render: renderAgents },
  packs: { label: "Packs", render: renderPacks },
  permissions: { label: "Permissions", render: renderPermissions },
};
const main = document.getElementById("view");
let current = "overview";
let stopGraph = null;

function renderNav(inboxCount) {
  put(document.getElementById("nav"), ...Object.entries(views).map(([id, v]) =>
    h("button", { "aria-current": id === current ? "page" : null, onclick: () => go(id) },
      v.label, id === "inbox" && inboxCount ? h("span", { class: "badge" }, inboxCount) : null)));
}

async function go(id) {
  if (stopGraph) { stopGraph(); stopGraph = null; }
  current = id;
  try { sessionStorage.setItem("engram-view", id); } catch (e) {}
  renderNav(window.__inbox || 0);
  put(main, h("p", { class: "muted" }, "Loading…"));
  try {
    await views[id].render();
  } catch (err) {
    put(main, h("h1", {}, "Couldn't load this view"), h("p", { class: "lede" }, err.message));
  }
  main.focus({ preventScroll: true });
}

async function renderOverview() {
  const o = await api("overview");
  window.__inbox = o.inbox;
  renderNav(o.inbox);
  const s = o.stats, last = o.lastSleep;
  const tile = (k, v, sub, target) => h("div", { class: "tile", style: target ? "cursor:pointer" : null, onclick: target ? () => go(target) : null },
    h("div", { class: "k" }, k), h("div", { class: "v" }, v), sub ? h("div", { class: "s" }, sub) : null);
  put(main, 
    h("h1", {}, "Your shared memory"),
    h("p", { class: "lede" }, `${s.current} current memories across ${o.namespaces.length} namespaces, written by ${o.clients} agents and tools.`),
    h("div", { class: "grid" },
      tile("Memories", s.current, `${s.memories - s.current} archived`, "memories"),
      tile("Pathways", s.edges, "learned and similar links", "graph"),
      tile("Health", o.health.worst, `${o.health.bounds.length} bounds checked`, "health"),
      tile("Inbox", o.inbox, "waiting for you", "inbox"),
      tile("Open tasks", o.openSessions, "checkpointed by agents", "sessions"),
      tile("Triggers", o.triggers, `${o.recentFailures} recent failures`, "triggers")),
    h("h2", {}, "Last sleep cycle"),
    last ? h("div", { class: "card" },
      h("div", { class: "meta" }, `${fmtDate(last.finishedAt)} · ${last.light ? "light" : "full"} cycle`),
      h("div", {}, `Merged ${last.merged}, archived ${last.archived}, downscaled ${last.downscaled}, pruned ${last.pruned} pathways, distilled ${last.distilled.length} facts from ${last.hubs} hubs.`),
      last.selfTest.tested ? h("div", {}, `Recall self-test: found ${last.selfTest.found} of ${last.selfTest.tested} of your decisions.`) : null,
      (last.notes || []).map((n) => h("div", { class: "muted" }, n)))
      : h("div", { class: "empty" }, "No sleep cycle yet. Run engram sleep run, or set sleep hours."),
    h("h2", {}, "Namespaces"),
    table(["Namespace", "Memories"], o.namespaces.map((n) => [h("span", { class: "mono" }, n.namespace), n.memories]), [1]));
}

async function renderInbox() {
  const { items } = await api("inbox");
  window.__inbox = items.length;
  renderNav(items.length);
  put(main, h("h1", {}, "Approvals inbox"),
    h("p", { class: "lede" }, "What Engram learned on its own, and disagreements only you can settle."));
  if (!items.length) { main.append(h("div", { class: "empty" }, "Nothing to review. Distilled facts and conflicts appear here.")); return; }
  for (const it of items) {
    const card = h("div", { class: "card" });
    const done = (msg) => { put(card, h("div", { class: "muted" }, msg)); window.__inbox = Math.max(0, (window.__inbox || 1) - 1); renderNav(window.__inbox); };
    if (it.kind === "distilled") {
      card.append(
        h("div", { class: "row" }, pill("distilled", "ok"), h("span", { class: "meta" }, fmtDate(it.at))),
        h("h1", { style: "margin:8px 0 2px;font-size:16px" }, it.title),
        h("div", { class: "meta" }, it.detail),
        h("div", { class: "sources" }, (it.sources || []).map((m) => memRow(m))),
        h("div", { class: "row", style: "margin-top:12px" },
          h("button", { class: "btn primary", onclick: async () => { await api("inbox/review", { id: it.id, approve: true }); done("Kept."); } }, "Keep"),
          h("button", { class: "btn danger", onclick: async () => { await api("inbox/review", { id: it.id, approve: false }); done("Rejected and archived."); } }, "Reject")));
    } else {
      const [a, b] = it.sources;
      const side = (keep, drop) => h("div", { class: "choice" },
        memRow(keep),
        h("button", { class: "btn", onclick: async () => { await api("inbox/resolve", { keep: keep.id, drop: drop.id }); done("Resolved."); } }, "This one is right"));
      card.append(h("div", { class: "row" }, pill("conflict", "warn"), h("span", { class: "meta" }, it.detail)),
        h("div", { class: "choices" }, side(a, b), side(b, a)));
    }
    main.append(card);
  }
}

async function renderMemories() {
  const o = await api("overview");
  const q = h("input", { type: "search", placeholder: "Search by meaning, like an agent would…", "aria-label": "Search memories" });
  const ns = h("select", { "aria-label": "Namespace" }, h("option", { value: "" }, "All namespaces"), o.namespaces.map((n) => h("option", { value: n.namespace }, n.namespace)));
  const kind = h("select", { "aria-label": "Kind" }, h("option", { value: "" }, "All kinds"),
    ["decision", "fact", "episode", "suggestion", "concept", "trait", "procedure"].map((k) => h("option", { value: k }, k)));
  const list = h("div", { class: "card" });
  const load = async () => {
    const p = new URLSearchParams({ q: q.value, ns: ns.value, kind: kind.value });
    const d = await api("memories?" + p);
    const rows = d.scored ? d.scored.map((s) => memRow(s.memory, h("span", { class: "muted" }, `${s.reason} · ${s.confidence.toFixed(2)}`))) : d.memories.map((m) => memRow(m));
    put(list, ...(rows.length ? rows : [h("div", { class: "empty" }, "No memories match.")]));
  };
  let t;
  q.addEventListener("input", () => { clearTimeout(t); t = setTimeout(load, 250); });
  ns.addEventListener("change", load);
  kind.addEventListener("change", load);
  put(main, h("h1", {}, "Memories"),
    h("p", { class: "lede" }, "Search runs the same recall agents use, without training any pathways."),
    h("div", { class: "row", style: "margin-bottom:12px" }, q, ns, kind), list);
  await load();
}

async function renderGraph() {
  const o = await api("overview");
  const ns = h("select", { "aria-label": "Namespace" }, h("option", { value: "" }, "All namespaces"), o.namespaces.map((n) => h("option", { value: n.namespace }, n.namespace)));
  const canvas = h("canvas", { id: "graph", role: "img", "aria-label": "Memory graph" });
  const tip = h("div", { class: "tooltip" });
  const kinds = ["decision", "fact", "episode", "suggestion", "concept", "trait", "procedure"];
  const legend = h("div", { class: "legend" }, kinds.map(kindTag), h("span", {}, "line width = pathway weight · ring = recalled in the last day"));
  put(main, h("h1", {}, "Graph"),
    h("p", { class: "lede" }, "The strongest and most used memories and the pathways between them. Click a memory to inspect it."),
    h("div", { class: "row", style: "margin-bottom:12px" }, ns),
    h("div", { id: "graph-wrap" }, canvas, tip, legend));
  const draw = async () => {
    if (stopGraph) stopGraph();
    const g = await api("graph?" + new URLSearchParams({ ns: ns.value, limit: 220 }));
    stopGraph = forceGraph(canvas, tip, g);
  };
  ns.addEventListener("change", draw);
  await draw();
}

function forceGraph(canvas, tip, g) {
  const ctx = canvas.getContext("2d");
  const dpr = window.devicePixelRatio || 1;
  const css = getComputedStyle(document.documentElement);
  const color = (k) => css.getPropertyValue("--" + (["episode", "fact", "decision", "suggestion", "concept", "trait", "procedure"].includes(k) ? k : "muted")).trim();
  const lineColor = css.getPropertyValue("--muted").trim();
  let W = 0, H = 0;
  const resize = () => { W = canvas.clientWidth; H = canvas.clientHeight; canvas.width = W * dpr; canvas.height = H * dpr; };
  resize();
  const idx = new Map();
  const nodes = g.nodes.map((n, i) => {
    idx.set(n.id, i);
    const a = (i / Math.max(1, g.nodes.length)) * Math.PI * 2;
    return { ...n, x: W / 2 + Math.cos(a) * W / 4 + Math.random() * 20, y: H / 2 + Math.sin(a) * H / 4 + Math.random() * 20, vx: 0, vy: 0,
      r: 4 + Math.min(10, n.strength * 8 + Math.sqrt(n.activations)),
      recent: n.lastActive && Date.now() - new Date(n.lastActive) < 864e5 };
  });
  const edges = g.edges.filter((e) => idx.has(e.from) && idx.has(e.to)).map((e) => ({ ...e, a: nodes[idx.get(e.from)], b: nodes[idx.get(e.to)] }));
  let view = { x: 0, y: 0, k: 1 }, hover = null, drag = null, alpha = 1, raf = 0, pan = null;
  const step = () => {
    if (alpha > 0.01) {
      for (let i = 0; i < nodes.length; i++) {
        const p = nodes[i];
        for (let j = i + 1; j < nodes.length; j++) {
          const q = nodes[j];
          let dx = p.x - q.x, dy = p.y - q.y, d2 = dx * dx + dy * dy + 0.01;
          if (d2 > 40000) continue;
          const f = (900 / d2) * alpha;
          p.vx += dx * f; p.vy += dy * f; q.vx -= dx * f; q.vy -= dy * f;
        }
        p.vx += (W / 2 - p.x) * 0.008 * alpha; p.vy += (H / 2 - p.y) * 0.008 * alpha;
      }
      for (const e of edges) {
        const dx = e.b.x - e.a.x, dy = e.b.y - e.a.y, d = Math.sqrt(dx * dx + dy * dy) || 1;
        const f = ((d - 60) / d) * 0.04 * (0.3 + e.weight) * alpha;
        e.a.vx += dx * f; e.a.vy += dy * f; e.b.vx -= dx * f; e.b.vy -= dy * f;
      }
      for (const p of nodes) {
        if (p === drag) continue;
        p.x = Math.min(W - p.r - 6, Math.max(p.r + 6, p.x + p.vx));
        p.y = Math.min(H - p.r - 30, Math.max(p.r + 6, p.y + p.vy));
        p.vx *= 0.6; p.vy *= 0.6;
      }
      alpha *= 0.985;
    }
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, W, H);
    ctx.translate(view.x, view.y); ctx.scale(view.k, view.k);
    for (const e of edges) {
      ctx.strokeStyle = lineColor;
      ctx.globalAlpha = e.kind === "conflicts" ? 0.9 : 0.15 + e.weight * 0.5;
      ctx.lineWidth = (0.5 + e.weight * 3) / view.k;
      ctx.setLineDash(e.kind === "conflicts" ? [4 / view.k, 3 / view.k] : e.kind === "similar" ? [1.5 / view.k, 2.5 / view.k] : []);
      ctx.beginPath(); ctx.moveTo(e.a.x, e.a.y); ctx.lineTo(e.b.x, e.b.y); ctx.stroke();
    }
    ctx.setLineDash([]); ctx.globalAlpha = 1;
    for (const p of nodes) {
      ctx.fillStyle = color(p.kind);
      ctx.beginPath(); ctx.arc(p.x, p.y, p.r, 0, Math.PI * 2); ctx.fill();
      if (p.recent || p === hover) { ctx.strokeStyle = color(p.kind); ctx.lineWidth = 2 / view.k; ctx.beginPath(); ctx.arc(p.x, p.y, p.r + 4 / view.k, 0, Math.PI * 2); ctx.stroke(); }
    }
    raf = requestAnimationFrame(step);
  };
  const toWorld = (ev) => { const r = canvas.getBoundingClientRect(); return { x: (ev.clientX - r.left - view.x) / view.k, y: (ev.clientY - r.top - view.y) / view.k, sx: ev.clientX - r.left, sy: ev.clientY - r.top }; };
  const pick = (w) => nodes.find((p) => (p.x - w.x) ** 2 + (p.y - w.y) ** 2 <= (p.r + 3) ** 2);
  const onMove = (ev) => {
    const w = toWorld(ev);
    if (drag) { drag.x = w.x; drag.y = w.y; alpha = Math.max(alpha, 0.3); return; }
    if (pan) { view.x = pan.vx + (ev.clientX - pan.x); view.y = pan.vy + (ev.clientY - pan.y); return; }
    hover = pick(w) || null;
    canvas.style.cursor = hover ? "pointer" : "grab";
    if (hover) { tip.textContent = `${hover.kind} · ${hover.namespace}: ${hover.content}`; tip.style.display = "block"; tip.style.left = Math.min(w.sx + 12, W - 330) + "px"; tip.style.top = w.sy + 12 + "px"; }
    else tip.style.display = "none";
  };
  let downAt = null;
  const onDown = (ev) => { const w = toWorld(ev); downAt = { x: ev.clientX, y: ev.clientY }; const p = pick(w); if (p) drag = p; else pan = { x: ev.clientX, y: ev.clientY, vx: view.x, vy: view.y }; };
  const onUp = (ev) => {
    const moved = downAt && Math.hypot(ev.clientX - downAt.x, ev.clientY - downAt.y) > 4;
    if (drag && !moved) openMemory(drag.id);
    drag = null; pan = null; downAt = null;
  };
  const onWheel = (ev) => { ev.preventDefault(); const w = toWorld(ev); const k = Math.min(4, Math.max(0.25, view.k * (ev.deltaY < 0 ? 1.1 : 0.9))); view.x = w.sx - w.x * k; view.y = w.sy - w.y * k; view.k = k; };
  canvas.addEventListener("pointermove", onMove);
  canvas.addEventListener("pointerdown", onDown);
  window.addEventListener("pointerup", onUp);
  canvas.addEventListener("wheel", onWheel, { passive: false });
  window.addEventListener("resize", resize);
  if (!nodes.length) { tip.style.display = "block"; tip.textContent = "No memories yet."; }
  step();
  return () => { cancelAnimationFrame(raf); window.removeEventListener("pointerup", onUp); window.removeEventListener("resize", resize); };
}

async function renderSleep() {
  const { runs, changes } = await api("sleep");
  put(main, h("h1", {}, "Sleep"), h("p", { class: "lede" }, "Every merge, archive and distilled fact from the last cycle can be undone. Nothing is deleted outright."));
  if (!runs.length) { main.append(h("div", { class: "empty" }, "No sleep cycles yet.")); return; }
  main.append(h("h2", {}, `Changes in the last cycle (${fmtDate(runs[0].finishedAt || runs[0].startedAt)})`));
  main.append((changes || []).length ? table(["Change", "Memory", "Detail", ""], changes.map((c) => [
    pill(c.kind, "ok"),
    h("a", { href: "#", class: "mono", onclick: (e) => { e.preventDefault(); openMemory(c.memoryId); } }, c.memoryId),
    c.detail || "",
    c.undoneAt ? h("span", { class: "muted" }, "undone") : h("button", { class: "btn", onclick: async (e) => {
      e.target.disabled = true;
      try { await api("sleep/undo", { id: c.id }); toast("Undone."); renderSleep(); } catch (err) { toast(err.message); e.target.disabled = false; }
    } }, "Undo")])) : h("div", { class: "empty" }, "The last cycle changed nothing."));
  main.append(h("h2", {}, "Recent cycles"), table(["Finished", "Type", "Merged", "Archived", "Distilled", "Decision recall"], runs.map((r) => {
    const rep = r.report || {};
    const st = rep.selfTest || {};
    return [fmtDate(r.finishedAt || r.startedAt), r.light ? "light" : "full", rep.merged ?? "", rep.archived ?? "", (rep.distilled || []).length,
      st.tested ? `${st.found}/${st.tested}` : "-"];
  }), [2, 3, 4, 5]));
}

async function renderHealth() {
  const d = await api("health");
  const r = d.report;
  const st = h("div");
  put(main, h("h1", {}, "Health"),
    h("p", { class: "lede" }, "Software can pass every test and still drift into bad dynamics. These bounds catch that."),
    h("div", { class: "row" }, h("span", {}, "Overall: "), pill(r.worst), h("span", { class: "spacer" }),
      h("button", { class: "btn", onclick: async (e) => {
        e.target.disabled = true; put(st, h("p", { class: "muted" }, "Running on a sandbox copy…"));
        try {
          const res = await api("selftest", {});
          put(st, h("h2", {}, "Self-test"), table(["Result", "Kind", "Test", "Detail"], res.results.map((t) => [pill(t.passed ? "pass" : "fail", t.passed ? "ok" : "bad"), t.kind, t.name, t.detail || ""])));
        } catch (err) { put(st, h("p", {}, err.message)); }
        e.target.disabled = false;
      } }, "Run self-test")),
    h("h2", {}, "Bounds"),
    table(["Status", "Bound", "Value", "Healthy when", "Note"], [...r.bounds, ...(r.drift || [])].map((b) =>
      [pill(b.status === "not yet measurable" ? "n/a" : b.status, b.status === "not yet measurable" ? "" : b.status), b.name,
        b.status === "not yet measurable" ? "-" : b.value.toFixed(2), b.limit, b.detail || ""]), [2]),
    h("h2", {}, "Not yet measurable"),
    h("div", { class: "card" }, (r.notYetMeasurable || []).map((u) => h("div", { class: "muted" }, u))),
    h("h2", {}, `History (${(d.history || []).length} nights)`),
    (d.history || []).length ? table(["Night", "Memories", "Pathways", "Saturated", "At floor", "Largest hub", "Decision recall"], d.history.map((m) => [
      fmtDate(m.at), m.memories, m.coactiveEdges, pct(m.saturated), pct(m.atFloor), m.maxDegree, m.decisionRecall < 0 ? "-" : pct(m.decisionRecall)]), [1, 2, 3, 4, 5, 6])
      : h("div", { class: "empty" }, "Recorded after each sleep cycle."),
    h("h2", {}, `Golden queries (${(d.golden || []).length})`),
    (d.golden || []).length ? table(["Query", "Must recall"], d.golden.map((g) => [g.query, h("span", { class: "mono" }, g.expectId)]))
      : h("div", { class: "empty" }, "Add one with engram golden add \"QUERY\" MEMORY_ID."),
    st);
}

async function renderTriggers() {
  const { triggers, events } = await api("triggers");
  put(main, h("h1", {}, "Triggers and jobs"),
    h("p", { class: "lede" }, "Events that wake Engram up: webhooks, GitHub, and the outcomes of jobs you already run."));
  if (!triggers.length) { main.append(h("div", { class: "empty" }, "No triggers. Create one with engram trigger add NAME, or wrap a cron job in engram job run NAME -- COMMAND.")); return; }
  main.append(table(["Trigger", "Kind", "Namespace", "Fired", "Failed", "Last", ""], triggers.map((t) => [
    h("strong", {}, t.name), t.kind, h("span", { class: "mono" }, t.namespace), t.fired, t.failed, fmtDate(t.lastFiredAt),
    h("button", { class: "btn", onclick: async () => { await api(`triggers/${encodeURIComponent(t.name)}/${t.paused ? "resume" : "pause"}`, {}); renderTriggers(); } }, t.paused ? "Resume" : "Pause")]), [3, 4]));
  main.append(h("h2", {}, "Recent events"), table(["When", "Trigger", "Status", "Notified", "Summary"], events.map((e) => [
    fmtDate(e.at), e.trigger, pill(e.status, e.status === "ok" ? "ok" : e.status === "failed" ? "failed" : "warn"), e.notified,
    e.memoryId ? h("a", { href: "#", onclick: (ev) => { ev.preventDefault(); openMemory(e.memoryId); } }, e.summary) : e.summary]), [3]));
}

async function renderSessions() {
  const { sessions } = await api("sessions");
  put(main, h("h1", {}, "Sessions"), h("p", { class: "lede" }, "Tasks agents checkpointed, so any of them can pick up where another stopped."));
  if (!sessions.length) { main.append(h("div", { class: "empty" }, "No checkpointed tasks yet.")); return; }
  for (const s of sessions) {
    main.append(h("div", { class: "card" },
      h("div", { class: "row" }, pill(s.status, s.status === "open" ? "warn" : "ok"), h("span", { class: "mono muted" }, s.namespace),
        h("span", { class: "meta" }, `started by ${s.createdBy || "?"}, last ${s.lastClient}${s.delegatedTo ? ", delegated to " + s.delegatedTo : ""} · ${fmtDate(s.updatedAt)}`)),
      h("h1", { style: "font-size:16px;margin:8px 0" }, s.goal),
      s.next ? h("div", {}, h("span", { class: "muted" }, "Next: "), s.next) : null,
      s.result ? h("div", {}, h("span", { class: "muted" }, "Result: "), s.result) : null,
      (s.plan || []).length ? h("div", { class: "meta" }, `${(s.done || []).length} done · plan: ${s.plan.join(" → ")}`) : null,
      (s.openQuestions || []).length ? h("div", { class: "meta" }, "Open questions: " + s.openQuestions.join("; ")) : null));
  }
}

async function renderAgents() {
  const { clients } = await api("clients");
  put(main, h("h1", {}, "Agents and sources"), h("p", { class: "lede" }, "Who can read and write what, and what they did in the last 30 days. Pairing happens in your terminal, never here."));
  if (!clients.length) { main.append(h("div", { class: "empty" }, "No agents paired. Run engram pair claude-code --read me --write me.")); return; }
  main.append(table(["Agent", "Access", "Activity (30 days)", "Last seen", ""], clients.map((c) => [
    h("strong", {}, c.client),
    h("div", {}, c.grants.map((g) => h("div", { class: "mono" }, `${g.namespace}: ${[g.read && "read", g.write && "write"].filter(Boolean).join("+")}${g.expiresAt ? " until " + fmtDate(g.expiresAt) : ""}`))),
    Object.entries(c.actions).map(([a, n]) => `${a} ${n}`).join(" · ") || "-",
    fmtDate(c.last),
    h("button", { class: "btn danger", onclick: async (e) => {
      if (!confirmInline(e.target, "Revoke?")) return;
      await api(`clients/${encodeURIComponent(c.client)}/revoke`, {}); toast(`${c.client} revoked.`); renderAgents();
    } }, "Revoke")])));
}

async function renderPacks() {
  const [{ packs }, o] = await Promise.all([api("packs"), api("overview")]);
  main.replaceChildren();
  put(main, h("h1", {}, "Packs"),
    h("p", { class: "lede" }, "Pre-wired memory that gives Engram a head start, then adapts to you. Pack pathways install at low weight; the bars show how your use has reweighted them."));
  const installed = packs.filter((p) => p.installed), available = packs.filter((p) => !p.installed);
  if (installed.length) {
    main.append(table(["Pack", "Version", "Source", "Memories", "Pathways", "How use changed them", ""], installed.map((p) => {
      const s = p.stats || {};
      const total = Math.max(1, s.edges);
      return [
        h("div", {}, h("strong", {}, p.name), " ", pill(p.kind, ""), h("div", { class: "meta muted" }, p.description || "")),
        p.version,
        h("div", {}, p.source, " ", pill(p.verified ? "verified" : "unverified", p.verified ? "ok" : "warn")),
        `${s.memories} (${s.recalled} recalled)`,
        s.edges,
        s.edges ? h("div", {},
          h("div", { class: "bar", title: `${s.strengthened} strengthened, ${s.weakened} weakened` },
            h("span", { style: `width:${Math.round((100 * s.strengthened) / total)}%` })),
          h("div", { class: "meta muted" }, `${s.strengthened} strengthened, ${s.weakened} weakened, mean change ${s.meanDelta >= 0 ? "+" : ""}${s.meanDelta.toFixed(2)}`)) : "-",
        h("button", { class: "btn danger", onclick: async (e) => {
          if (!confirmInline(e.target, "Remove?")) return;
          await api(`packs/${encodeURIComponent(p.name)}/remove`, {}); toast(`${p.name} removed.`); renderPacks();
        } }, "Remove")];
    }), [3, 4]));
  } else {
    main.append(h("div", { class: "empty" }, "No packs installed."));
  }
  if (available.length) {
    main.append(h("h2", {}, "Built in"), table(["Pack", "Version", "About", ""], available.map((p) => [
      h("strong", {}, p.name), p.version, p.description,
      h("button", { class: "btn primary", onclick: async (e) => {
        e.target.disabled = true;
        try { await api(`packs/${encodeURIComponent(p.name)}/install`, {}); toast(`${p.name} installed.`); renderPacks(); }
        catch (err) { toast(err.message); e.target.disabled = false; }
      } }, "Install")])));
  }
  main.append(h("p", { class: "meta muted" }, "Packs from registries or files are installed in the terminal (engram pack install), where their signatures and publishers are checked."));

  const ns = h("select", { "aria-label": "Namespace to export" }, o.namespaces.filter((n) => !n.namespace.startsWith("pack/"))
    .map((n) => h("option", { value: n.namespace }, `${n.namespace} (${n.memories})`)));
  const name = h("input", { type: "search", placeholder: "pack name, like team-knowledge", "aria-label": "Pack name", style: "min-width:200px" });
  const out = h("div", { class: "meta" });
  main.append(h("h2", {}, "Export"),
    h("div", { class: "card" },
      h("div", {}, "Turn a namespace's learned subgraph into an instance pack: its current facts, concepts and procedures and the pathways between them. Episodes, decisions and suggestions stay private."),
      h("div", { class: "row", style: "margin-top:10px" }, ns, name,
        h("button", { class: "btn", onclick: async () => {
          out.textContent = "";
          try {
            const res = await fetch("/api/export?" + new URLSearchParams({ ns: ns.value, name: name.value.trim() }), { headers: { "X-Engram-Token": token } });
            if (!res.ok) throw new Error((await res.json()).error);
            const blob = await res.blob();
            const a = h("a", { href: URL.createObjectURL(blob), download: `${name.value.trim()}-0.1.0.json` });
            document.body.append(a); a.click(); a.remove();
            out.textContent = "Downloaded. Review it, then sign it with engram pack sign FILE before sharing.";
          } catch (err) { out.textContent = err.message; }
        } }, "Download")),
      out));
}

// Two-step confirm without a browser dialog.
function confirmInline(btn, label) {
  if (btn.dataset.armed) return true;
  btn.dataset.armed = "1"; const old = btn.textContent; btn.textContent = label;
  setTimeout(() => { delete btn.dataset.armed; btn.textContent = old; }, 3000);
  return false;
}

async function renderPermissions() {
  const r = await api("permits");
  put(main, h("h1", {}, "Permission assist"),
    h("p", { class: "lede" }, `Shadow mode: Engram predicts, you decide. Logging is ${r.enabled ? "on" : "off"}. ${r.threshold}.`),
    table(["Tier", "Requests", "Approved", "Denied", "Would allow", "Precision", "Wrong allows", "Prompts saved", "Ready?"], r.tiers.map((t) => [
      pill(t.tier, t.tier === "eligible" ? "ok" : t.tier === "never" ? "never" : ""), t.requests, t.approved, t.denied, t.predictedAllow,
      t.allowApproved + t.allowDenied ? t.precision.toFixed(3) : "-", t.allowDenied, pct(t.promptsAvoided), t.readinessReason]), [1, 2, 3, 4, 5, 6, 7]),
    h("h2", {}, "Would have approved, but you denied"),
    r.mistakes.length ? table(["When", "Request", "Why Engram would allow"], r.mistakes.map((m) => [fmtDate(m.at), h("span", { class: "mono" }, m.summary), m.reason]))
      : h("div", { class: "empty" }, "None so far."),
    h("h2", {}, "Recent requests"),
    r.recent.length ? table(["When", "Request", "Tier", "Prediction", "You"], r.recent.map((m) => [fmtDate(m.at), h("span", { class: "mono" }, m.summary), m.tier, `${m.predicted} (${m.confidence.toFixed(2)})`,
      m.outcome + (m.objective === "failed" ? ", failed" : "")]))
      : h("div", { class: "empty" }, "Install the hook printed by engram setup claude-code to start collecting."));
}

// ---------------------------------------------------------------- start

(async () => {
  if (!token) {
    put(main, h("h1", {}, "Open the link from your terminal"), h("p", { class: "lede" }, "Run engram dashboard and open the URL it prints. It carries a one-time access token."));
    return;
  }
  let start = "overview";
  try { start = sessionStorage.getItem("engram-view") || "overview"; } catch (e) {}
  try {
    const o = await api("overview");
    window.__inbox = o.inbox;
    document.getElementById("status").textContent = `${o.stats.current} memories · health ${o.health.worst}`;
  } catch (e) {}
  go(views[start] ? start : "overview");
})();
