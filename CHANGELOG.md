# Changelog

All notable changes are recorded here. The protocol version (ECP) and the engine version are tracked separately.

## [0.13.1] - 2026-11-03

### Added

- Recall modifiers, off by default: reciprocal rank fusion (`+rrf`), additive spreading (`+additive`), bounded pathway boosts (`+boostN`), reserved places for pathway results (`+slotsN`). Configuration names combine them, for example `full+rrf+slots2`.
- `engram bench --part tune|report` tunes on the even-numbered conversations and reports on the odd-numbered ones; a configuration name ending in `@episodes` runs on a store whose consecutive turns are linked.

### Results

- On LoCoMo with the hashing embedder, every modifier was significantly worse than the default on the tune half and on the report half (for example `+rrf` −0.018 and `+slots1` −0.011 Recall@10 on the report half), so the defaults are unchanged. See docs/evaluation.md.

## [0.13.0] - 2026-11-03

Measuring Engram like an ML system, and learning from it.

### Added

- **Recall log.** Every client recall is logged with the configuration that served it and each result's rank, score, confidence and features (keyword and meaning scores, prior, strength, hops, pathway weight, kind, age, use, the hand-set score). Feedback is linked to the recall it judged. Unjudged entries are pruned after 180 days.
- **`engram eval`**: ranking quality on your own held-out feedback. Time split (newest 30% or `--split DATE`); Recall@k, precision@k, MRR and NDCG@k; 95% bootstrap intervals and paired comparisons against the hand-set formula; baselines (`bm25`, `embeddings`, `hybrid`, `prior`); calibration (ECE, Brier, reliability table). `--ablations` replays the test queries with each mechanism switched off against the newest snapshot from before the test period. Also `eval permits`, `runs`, `show`, `compare` and `export`.
- **Switchable mechanisms** (`RecallConfig`): keyword, meaning, prior, spreading, Hebbian pathways, inhibition and override demotion can each be turned off, the constants changed, and `about` edges optionally spread (`with-about`). Default recall is unchanged.
- **`engram bench`**: LoCoMo and LongMemEval runners. Each conversation goes into a fresh store; retrieval is scored against the evidence turns per question type, with context tokens; `--qa` answers with the chat model and grades with an LLM judge; `--ablations` and `--link-turns` compare variants.
- **`engram model`**: a built-in pairwise linear ranker; XGBoost rankers trained by `ml/train_xgboost.py` and scored in Go (checked against XGBoost's own predictions); isotonic confidence calibration; a logistic-regression permission model. Each is stored with its held-out evaluation and activated only if it passes its gate (see docs/evaluation.md); `engram model off KIND` returns to the hand-set rules. Activated models serve real recalls and permission predictions, which the log records.
- **`engram experiment`**: A/B tests on live recalls. A share of recalls, assigned at random by recall id, gets the variant; status compares accept rate and MRR with an unpaired bootstrap.
- Health: a Kolmogorov–Smirnov test next to PSI for pathway-weight drift, and confidence calibration (ECE over the last 30 days) as a measured bound.
- CI job that runs the whole XGBoost loop: simulated feedback, export, Python training, Go scoring and the gate.
- [Evaluating Engram](docs/evaluation.md): the method, the gates and the first results.

### Results

- LoCoMo (10 conversations, 1,536 questions, k = 10, offline hashing embedder): Engram Recall@10 0.558 [0.53–0.58], BM25 0.560 [0.54–0.58]. Spreading, the prior and Hebbian learning make no measurable difference on a cold store; spreading never reaches the top 10. Not yet run with a real embedding model or a chat judge.

## [0.12.0] - 2026-11-03

Setup that absorbs what you already have.

### Added

- **`engram absorb [DIR|REPO ...]`**: absorb projects. For each git repository: its history (as `import git`), the instructions you already give agents (`CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `.cursorrules`, `.cursor/rules/*`, `.windsurfrules`, `.github/copilot-instructions.md`), its READMEs and docs (`docs/`, `adr/`, `decisions/`, `design/`, `rfcs/`, top-level `ARCHITECTURE.md` and similar; not changelogs or licenses), split at headings into sections, and its stack (languages by file share, frameworks from `package.json`, `go.mod`, `pyproject.toml` and `requirements.txt`, plus Terraform, Docker, Prisma and GitHub Actions). A folder is searched up to four levels deep, skipping hidden, dependency and media folders.
- Absorbed sections link to a concept per document, documents link to a concept per repository, and a section that mentions a tracked file (for example `api/payouts.go`) links to that file's concept from history.
- Only files git tracks are read, so `.gitignore` is respected and untracked secrets are never opened; sections that look like secrets are skipped and counted.
- Re-absorbing adds new commits, updates edited sections in place, retires (archives) sections and documents that are gone, and never resets learned weights (`AddEdgeIfAbsent`).
- `--dry-run [--json]` describes what would be stored; `--ns` names a single project's namespace; new namespaces are granted to the clients that can write your personal memory (`--grant`, `--no-grant`).
- **Interactive `engram init`** in a terminal: a full-screen setup (Bubble Tea) with model checks, an agent checklist, a project picker (scan any folder, toggle projects, rename namespaces, see what each one contributes), extras (sleep hours, scheduler, Developer pack, permission assist), a review screen, and live progress with memory and pathway counters. Its header is a small network that fires and spreads like recall does.
- `engram init --scan DIR`, `--plain` (yes/no prompts), `--dry-run [--json]` (the plan, including models, agents and projects, without changing anything), `--since`, `--max-commits`.

### Changed

- `engram init` absorbs projects into their own namespaces (`ink`, `easyscript`) instead of importing the current repository into `me`, and grants connected agents read and write on them.
- The plain prompts and the interactive setup share one implementation (`setupChoices` and `applySetup`), so they can't drift apart.
- New dependency: `github.com/charmbracelet/bubbletea`, `bubbles` and `lipgloss` (MIT), for the terminal UI only.

## [0.11.0] - 2026-11-03

Engine milestone M11: packaging and protocol. Not included: freezing ECP v1 (open issues remain) and the Postgres backend (its own milestone).

### Added

- Recall scales past 5,000 memories: an FTS4 full-text index (Porter stemmer, in the default SQLite build) and an exact in-memory vector index supply candidates, and spreading activation loads neighbors on demand. Measured at 100,000 memories on 2 CPU cores: p50 46 ms, p95 69 ms (`ENGRAM_SCALE=1 go test -run RecallLatency100k ./internal/store/`).
- The vector index refreshes incrementally from the database, so memories written by other processes (hooks, the trigger listener, jobs) are found without a restart. kNN wiring on insert uses the same index.
- **Native ECP.** JSON-RPC 2.0 `engram/*` methods with `initialize`, version and capability negotiation, batches, and the spec's error codes: `observe` (with `contradicts`), `recall` (`includePaths`), `get`, `path`, `attend`, `feedback`, `forget`, `subscribe`, `unsubscribe`, `consolidate` and `session/checkpoint | resume | list | delegate`.
- `engram/path`: the maximin (strongest weakest link) or most probable (−log weight) chain between two memories, through current memories the client may read.
- `engram/consolidate`: runs a sleep cycle; needs a write grant on `*`.
- Server-pushed `notifications/engram/relevant`, `conflict` and `consolidated`, only to clients that declared them, grant-filtered, and rate-limited per subscription. Conflicts and sleep cycles go through a shared event log, so whichever process caused them, every serving process announces them.
- `engram serve` detects the protocol from the first message on stdio (native ECP or MCP). `engram serve --http ADDR` serves native ECP at `/ecp` (POST, an SSE stream on GET, DELETE) and MCP at `/mcp` (Streamable HTTP).
- Bearer tokens: `engram token create CLIENT [--service] | list | revoke`. Tokens are stored hashed, and the token, not `clientInfo`, decides identity. Service tokens write as `author: "system"`.

- **Conformance suite** (`conformance/`, `engram conformance --url URL --token T` or `engram conformance -- COMMAND`): 27 checks of the spec's lifecycle, JSON-RPC, memory, provenance, bitemporal, scope and optional-capability requirements, over HTTP or stdio, against any ECP server. Engram passes all applicable checks on both transports, and CI runs the suite.
- **SDKs**: Go (`sdk/go/ecpclient`), TypeScript (`sdk/typescript`, Node 20+) and Python (`sdk/python`, 3.9+), with no runtime dependencies, over HTTP or stdio, including notifications. `sdk/test.sh` runs the TypeScript and Python suites against a live server.
- Ending an HTTP session (`DELETE /ecp`) closes its event stream.
- **`engram init`**: one-command setup. It opens the database, checks the embedding and chat models, connects Claude Code (`claude mcp add --scope user`), Claude Desktop, Codex and Cursor by editing their MCP config (idempotent, with backups), pairs them, sets sleep hours, installs a scheduler (cron, or launchd on macOS), installs the Developer pack, imports the current git repository, and optionally adds the permission-assist hooks. `--yes` accepts every recommended step.
- `~/.engram/config`: `ENGRAM_*` settings saved by `init --embed-url/--embed-model/--llm-url/--llm-model`. Environment variables still win.
- **Releases**: a tag `v*` builds native binaries for Linux (amd64, arm64), macOS (arm64, amd64) and Windows (amd64) with checksums, and pushes a container image to GHCR. `install.sh` downloads and verifies the right binary. A Homebrew formula lives in `packaging/homebrew/`.
- **Docker**: a multi-stage `Dockerfile` (non-root, `/data` volume) and `docker-compose.yml` with the engine, trigger listener, dashboard and sleep loop sharing one volume, published on 127.0.0.1 only, plus an optional Ollama profile. `engram dashboard --container` binds every interface but still answers only requests addressed to localhost.
- **Pack registry**: signed packs (Ed25519; `engram pack keygen | sign | trust | untrust | keys`). Registries can be a local directory, a public https URL or a private URL with a token from an environment variable (`engram pack registry add | list | remove`), each a static `index.json` plus pack files with SHA-256 checksums. Also `engram pack search`, `install NAME[@VERSION]`, `update`, and `publish FILE --to DIR`. Registry packs must be signed by a trusted key; local files must be signed or explicitly allowed.
- Pack updates add new memories, change edited ones in place and archive dropped ones, and never reset learned pathway weights. Install-time priors are recorded, so `engram pack list` and the dashboard show how use has reweighted each pack.
- `engram pack export NAMESPACE --name NAME [--sign]`: an instance pack of a namespace's current facts, concepts and procedures and their learned pathways. Episodes, decisions, suggestions and secret-looking content are never exported.
- Dashboard **Packs** view: installed packs with verification and reweighting, built-in installs, removal, and export as a download.

### Fixed

- The static report failed to render nights with distilled facts (it read a field that doesn't exist).
- Removing a pack left its import keys behind, so reinstalling it installed nothing.
- Reinstalling a pack reset its learned pathway weights to the pack's priors.

## [0.10.0] - 2026-11-03

Engine milestone M10: the dashboard. This completes milestones M1 to M10.

### Added

- `engram dashboard`: a local web dashboard embedded in the binary. Views: overview, approvals inbox, graph explorer, memory search, sleep changes with undo, health with the sandbox self-test, triggers with pause and resume, sessions, agents with revoke, and permission-assist agreement.
- Approvals inbox: keep or reject facts distilled during sleep (with their sources), and resolve conflicts by keeping one side (the other is archived as superseded).
- Security: loopback only, a random per-run token in a header, a loopback `Host` check against DNS rebinding, a strict Content-Security-Policy, and memory content always rendered as text.
- `engram report --html` and an automatic `report.html` (mode 0600) after each `engram sleep run`: a read-only snapshot with no scripts.

### Fixed

- Git imports from a relative path (`engram import git .`) name the repository after its directory instead of ".".

## [0.9.0] - 2026-11-03

Engine milestone M9: permission assist, shadow mode only.

### Added

- `engram hook claude-code`: a Claude Code hook for `PermissionRequest`, `PostToolUse` and `PostToolUseFailure` that logs each permission prompt with Engram's prediction, then the user's decision and the outcome. It never prints a decision and never fails the host. `engram setup claude-code` prints the settings.
- Rule tiers (eligible, never, unknown) for Read, Edit, Write, Bash, web and MCP tools. Bash lines are split into simple commands, and every part must be eligible.
- Predictions per category from precedents, with outcome-aware confidence: allow needs 8 precedents and ≥ 0.90.
- Denials inferred when the session moves on without running the tool; a later run corrects them.
- `engram permit report [--json]`, `engram permit explain TOOL INPUT`, `engram permit on | off`.

### Changed

- The database is opened with a 5-second busy timeout, since agents' servers, hooks, the trigger listener and jobs share it.

## [0.8.0] - 2026-11-03

Engine milestone M8: triggers and push.

### Added

- `attend` tool: what matters now, ranked by relevance, overlap with active goals, recent change, unresolved conflicts and a kind preference; modes `detail`, `explore`, `focus` and `generalize`; at most 12 items; never trains pathways; unread notifications under `pending`.
- Conflicts: `observe` with `contradicts` records a `conflicts` edge, surfaced by `attend` until one memory supersedes the other.
- Triggers: `engram trigger add | list | events | pause | resume | remove`; `engram listen` serves `POST /hooks/NAME` (bearer token, or GitHub HMAC-SHA256 for workflow runs, pull requests, issues, comments and pushes) and `POST /jobs/NAME`. Rate limit of 60 events a minute, idempotent redeliveries, secret filter.
- `engram job run NAME -- COMMAND` wraps an existing job (output and exit code pass through) and `engram job report` records one that ran elsewhere.
- Push: `subscribe` and `unsubscribe` tools, the subscribable `engram://attention` resource with `notifications/resources/updated`, and `engram://memory/{id}`. Notifications respect the subscriber's grants, never repeat a memory for a subscription, and are rate-limited per connection.

### Protocol

- Additive: `ObserveParams.contradicts`, `SubscribeParams.namespaces`, `AttendResult.pending` and `PendingNotification`.

## [0.7.0] - 2026-11-03

Engine milestone M7: a second agent and sessions.

### Added

- MCP tools `checkpoint`, `sessions`, `resume` and `delegate` (ECP `engram/session/*`): any paired agent can continue a task another agent started.
- `resume` reports what the resuming agent lacks (the repository, compared by owner and name; tools; write access) under `missing`, so it asks instead of assuming.
- Finished tasks are recorded as episodes; a finished delegated subtask appends its result to the parent task.
- Partial checkpoint updates keep the fields left out; checkpoints pass the secret filter and respect namespace grants.
- `engram sessions`, `engram session show`, and `engram setup codex | cursor`.

### Protocol

- `Checkpoint` gains optional `status`, `result`, `parentId`, `delegatedTo`, `createdBy`, `lastClient` and `createdAt`; `ResumeParams` gains `environment`; `ResumeResult` gains `subtasks` and `memories`; `session/list` returns `sessions`. All additive.

## [0.6.0] - 2026-11-03

Engine milestone M6: the testing harness.

### Added

- `engram health [--json]`: weight saturation, memories at the strength floor, largest hub degree and decision recall, each with healthy, warning and breach limits. Bounds that cannot be measured yet are listed, not faked.
- Drift detection: the population stability index of the pathway-weight distribution against the mean of the last week. Metrics are recorded after every `engram sleep run`, and a severe breach prints the snapshot restore command.
- `engram selftest`: runs on a sandbox copy, never the real memory. Golden queries, superseded facts not returned as current, and mutation tests (an injected duplicate must be merged, a poisoned memory with a secret refused, a runaway hub must breach the degree bound).
- `engram golden add | list`.

## [0.5.0] - 2026-11-03

Engine milestone M5: the sleep cycle.

### Added

- `engram sleep run`: snapshot, decay, homeostatic downscaling of unused memories, near-duplicate merging, archiving of faded memories, hub detection, LLM distillation verified by a separate call, a recall self-test of user decisions, and a stored morning report.
- Distilled facts link to every source with `derived` edges; unsupported claims are reported and discarded; a hub is never distilled twice, and distilled facts never join new hubs.
- `engram sleep hours` with `--if-due` scheduling: inside the window, after 20 minutes of agent quiet, once a night. `engram sleep schedule` prints cron and launchd entries.
- `engram sleep report` and `engram sleep undo`: every merge, archive and distilled fact is logged and reversible. Nothing is deleted outright.
- Snapshots before each cycle (newest 7 kept); `engram snapshot list | restore`.
- Chat client for any OpenAI-compatible `/v1/chat/completions` server (`ENGRAM_LLM*`, Ollama `llama3.2` by default); language steps are skipped, never faked, when no model answers.

## [0.4.0] - 2026-11-03

Engine milestone M4: learning from feedback and better retrieval.

### Added

- `feedback` tool: report accepted or overridden memories, real outcomes (`succeeded`, `failed`) and corrections. A correction is stored as a user decision linked by an `overrode` edge, and overridden memories are demoted in later recalls.
- Three-factor learning: recall gives co-activated pairs half a Hebbian step; confirming feedback gives the rest, contradicting feedback halves the pathway. Failed outcomes count as negative even when approved.
- ACT-R base-level activation (frequency and recency) as a ranking prior alongside strength.
- Multi-hop spreading activation: a beam of 10 over up to 3 hops with a 1/√degree fan penalty; results carry their full path.
- Lateral inhibition: near-duplicate results collapse to the best one.
- A confidence on every result; noise below a floor is dropped rather than padded.
- Feedback events are stored as training data for later ranking models.

### Changed

- Recall's `spread` now accepts 0 to 3 hops (default 2).
- Database migrations are defined in one ordered list.

## [0.3.0] - 2026-11-03

Engine milestone M3: bootstrapping, so a new Engram starts out knowing something.

### Added

- `engram import git`: one episode per commit, a concept per recurring file, `about` edges, `coactive` pathways between files that repeatedly change together, and facts for detected conventions (ticket-key branch names and commit messages, Conventional Commits).
- `engram import jira` for saved Jira search exports and `engram import json` for generic records.
- Pathway packs: the pack format, a built-in Developer pack, and `engram pack list | install | remove`.
- Imported and pack memories carry `system` provenance, start at low strength, and are idempotent: importing twice changes nothing. Imports can never create decisions.

## [0.2.0] - 2026-11-03

Engine milestone M2: the memory becomes a learning graph.

### Added

- Embeddings through any OpenAI-compatible `/v1/embeddings` server (Ollama with `nomic-embed-text` by default), configured with `ENGRAM_EMBED*`, plus an offline hashing embedder used automatically when no server answers.
- kNN wiring: each new memory links to up to 8 similar memories in its namespace with `similar` edges.
- Hebbian learning: memories recalled together strengthen a `coactive` edge (w ← w + η(1 − w)). Anti-echo: a pair counts once per client per hour.
- Decay and pruning of learned edges (`engram decay`).
- Pathway recall: keyword and vector seeds plus one hop of spreading activation; results reached through a pathway carry their path and a reason.
- `engram reembed` to embed memories stored before an embedding server was available.
- Database migrations (`PRAGMA user_version`); M1 databases upgrade automatically.

### Changed

- Recall ranks by meaning and keywords together when vectors exist, and takes a `spread` parameter (0 or 1).
- `observe` results include `embedded`.

## [0.1.0] - 2026-11-03

First public draft. Engine milestone M1; protocol ECP `2026-11-01` (draft).

### Added

- Design document and ECP v0 specification, with the normative schema in `spec/schema.ts`.
- `engram` engine in Go: MCP server over stdio with `observe`, `recall`, `get` and `forget` tools (the ECP MCP compatibility profile).
- SQLite store with namespaces, provenance, bitemporal validity (`supersedes`, `as_of`), forget with cascade and an audit log.
- Client pairing through the CLI (`pair`, `clients`, `revoke`) with per-namespace read and write grants and optional expiry.
- Secret filter that refuses keys, tokens, private keys and payment card numbers.
- BM25 ranking for recall (replaced by embeddings in M2).

### Protocol status

| Method or capability | Status |
| --- | --- |
| `engram/observe`, `engram/recall`, `engram/get`, `engram/forget` | draft, implemented over the MCP profile |
| Native ECP methods over JSON-RPC, `attend`, `feedback`, `subscribe`, sessions | draft, not implemented |
