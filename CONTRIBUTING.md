# Contributing

Thanks for your interest in Engram. The project is pre-alpha, so the most useful contributions right now are issues: design questions, gaps in the spec, and bug reports.

## Build and test

Requirements: Go 1.24 or newer and a C compiler (the SQLite driver uses cgo).

```bash
go build ./...
go test ./...
go run ./cmd/engram help
```

Point `ENGRAM_DB` at a scratch file while experimenting so your real memory stays untouched:

```bash
ENGRAM_DB=/tmp/engram-dev.db go run ./cmd/engram stats
```

To inspect the server's raw protocol traffic, run it under the MCP Inspector:

```bash
npx @modelcontextprotocol/inspector go run ./cmd/engram serve --client inspector
```

(Pair `inspector` first: `go run ./cmd/engram pair inspector --read me --write me`.)

## How changes are made

- **Code** follows the milestones in the [roadmap](docs/design.md#roadmap). Pick up work from an open issue, or open one first for anything larger than a bug fix.
- **Protocol changes** start as an issue labeled `spec`. Edit [`spec/schema.ts`](spec/schema.ts) and [`docs/spec/ecp-v0.md`](docs/spec/ecp-v0.md) together; the schema is normative.
- **Brain-inspired mechanisms** land behind a switch with an ablation result. A mechanism stays on by default only if it measurably helps (see the design document, "Neuroscience grounding").
- Every change that affects behavior needs a test. Every user-visible change gets a line in [CHANGELOG.md](CHANGELOG.md).

## Code conventions

- Run `gofmt` and `go vet` before committing.
- Return `*ecp.Error` with the matching code for anything a client could cause; plain errors are internal faults.
- Tool descriptions are written for the model that reads them: say when to call the tool, not only what it does.
- Never log or store memory content in error messages or the audit log beyond ids.

## Commit messages

Use the imperative mood and reference the milestone or issue, for example `store: add bitemporal as-of recall (M1)`.

## License

By contributing, you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).
