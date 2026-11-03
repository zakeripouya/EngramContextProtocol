# Backlog

Everything specified in the [design](design.md), the [ECP spec](spec/ecp-v0.md) and the [README](../README.md) that is **not built yet**, plus known rough edges in what is built. Milestones M1–M11 are done; this file tracks what's left.

**How to use it.** Tick an item when it ships, add a line to the [CHANGELOG](../CHANGELOG.md), and change the matching *design* label in the README or design doc. New gaps found while building go here, not in TODO comments.

**Tags.** Phase: `polish` (refine what exists), `M11` (packaging and protocol), `M12` (publish), `v2` (next mechanisms), `v3` and `research` (later, off by default). Size: `S` (hours), `M` (a day or two), `L` (a milestone of its own).

---

## 0. Fix first

- [ ] Run `go mod tidy` with network access and commit `go.sum`, so CI goes green. The sandbox that built M1–M10 couldn't download modules. `polish` `S`
- [x] Stale doc labels: the README protocol reference still marks `engram/attend` as *(design)*, and "Decision memory and the personal model" says *Design (M4, M9)* though decisions, feedback and permission shadow mode exist. Retrieval says "up to two hops"; the default is 2 and the maximum 3. `polish` `S`
- [x] The README's "Planned beyond M10" dashboard table overlaps what M10 built; trim it to what's left (§10). `polish` `S`

## 1. Protocol and transport (ECP)

