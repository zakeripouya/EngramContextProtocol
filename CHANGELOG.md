# Changelog

All notable changes are recorded here. The protocol version (ECP) and the engine version are tracked separately.

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
