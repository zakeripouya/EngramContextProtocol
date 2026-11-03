# ECP SDKs

Small clients for the [Engram Context Protocol](../docs/spec/ecp-v0.md), over HTTP (an ECP server's `/ecp` endpoint and a bearer token) or stdio (a server process such as `engram serve`). Types follow the normative schema in [`spec/schema.ts`](../spec/schema.ts).

| Language | Path | Install | Dependencies |
| --- | --- | --- | --- |
| Go | [`sdk/go/ecpclient`](go/ecpclient) | `go get github.com/zakeripouya/EngramContextProtocol/sdk/go/ecpclient` | none beyond the standard library |
| TypeScript (Node 20+) | [`sdk/typescript`](typescript) | `npm install engram-ecp` *(not yet published; build from this folder)* | none at runtime |
| Python 3.9+ | [`sdk/python`](python) | `pip install engram-ecp` *(not yet published; `pip install ./sdk/python`)* | none beyond the standard library |

## Connecting

Start a server and issue a token for your program:

```bash
engram pair my-script --read me --write me
engram token create my-script            # prints ecp_…
engram serve --http 127.0.0.1:7079       # ECP at /ecp, MCP at /mcp
```

```go
c, err := ecpclient.DialHTTP(ctx, "http://127.0.0.1:7079/ecp", token, ecpclient.Info{Name: "my-script"})
res, err := c.Observe(ctx, ecpclient.ObserveParams{Kind: "fact", Content: "Invoices go out on the 1st"})
hits, err := c.Recall(ctx, ecpclient.RecallParams{Query: "when do invoices go out"})
```

```ts
import { ECPClient } from "engram-ecp";
const c = await ECPClient.http("http://127.0.0.1:7079/ecp", token, { name: "my-script" });
await c.observe({ kind: "fact", content: "Invoices go out on the 1st" });
const hits = await c.recall({ query: "when do invoices go out" });
```

```python
from engram_ecp import Client
with Client.http("http://127.0.0.1:7079/ecp", token, name="my-script") as c:
    c.observe("fact", "Invoices go out on the 1st")
    hits = c.recall("when do invoices go out")
```

Over stdio, the SDKs start the server themselves: `ecpclient.DialCommand(ctx, "engram", []string{"serve", "--client", "my-script"}, info)`, `ECPClient.stdio("engram", ["serve", "--client", "my-script"], info)`, `Client.stdio(["engram", "serve", "--client", "my-script"], name="my-script")`. Pair the client first; stdio needs no token.

**Notifications.** Declare the ones you want when connecting (`relevant`, `conflict`, `consolidated`), subscribe to a context, and read them from `Notifications()` (Go) or a handler (`onNotification`, `on_notification`). Over HTTP, call `Listen` / `listen()` to open the event stream.

**Errors** carry the spec's codes: `ecpclient.IsCode(err, ecpclient.CodeScopeDenied)`, `e instanceof ECPError && e.code === Codes.ScopeDenied`, `except ECPError as e: e.code == Codes.SCOPE_DENIED`.

## Tests

`sdk/test.sh` builds Engram, starts it with a temporary database, and runs the TypeScript and Python suites against it. The Go client is tested with `go test ./sdk/... ./conformance/...`.

The TypeScript package keeps a copy of the schema in `src/schema.ts`; run `npm run sync-schema` after changing `spec/schema.ts`.
