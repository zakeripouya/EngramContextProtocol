# Changelog

All notable changes are recorded here. The protocol version (ECP) and the engine version are tracked separately.

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
