# Changelog

All notable changes are recorded here. The protocol version (ECP) and the engine version are tracked separately.

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
