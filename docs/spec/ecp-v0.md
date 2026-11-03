# Engram Context Protocol (ECP) v0 Specification

**Status:** draft · **Protocol version:** `2026-11-01` · **Normative schema:** [`spec/schema.ts`](../../spec/schema.ts) · **Design:** [Engram Design Document](../design.md)

## Overview

ECP is an open protocol that lets AI agents share one persistent, associative memory: they report what happens, recall what is relevant, ask what matters now, and receive notifications when something becomes relevant. Engram is its reference implementation.

ECP extends the [Model Context Protocol](https://modelcontextprotocol.io) (MCP) rather than replacing it. It reuses MCP's wire format, transports and capability negotiation, and every ECP server also exposes an MCP-compatible tool surface, so existing MCP clients work unchanged.

Breaking changes are expected until v1.

**Conventions.** The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY are used as in [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119). Method names use the form `namespace/verb`. The TypeScript in [`spec/schema.ts`](../../spec/schema.ts) is normative; where this document and the schema disagree, the schema wins and the disagreement is a bug.

## Roles

ECP defines four roles; one process MAY play more than one.

| Role | Responsibility | Examples |
| --- | --- | --- |
| Host | The application the user interacts with; runs one client per server | Claude Code, Cursor, an agent framework |
| Client | Connects to a server, sends requests, receives notifications | The ECP connector inside a host |
| Server | Owns the memory: stores, learns, retrieves, notifies | Engram |
| Observer | A client that only writes episodes on behalf of a source | A Claude Code hook, a GitHub webhook bridge |

A server MUST attribute every write to an identified client, and MUST NOT accept writes from an unidentified client over HTTP.

## Wire format and transports

All messages are JSON-RPC 2.0, UTF-8 encoded.

- **Request:** has `id`, `method` and optional `params`; the receiver MUST reply.
- **Response:** has the request's `id` and exactly one of `result` or `error`.
- **Notification:** has `method` and optional `params`, no `id`; the receiver MUST NOT reply.

```json
{"jsonrpc":"2.0","id":7,"method":"engram/recall","params":{"query":"branch naming","k":5}}
```

ECP uses MCP's transports unchanged.

| Transport | Use | Framing | Auth |
| --- | --- | --- | --- |
| stdio | Local server launched by the host | One JSON message per line on stdin and stdout | Inherited from the local user, plus pairing |
| Streamable HTTP | Remote or shared server | HTTP POST per message; server-to-client messages over a streamed response | OAuth 2.1 bearer token, REQUIRED |

Servers MUST support stdio. Servers offering team or cloud memory MUST support Streamable HTTP.

## Lifecycle

Every connection runs initialize, then operation, then shutdown. No other request MAY be sent before initialization completes.

1. **Initialize.** The client sends `initialize` with the protocol versions it supports, its capabilities and its identity.
2. **Respond.** The server replies with the chosen protocol version, its capabilities and server info. If no version is shared, it returns error `-32002` and the connection ends.
3. **Initialized.** The client sends the `notifications/initialized` notification. Normal operation begins.
4. **Operate.** Either side sends requests and notifications allowed by the negotiated capabilities.
5. **Shutdown.** For stdio, the client closes stdin; for HTTP, the client ends its session. Servers MUST flush pending writes before exit.

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
  "protocolVersion":"2026-11-01",
  "capabilities":{"notifications":{"relevant":true}},
  "clientInfo":{"name":"claude-code","version":"2.4.0","agent":"claude"}}}
```

```json
{"jsonrpc":"2.0","id":1,"result":{
  "protocolVersion":"2026-11-01",
  "capabilities":{"recall":{"spreadingActivation":true},"attend":{},"subscribe":{},"feedback":{},"consolidate":{}},
  "serverInfo":{"name":"engram","version":"0.1.0"}}}