- [x] Native `engram/*` JSON-RPC methods over stdio and HTTP (0.11.0). `M11` `L`
- [x] HTTP transport with real authentication: CLI-issued bearer tokens decide identity (0.11.0). `M11` `L`
- [ ] OAuth 2.1 authorization server for HTTP clients (tokens are CLI-issued today). `v2` `M`
- [x] `engram/path`: maximin (weakest-link) and `most_probable` (−log weight) paths between two memories. `M11` `M`
- [x] `engram/consolidate`: trigger a sleep cycle over the protocol (`namespace`, `light`). `M11` `S`
- [x] Native notifications: `notifications/engram/relevant`, `notifications/engram/conflict` and `notifications/engram/consolidated`. Over MCP today only `engram://attention` updates exist, and conflict and consolidation events aren't pushed at all. `M11` `M`
- [x] Push rate limit per subscription, as the spec says. Today it's per connection. `polish` `S`
- [x] Capability negotiation as specified (`capabilities` in `initialize`, `supportedVersions`). `M11` `M`
- [x] Conformance test suite that any implementation can run. `M11` `L`
- [x] SDKs in Go, TypeScript and Python, generated from `spec/schema.json`. `M11` `L`
- [ ] ECP v1: freeze the schema and settle the open issues below. A governance decision for the maintainer; the reference server and conformance suite are ready for it. `M11` `M`
- [ ] Open issues from the spec:
  - [ ] Name check: "ECP" and "Engram" against other AI-protocol projects and trademarks (Roku's External Control Protocol uses the acronym).
  - [ ] Expose sparse codes and edge weights on the wire, or keep them server-internal?
  - [ ] Multi-server federation: can two Engram servers share a namespace?
  - [ ] Standard format for importing chat exports from assistant apps.
  - [ ] How clients declare active goals for `attend` (per request, per session, or as stored memories). Per request today.
  - [ ] Should `engram/feedback` accept partial credit?
  - [ ] Governance once third parties implement the spec.

## 2. Memory model

- [ ] Entity resolution: turn agent observations into shared `concept` nodes with `about` edges, so two agents discussing one subject meet at one node. Today `concept` and `about` come only from git imports. Needs a threshold and a merge-reversal policy. `v2` `L`
- [ ] `cross` edges between namespaces: start weak, earn strength, never merge across namespaces without approval. `v2` `M`
- [ ] `accepted` edges from feedback. Today feedback only adjusts weights; overrides do create `overrode` edges. `polish` `S`
- [ ] `scope` semantics (`private`, `team`, `public`). Today everything is effectively `private`, and grants do all the work. `M11` `M`
- [ ] External-knowledge namespaces (`ext/...`) refreshed from their source and linked, never copied wholesale. `v2` `M`
- [ ] Sparse block codes (64 blocks × 128 positions, ~0.8% density): overlap as similarity, binding by per-block shifts, bundling by voting, and storable trains of thought. Parameters are an open question. `research` `L`

## 3. Learning

- [ ] Replay step in sleep: re-run Hebbian updates on recent independent co-activations. `v2` `M`
- [ ] Self-test that learns: weaken pathways that predicted held-out decisions wrong. Today it only measures. `v2` `M`
- [ ] Candidate traits distilled from patterns across decisions, held at low confidence until the user approves them. Today distillation only produces facts. `v2` `L`
- [ ] Procedures: compile successful multi-step runs into `procedure` nodes (Soar chunking) and record impasses with their resolutions. `v2` `L`
- [ ] Mid-task corrections ("no, not like that") recorded automatically as overrides, from hooks rather than only through `feedback`. `v2` `M`
- [ ] v2 neuroscience mechanisms, each a switchable module kept only if an ablation shows it helps: `v2`
  - [ ] Prediction error: update size scales with surprise; fully predicted episodes aren't stored.
  - [ ] Synaptic tagging and capture: stabilize weak memories just before important outcomes.
  - [ ] Schema-accelerated consolidation: pack-consistent memories consolidate fast.
  - [ ] Temporal linking: same-session episodes get weak edges.
  - [ ] Reconsolidation: recall opens a logged correction window.
  - [ ] Global workspace: `attend` as a limited stage broadcast to subscribers.
- [ ] Self-tuning parameters: let the sleep cycle propose changes to Engram's own hand-set constants (recall mix 0.75/0.25, damping 0.9, learning rate 0.15, cutoffs, `attend` salience weights) by searching them against held-out feedback. Proposals are bounded, logged, applied only if they beat the current values on held-out data and health bounds, need approval by default, and roll back with the snapshot. `v2` `M`
- [ ] Local model adaptation (research, off by default): a LoRA adapter on a local model trained on the user's approved decisions, so the model itself carries some of the style. Open problems: it can't honor `forget` or provenance (a fact baked into weights has no source and can't be deleted except by retraining), catastrophic forgetting, and evaluation. The graph stays the source of truth either way. `research` `L`
- [ ] Neuromodulation as bounded control signals: dopamine (outcome-gated learning; partly done via three-factor), norepinephrine (urgency narrows search), acetylcholine (encode vs. retrieve), serotonin (long-term vs. recent precedent). Logged, slow and bounded. `v2` `M`
- [ ] v3 and research: pattern separation and completion via sparse codes; cognitive maps (reusing relational structure across domains). `research`

## 4. Retrieval

- [ ] Seeding by sparse-code overlap, not only keywords and embeddings. Needs §2 sparse codes. `research` `M`
- [ ] A* with embedding distance for connecting two known concepts. `v2` `M`
- [ ] "Don't know" answers: return nothing, explicitly, when confidence is low across the board. `polish` `S`
- [ ] Calibrated confidence (isotonic or Platt) once feedback data exists. Today it's heuristic. `v2` `M`
- [ ] Learning-to-rank with gradient-boosted trees (XGBoost or LightGBM, `rank:pairwise` / `lambdarank`), trained on accepts and overrides. `v2` `L`
  - Features per recalled item: keyword score, meaning score, prior, strength, use count, age, hop count, pathway weight, edge kinds on the path, memory kind, overridden flag, namespace match.
  - Labels from the feedback table: succeeded or accepted = relevant, failed or overridden = not; items shown but never used = weak negatives.
  - Train in a Python worker during the sleep cycle (§13); save the model with a version; score in Go with a pure-Go reader of the model file (for example `leaves`), so no Python runs at recall time.
  - Gate: keep the hand-set formula as baseline; switch only when the model wins on held-out feedback (NDCG@5 and accept rate), per namespace, with one-command rollback. Below a few hundred labels, try logistic regression instead.
- [ ] Decision prediction API: predict a choice for a new situation, *with the precedents behind it*. This is the core "knows how you decide" claim. `v2` `L`
- [ ] Monte Carlo tree search for dreaming (proposing hypotheses), an adversarial verifier for claims, and minimax regret for trigger actions. `research` `L`

## 5. Sleep cycle

- [ ] MinHash/LSH dedupe, replacing pairwise Jaccard and cosine, so it scales. `v2` `M`
- [ ] HDBSCAN and Leiden clustering and an isolation forest for outliers, replacing union-find hubs. These live in the Python ML workers (§13). `v2` `L`
- [ ] Sleep hours learned from activity after two weeks, changed only with approval. `v2` `M`
- [ ] Don't start on battery (laptops); pause a running cycle when the user starts working. Today it checks the window and 20 minutes of idle at start only. `polish` `M`
- [ ] Quiet notifications: hold non-urgent pushes during sleep hours for the morning report, with user-chosen urgent kinds. `v2` `S`
- [ ] Per-region changes in the morning report. `polish` `S`

## 6. Testing and drift

- [ ] Kolmogorov-Smirnov test alongside PSI for drift. `polish` `S`
- [ ] Automatic rollback to the last healthy snapshot on a severe breach. Today it prints the restore command. `v2` `M`
- [ ] Generated tests from the graph: hide a decision and predict it. `v2` `M`
- [ ] Mutation tests for contradictions (only duplicates, secrets and runaway hubs today). `polish` `S`
- [ ] A small adversarial agent probing for prompt injection and scope leaks. `v2` `L`
- [ ] Bounds not measured yet: code sparsity (needs §2), forgetting-curve power-law fit, interference as the graph grows, result diversity, activation convergence, confidence calibration, trait stability. `v2` `M`
- [ ] Software tests from the design: property-based invariants (weights in range, provenance always present, scopes never leak), store parity once Postgres exists, and security fuzzing of scopes. Protocol conformance and the 100k-memory performance test exist (0.11.0). `M11` `L`

## 7. Triggers, push and awareness

- [ ] Queue triggers: Kafka and Redis Streams. `v2` `M`
- [ ] File-change triggers. `v2` `S`
- [ ] Native cron schedules. Today system cron wraps `engram job run`. `v2` `S`
- [ ] Trigger actions: call a webhook or run a procedure when a threshold is crossed, then feed the outcome back. Needs §9 sandboxed procedures and first-run approval. `v2` `L`
- [ ] "While you were away" briefing of what agents and triggers did since you last checked in. `v2` `M`
- [ ] Cache embeddings of subscription contexts. `Dispatch` re-embeds each subscription's context per event. `polish` `S`
- [ ] Optional TLS for `engram listen` (today it expects a tunnel or proxy in front for GitHub). `polish` `S`

## 8. Sessions and handoff

- [ ] Automatic checkpoints at milestones and session end via client hooks, not only when the agent remembers to call `checkpoint`. `v2` `M`
- [ ] Cross-device resume, which needs sync (§11). `v2` `L`

## 9. Packs and procedures

- [x] Pack registry: public, private (org) and local visibility; versioning and updates. `M11` `L`
- [x] Pack signing and verification; exports that strip private memories and secrets. `M11` `M`
- [ ] Source packs: rules that map a system into the graph (for example a GitHub repo's modules, import graph and reviews). `v2` `L`
- [x] Instance packs: export your own learned subgraph to share with a teammate. `v2` `M`
- [x] Usage reweighting stats per pack, for the dashboard. `polish` `S`
- [ ] Executable procedures in a sandbox with declared permissions and first-run approval: a declarative step list of MCP calls first, then WebAssembly, Starlark or Deno-style permissions if general code is needed. `research` `L`

## 10. Dashboard

- [ ] Thought-path view: why a memory was recalled, its maximin path and weakest link, with weaken or remove actions. Needs `engram/path`. `v2` `M`
- [ ] Decisions and traits view: approve, edit or reject candidate traits and procedures. `v2` `M`
- [ ] Inbox additions: live permission requests (once auto-approval exists), trait and procedure approvals, from every agent and device. `v2` `M`
- [x] Packs view: install, update, uninstall, export, and how use reweighted each pack. `M11` `M`
- [ ] Consent management for teammates (§12). `v2` `M`
- [ ] Cost metrics (LLM spend per cycle) on the overview. `polish` `S`
- [ ] Rotatable 3D graph; the 2D force layout is O(n²) and capped at about 220 nodes. `polish` `M`
- [ ] Hosted dashboard (Engram Cloud or a published page) exporting only aggregates and the morning report by default. `v2` `L`
- [ ] Show integer bounds such as hub degree without decimals. `polish` `S`

## 11. Storage, caching, sync and observability

- [ ] `Store` interface with a shared test suite, then a Postgres backend with `pgvector` and row-level security. Deferred from M11 to its own milestone: about 150 SQL call sites use SQLite-specific features (FTS4, rowid-based index refresh, `VACUUM INTO` snapshots, `INSERT OR IGNORE`). `teams` `L`
- [x] Vector index: exact in-memory index plus an FTS4 full-text index for candidates (0.11.0); recall at 100k memories measured at p50 46 ms on 2 cores. `M11` `M`
- [ ] Caching ("myelination"): embedding cache, hot subgraph, precomputed paths between busy hubs, recall and attend results, and LLM outputs, invalidated per graph region; batched Hebbian updates. `v2` `L`
- [ ] Sync between devices and teammates with CRDTs; weight changes as additive deltas. `research` `L`
- [ ] OpenTelemetry: latency, cache hits, cycle duration, health bounds, LLM cost. `v2` `M`

## 12. Privacy, security and trust

- [ ] Encryption at rest, and end-to-end encryption for cloud sync. `M11` `L`
- [ ] Legal deletion: purge caches and models, and crypto-shred backups. `v2` `M`
- [ ] Teammate consent: opt in by source, see what Engram knows about you, remove your contributions. `v2` `L`
- [ ] Re-approval per session for namespaces marked sensitive. `v2` `S`

## 13. Integrations and the Python ML layer

- [ ] "Octopus legs": Engram as an MCP client to GitHub, Gmail, Slack, Calendar and Linear, turning their changes into episodes. `v2` `L`
- [ ] Observers: capture agent turns through Claude Code hooks and CLI wrappers; a consented browser extension for assistant web apps. `v2` `L`
- [ ] Python ML workers reading and writing through the store, with versioned models and rollback, for: HDBSCAN, Leiden, isolation forest, GNN link prediction (GraphSAGE or GAT), learning-to-rank, decision prediction (gradient-boosted trees or logistic regression), calibration, and monthly contrastive fine-tuning of the embedding model. Hand-set rules until a few hundred feedback events exist. `v2` `L`
- [x] Headless service identities with their own tokens, writing as `author: "system"` (with §1 HTTP auth). `M11` `M`

## 14. Permission assist

- [ ] Auto-approval per tier, only after the report shows readiness (50 resolved allow predictions at precision ≥ 0.98), with a per-tier switch. `v2` `M`
- [ ] "Unusual means ask": inputs unlike any precedent, actions following web or email content (taint tracking), and an anomaly detector. `v2` `L`
- [ ] Classify MCP tools (read-only vs. writes) instead of always asking. `v2` `M`
- [ ] Learned permission model: gradient-boosted trees (or logistic regression at first) over the shadow-mode log, replacing the Beta approval count. Features: tier, command and arguments, file types, working directory, time of day, recent failures, client. Same Python-train / Go-score split as learning-to-rank. Ships only if the agreement report shows it beats the current rule. A good first ML item: the data and the success metric already exist. `v2` `M`
- [ ] Daily audit review of auto-approvals with their precedents. `v2` `S`
- [ ] Other hosts' permission hooks (Codex, Cursor) if they expose them. `v2` `M`
- [ ] Re-check the Claude Code hook semantics as they evolve. Denials are inferred because no hook fires on a manual deny, and `PermissionRequest` carries no `tool_use_id`. `polish` `S`

## 15. Packaging (M11)

- [x] `engram init`: one-command setup (database, embeddings, LLM, sleep hours, pairing, hooks). Install under five minutes. `M11` `M`
- [x] Project absorption: `engram absorb` and `init --scan` find repositories and absorb history, agent rules files, docs and stack, incrementally (0.12.0). `polish` `M`
- [x] Interactive full-screen `engram init` with a project picker, review screen and live progress; `--plain`, `--yes` and `--dry-run --json` for scripts and agents (0.12.0). `polish` `M`
- [ ] Browser setup wizard in the dashboard, for people who don't live in a terminal: the same choices as the terminal setup, with the absorb preview and the map drawing itself. `polish` `M`
- [ ] Re-absorb known projects in the sleep cycle (new commits, edited docs), so the map stays current without running `absorb`. `polish` `S`
- [ ] Absorb a claude.ai or ChatGPT data export: a chat model extracts decisions and facts per conversation (one call per chunk, no agent loop), routed to project namespaces, with a keep/discard preview before anything is stored. `v2` `M`
- [ ] Absorb issues and pull requests (GitHub, Jira) during the scan, linked to the commits and files they touched. `v2` `M`
- [ ] A short agent skill for driving setup: read `init --dry-run --json`, ask the user, then run `absorb` per project. `polish` `S`
- [x] Release builds and install paths (GitHub releases, Homebrew), with cgo SQLite cross-compiles. `M11` `M`
- [x] Docker Compose for self-hosting: engine, trigger listener, dashboard, sleep loop and optional Ollama, on SQLite (0.11.0); Postgres, Redis and ML workers join when they exist. `M11` `M`
- [ ] Engram Cloud: hosted sync, team memory, backups. `research` `L`

## 16. Evaluation and publishing (M12)

- [ ] LoCoMo and LongMemEval: accuracy and context tokens against HeLa-Mem, Mem0, Zep and plain RAG. `M12` `L`
- [ ] Held-out decision prediction: top-1 and top-3 accuracy on hidden real decisions vs. text-note memory and kNN without Hebbian edges. `M12` `L`
- [ ] Cross-agent transfer: task success and re-explanation turns vs. isolated per-agent memory. `M12` `M`
- [ ] Encoding: noisy recall, capacity, bind fidelity and latency of sparse codes vs. dense only. `M12` `M`
- [ ] Echo resistance: weight drift under repeated restatement, with and without anti-echo rules. `M12` `S`
- [ ] Per-mechanism ablations. `M12` `L`
- [ ] Engineering targets: recall under 100 ms at 100k memories. `M12` `M`
- [ ] Paper draft. `M12` `L`

## 17. Open design questions

- [ ] Sparse code parameters: block count, block length, projection method.
- [ ] Engine license: Apache-2.0 (current) or AGPL.
- [ ] Entity-resolution threshold and merge-reversal policy.
- [ ] Consolidation cadence and the cost of LLM distillation.
- [ ] `attend` salience weights: hand-set today (relevance, goals, recency, kind, conflicts); tune or learn them once feedback exists.
- [ ] Hashing-embedder fallback: keep it, or require an embedding server for anything beyond a demo?

## Known rough edges in what's built

Small things to revisit during refinement; none block use.

- Stdio identity is a consent gate, not authentication: any local process can claim a client name (fixed by §1 HTTP auth).
- Each serving process polls the store every 2 s for notifications; fine locally, wasteful at scale.
- Permission tiers are regex rules; expect false "unknown"s for legitimate commands and keep tuning the lists.
- Distillation and verification need a chat model; without one they're skipped, not faked.
- Git import labels repositories imported before 0.10.0 from `.` as ".".
- Multiple processes share one SQLite file with a 5 s busy timeout; heavy concurrent writers may still see lock waits.
- Packs installed before 0.11.0 have no recorded priors, so their reweighting stats stay at zero until reinstalled.
- `engram/consolidate` cannot target one namespace yet, and needs a write grant on `*`.
- Absorbing a long `CLAUDE.md` or a docs-heavy repo produces many sections (EasyScript's 600-line `CLAUDE.md` became about 150), and kNN wiring adds up to several `similar` edges each; watch hub degree on large imports.
- The terminal setup asks the terminal for its background color; terminals that never answer delay start-up by a few seconds (a Bubble Tea/termenv behavior).
- The release workflow, Homebrew formula, Docker image and `install.sh` are written and statically checked, but have not run yet: their first real run is the first `v*` tag and CI build after `go.sum` is fixed.
