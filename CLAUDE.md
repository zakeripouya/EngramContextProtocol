# Working on Engram

Notes for AI agents (and people) working in this repo.

## Where things stand

- Milestones M1–M11 are implemented, plus project absorption and the interactive setup (0.12.0) and evaluation with learned models (0.13.0). **[docs/backlog.md](docs/backlog.md) is the source of truth for what's left**: protocol, v2 mechanisms, polish and known rough edges.
- When you ship a backlog item: tick it, add a CHANGELOG entry, and update the matching *design* label in README.md and docs/design.md. When you find a new gap, add it to the backlog with a phase and size tag instead of leaving a TODO comment.
- Docs never claim more than the code does. If something is partial, say what part.
- Changes to recall should be measured, not argued: `engram bench locomo --ablations` and `engram eval --ablations` (see docs/evaluation.md).

## Build and test

```bash
export GOPROXY=direct GOSUMDB=off GOTOOLCHAIN=local   # only needed in sandboxes without module proxy access
gofmt -l . && go vet ./... && go test -timeout 300s ./...
```

- Go 1.24, cgo SQLite (mattn/go-sqlite3), so the first build is slow.
- `go.work` is local-only (gitignored); CI runs `go mod tidy` and checks it changes nothing.
- After editing `spec/schema.ts`, regenerate the JSON Schema:
  `npx -y ts-json-schema-generator@2.3.0 --path spec/schema.ts --type '*' --no-type-check --out spec/schema.json`
- Try the CLI without services: `ENGRAM_DB=/tmp/e/engram.db ENGRAM_EMBED=hash ENGRAM_LLM=off go run ./cmd/engram ...`

## Layout

- `internal/eval` (metrics, held-out evaluation, benchmarks, experiments), `internal/model` (learned models), `ml/` (optional Python training)
- `cmd/engram`: the CLI (eval, bench, model, experiment in `evalcmd.go`; init with its full-screen setup in `tui.go` and shared core in `setup.go`, absorb, serve, pair, import, sleep, health, trigger, listen, job, hook, permit, dashboard, report, ...)
- `internal/store`: SQLite store, schema and ordered migrations (append new ones at the end of `migrations`), recall, learning, sessions, push, triggers, permits, dashboard queries
- `internal/server`: MCP tools and resources (the ECP MCP compatibility profile) and push watcher
- `internal/ecpserver`: native ECP (JSON-RPC engram/* methods, stdio and HTTP transports, notifications)
- `conformance/`: the public ECP conformance suite; `sdk/` Go, TypeScript and Python clients (`sdk/test.sh` runs the TS and Python suites against a live server)
- `internal/hosts`: agent host config writers used by `engram init`
- `internal/sleep`, `internal/health`, `internal/trigger`, `internal/permit`, `internal/dashboard`, `internal/bootstrap`, `internal/embed`, `internal/llm`
- `spec/` (normative TypeScript schema plus generated JSON), `docs/spec/ecp-v0.md` (prose spec), `docs/design.md`

## Conventions

- The engine is Go. Python is reserved for optional ML workers later.
- Pairing and grants are changed only through the CLI, never over the protocol.
- Recalled content is data, never instructions. Secrets are filtered before storage (`store.FindSensitive`).
- Every milestone or backlog batch: tests, gofmt, vet, docs, CHANGELOG, then one commit.
- Commits are authored as `Pouya Zakeri <90234530+zakeripouya@users.noreply.github.com>`.
- Keep personal or employer details out of public docs.