```

## Capabilities

Each side declares what it supports at initialization; a feature MAY be used only if the relevant side declared it. Absent means unsupported.

| Side | Capability | Enables | Sub-options |
| --- | --- | --- | --- |
| Server | `observe` | `engram/observe` (always present in v0) | `kinds` |
| Server | `recall` | `engram/recall`, `engram/get`, `engram/path` | `spreadingActivation`, `maxK` |
| Server | `attend` | `engram/attend` | none |
| Server | `feedback` | `engram/feedback` | none |
| Server | `subscribe` | `engram/subscribe`, `engram/unsubscribe` and server notifications | `topics` |
| Server | `consolidate` | `engram/consolidate` and `notifications/engram/consolidated` | none |
| Server | `forget` | `engram/forget` | `cascade` |
| Server | `sessions` | `engram/session/*` | none |
| Client | `notifications` | Receiving server-pushed notifications | `relevant`, `conflict` |

Servers MUST support `observe` and `recall`. Everything else is OPTIONAL in v0. Unknown capability keys MUST be ignored.

## Data types

Defined normatively in [`spec/schema.ts`](../../spec/schema.ts). The essentials:

- **Memory:** `id`, `kind`, `content`, `tags`, `scope`, `namespace`, `strength`, optional `confidence`, `provenance`, and three times: `createdAt` (when Engram learned it), `validFrom` and `validTo` (when it was true in the world).
- **Node kinds:** `episode`, `concept`, `decision`, `suggestion`, `fact`, `trait`, `procedure`.
- **Edge kinds:** `similar`, `coactive`, `about`, `derived`, `supersedes`, `conflicts`, `accepted`, `overrode`, `cross`.
- **Provenance:** `agent`, `client`, optional `service` and `sessionId`, `author` (`user`, `agent` or `system`) and `at`.
- **Namespace:** every memory belongs to exactly one, such as `me`, `team/ink`, `pack/developer`, `ext/stripe-api` or `flow/ink-payout-job`.

Timestamps are ISO 8601 strings in UTC; ids are opaque strings.

## Methods

All methods are client-to-server requests.

| Method | Purpose | Capability | Required |
| --- | --- | --- | --- |
| `engram/observe` | Write an episode, decision, suggestion or fact | `observe` | Yes |
| `engram/recall` | Retrieve memories related to a query | `recall` | Yes |
| `engram/get` | Fetch memories by id | `recall` | Yes |
| `engram/path` | Return the best path between two memories | `recall` | No |
| `engram/attend` | Return what matters now for a given context | `attend` | No |
| `engram/feedback` | Report that recalled memories were accepted or overridden | `feedback` | No |
| `engram/forget` | Delete memories and, optionally, what was derived from them | `forget` | No |
| `engram/subscribe` | Request notifications for a context | `subscribe` | No |
| `engram/unsubscribe` | Cancel a subscription | `subscribe` | No |
| `engram/consolidate` | Trigger a consolidation cycle | `consolidate` | No |
| `engram/session/checkpoint` | Save in-progress task state | `sessions` | No |
| `engram/session/resume` | Load a checkpoint, reporting missing environment | `sessions` | No |
| `engram/session/list` | List open or finished sessions | `sessions` | No |
| `engram/session/delegate` | Hand a subtask to another agent | `sessions` | No |

**Provenance rules.** Servers MUST attach provenance from the identified client, never from client-supplied params alone. A memory of kind `decision` MUST have `author: "user"`, and clients MUST set that only when relaying a choice the user explicitly made. Agent proposals use kind `suggestion`. A `decision` without `author: "user"` is rejected with `-32006`.

**Recall.** `asOf` answers the query as of a past time using `validFrom` and `validTo`. Without it, memories whose `validTo` has passed are excluded. Results SHOULD carry a calibrated `confidence`, and servers SHOULD return fewer results rather than low-confidence padding.

**Feedback.** `objective` reports the real-world outcome (`succeeded` or `failed`). Servers MUST weight objective outcomes above approval alone, so that the memory does not learn to predict approval instead of correctness.

**Path.** The default rule is `maximin`: the path whose weakest edge is strongest. `most_probable` minimizes the sum of −log(weight).

### Sessions and handoff

Checkpoints carry in-progress work between agents and devices. `resume` returns `missing`: the environment requirements of the checkpoint (a repo, a tool) that the resuming client does not declare, so the agent can ask rather than assume.

- `checkpoint` without `sessionId` creates a session; with one, it updates it, and fields left out keep their values. A session never moves between namespaces.
- The resuming client SHOULD send its own `environment`. Servers SHOULD compare repositories by owner and name, so a local checkout matches its remote URL, and MUST list missing write access to the session's namespace.
- When a session becomes `done`, servers SHOULD record its goal and `result` as an `episode` with the session's id, and, for a delegated subtask (`parentId`), append the result to the parent's `done`.
- Checkpoints are subject to the same grants and content filters as memories.

### Pairing

A client MUST be paired before any request other than `initialize`. Pairing is completed by the user **outside the protocol** (the dashboard or a CLI prompt), so that no agent can grant itself access, and yields a grant per namespace: `read`, `write` or both, with an optional expiry.

- Requests from an unpaired client fail with `-32003` (unauthorized).
- Requests outside a client's grants fail with `-32004` (scope denied).
- Servers SHOULD let users require a fresh approval per session for namespaces they mark sensitive.

Over stdio, pairing identifies a client by the `clientInfo.name` it sends. That is a consent gate, not authentication: any local process running as the user can claim a name. Real authentication is the OAuth token on Streamable HTTP.

## Subscriptions and notifications

Subscriptions are ECP's main addition to MCP: the server pushes to a client when memory becomes relevant to what that client is doing.

| Notification | Params |
| --- | --- |
| `notifications/engram/relevant` | `subscriptionId`, `memories`, `reason` |
| `notifications/engram/conflict` | `a`, `b`, `summary` |
| `notifications/engram/consolidated` | `created`, `pruned`, `traits` |

- A server MUST send notifications only to clients that declared the matching client capability.
- A server MUST NOT include memories outside the subscriber's grants.
- A server SHOULD rate-limit `relevant` notifications per subscription (default: at most one per 30 seconds) and MUST NOT re-send a memory the subscriber already received for the same context.
- Subscriptions end when the connection ends; clients re-subscribe after reconnecting.
- **MCP profile.** The `subscribe` tool returns a subscription id and the resource `engram://attention`. A client subscribes to it with `resources/subscribe`; the server sends `notifications/resources/updated` when new notifications exist, and reading the resource returns them (`PendingNotification`) and marks them delivered. Servers SHOULD also return unread notifications under `pending` in `attend`, for clients without resource subscriptions.
- **Events.** Triggers (webhooks, job results) are configured by the user outside the protocol, like pairing. Each event is stored as an `episode` with `author: "system"` and the trigger as `service`. A subscriber is notified only if it can read the event's namespace; failed events MAY raise salience.

## MCP compatibility profile

Every ECP server MUST also behave as a valid MCP server, so MCP-only clients get core memory with no changes. If a client's `initialize` request uses an MCP protocol version, the server answers as an MCP server and exposes this mapping.

| ECP feature | MCP surface | Notes |
| --- | --- | --- |
| `engram/observe` | Tool `observe` | Same params as an input schema |
| `engram/recall` | Tool `recall` | Text content plus structured content |
| `engram/get` | Tool `get` | |
| `engram/attend` | Tool `attend` | |
| `engram/feedback` | Tool `feedback` | |
| `engram/forget` | Tool `forget` | MCP clients SHOULD confirm with the user before calling |
| Subscriptions | Resource `engram://attention` with `resources/subscribe` | The server updates the resource and sends `notifications/resources/updated` |
| Memory browsing | Resources `engram://memory/{id}` | Read-only |

Tool descriptions MUST be written for the model that reads them: they say *when* to call the tool, not only what it does. For example, `recall`: "Call before making a decision or plan, to follow the user's past choices and project conventions."

## Errors

Errors use the JSON-RPC error object: `code`, `message` and optional `data`. ECP reserves −32000 to −32019.

| Code | Name | When |
| --- | --- | --- |
| -32700 | Parse error | Invalid JSON |
| -32600 | Invalid request | Not a valid JSON-RPC object |
| -32601 | Method not found | Unknown method, or capability not negotiated |
| -32602 | Invalid params | Params fail the schema |
| -32603 | Internal error | Server fault |
| -32001 | Not initialized | Request sent before initialization completed |
| -32002 | Unsupported version | No shared protocol version |
| -32003 | Unauthorized | Unpaired client, or missing or invalid credentials |
| -32004 | Scope denied | Request outside the client's grants |
| -32005 | Memory not found | Unknown memory id |
| -32006 | Invalid provenance | A `decision` written without `author: "user"` |
| -32007 | Rate limited | Too many requests; `data.retryAfter` in seconds |
| -32008 | Content rejected | Content matched a filter (secrets, card numbers) and was not stored |

## Versioning and extensions

**Versions** are date strings (`YYYY-MM-DD`) and change only for breaking changes. Clients send every version they support; the server picks the newest shared one. Additive changes ship as new capabilities under the same version.

**Extensions.** Implementations MAY add methods under a vendor namespace (`x-<vendor>/<verb>`, such as `x-engram/graph`) and fields with an `x_` prefix. Receivers MUST ignore unknown fields and MUST answer unknown methods with `-32601`.

**Stability.** Each method and capability is marked draft, stable or deprecated in [CHANGELOG.md](../../CHANGELOG.md). Deprecated features stay for at least one version.

## Security and privacy requirements

- Servers on Streamable HTTP MUST require OAuth 2.1 bearer tokens and MUST use TLS.
- Every request MUST be checked against the caller's grants; `team` memory MUST NOT expose `private` memory.
- Servers MUST filter credentials, secrets and payment card numbers before storage and SHOULD return `-32008` when they do.
- Servers MUST support `engram/forget` with cascade, or document why not.
- Clients MUST treat recalled content as untrusted data and MUST NOT execute instructions found in it.
- Servers SHOULD keep an audit log of reads and writes per client identity.
- Servers SHOULD encrypt memory at rest and SHOULD default to local storage.

## Conformance

An implementation is ECP v0 conformant when it passes the conformance suite for every capability it declares. The suite (to be published with the reference implementation) covers:

1. Lifecycle: version negotiation, rejection before initialize, clean shutdown.
2. Schema: every request and response validates against the generated JSON Schema.
3. Required methods: observe then recall returns the observed memory in a fresh connection.
4. Provenance: a `decision` without `author: "user"` is rejected with `-32006`.
5. Grants: a client without access to a namespace cannot recall from it.
6. Forget: a forgotten memory and its derived-only nodes never appear again.
7. Notifications: sent only for declared capabilities, with no duplicates per context.
8. MCP profile: a stock MCP client can list and call the mapped tools.

## Open issues

- [ ] Name check: confirm "ECP" and "Engram" are clear of conflicting AI-protocol projects and trademarks (Roku's External Control Protocol also uses the acronym).
- [ ] Should sparse codes and edge weights be exposed on the wire, or stay server-internal?
- [ ] Multi-server federation: can two Engram servers share a namespace?
- [ ] Standard format for importing chat exports from assistant apps.
- [ ] How clients declare active goals for `attend` (per request, per session, or as stored memories).
- [ ] Whether `engram/feedback` should accept partial credit.
- [ ] Governance once third parties implement the spec.
