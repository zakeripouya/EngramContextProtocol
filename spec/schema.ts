/**
 * Engram Context Protocol (ECP) — normative schema, protocol version 2026-11-01 (draft v0).
 *
 * This file is the source of truth for the wire format. The prose specification is
 * docs/spec/ecp-v0.md. A JSON Schema is generated from this file (see spec/README.md).
 */

export const PROTOCOL_VERSION = "2026-11-01";

/* ------------------------------------------------------------------ */
/* JSON-RPC 2.0 envelope                                              */
/* ------------------------------------------------------------------ */

export type RequestId = string | number;

export interface JSONRPCRequest<M extends string = string, P = unknown> {
  jsonrpc: "2.0";
  id: RequestId;
  method: M;
  params?: P;
}

export interface JSONRPCNotification<M extends string = string, P = unknown> {
  jsonrpc: "2.0";
  method: M;
  params?: P;
}

export interface JSONRPCResult<R = unknown> {
  jsonrpc: "2.0";
  id: RequestId;
  result: R;
}

export interface JSONRPCError {
  jsonrpc: "2.0";
  id: RequestId | null;
  error: { code: ErrorCode | number; message: string; data?: unknown };
}

/* ------------------------------------------------------------------ */
/* Core data types                                                    */
/* ------------------------------------------------------------------ */

export type NodeKind =
  | "episode"
  | "concept"
  | "decision"
  | "suggestion"
  | "fact"
  | "trait"
  | "procedure";

export type EdgeKind =
  | "similar"
  | "coactive"
  | "about"
  | "derived"
  | "supersedes"
  | "conflicts"
  | "accepted"
  | "overrode"
  | "cross";

export type Scope = "private" | "team" | "public";

export type Author = "user" | "agent" | "system";

export interface Provenance {
  agent: string; // e.g. "claude", "codex"
  client: string; // clientInfo.name of the writer, set by the server
  service?: string; // e.g. "github", "gmail"
  sessionId?: string;
  author: Author;
  at: string; // ISO 8601, UTC
}

export interface Memory {
  id: string;
  kind: NodeKind;
  content: string;
  tags?: string[];
  scope: Scope;
  namespace: string; // e.g. "me", "team/ink", "pack/developer", "ext/stripe-api"
  strength: number; // 0..1
  confidence?: number; // 0..1, for traits and distilled facts
  provenance: Provenance;
  createdAt: string; // when Engram learned it (recorded at)
  validFrom?: string; // when it became true in the world
  validTo?: string; // when it stopped being true; absent = still true
  lastActivatedAt?: string;
}

export interface Decision {
  situation: string;
  options?: string[];
  choice: string;
  rationale?: string;
}

export interface Edge {
  from: string;
  to: string;
  kind: EdgeKind;
  weight: number; // 0..1
}

export interface ScoredMemory {
  memory: Memory;
  score: number;
  confidence?: number; // calibrated; low values mean "don't know"
  path?: string[]; // node ids from seed to this memory
  reason?: string; // human-readable explanation
}

/* ------------------------------------------------------------------ */
/* Lifecycle                                                          */
/* ------------------------------------------------------------------ */

export interface ClientInfo {
  name: string;
  version: string;
  agent?: string;
}

export interface ServerInfo {
  name: string;
  version: string;
}

export interface ClientCapabilities {
  notifications?: { relevant?: boolean; conflict?: boolean; consolidated?: boolean };
  [extension: `x_${string}`]: unknown;
}

export interface ServerCapabilities {
  observe?: { kinds?: NodeKind[] };
  recall?: { spreadingActivation?: boolean; maxK?: number };
  attend?: Record<string, never>;
  feedback?: Record<string, never>;
  subscribe?: { topics?: boolean };
  consolidate?: Record<string, never>;
  forget?: { cascade?: boolean };
  sessions?: Record<string, never>;
  [extension: `x_${string}`]: unknown;
}

export interface InitializeParams {
  protocolVersion: string;
  supportedVersions?: string[];
  capabilities: ClientCapabilities;
  clientInfo: ClientInfo;
}

export interface InitializeResult {
  protocolVersion: string;
  capabilities: ServerCapabilities;
  serverInfo: ServerInfo;
}

/* ------------------------------------------------------------------ */
/* Methods                                                            */
/* ------------------------------------------------------------------ */

export interface ObserveParams {
  kind: "episode" | "decision" | "suggestion" | "fact";
  content: string;
  decision?: Decision; // REQUIRED when kind = "decision"
  tags?: string[];
  scope?: Scope; // default "private"
  namespace?: string; // default "me"
  service?: string;
  sessionId?: string;
  author?: Author; // default "agent"; a decision requires "user"
  validFrom?: string;
  supersedes?: string; // id of an older memory this replaces; it stops being current
  contradicts?: string[]; // ids of current memories this disagrees with; kept as a conflict until resolved
}
export interface ObserveResult {
  id: string;
  linked: number; // edges created (supersedes and kNN similar edges)
  embedded?: boolean; // false when no embedding was available; the memory is still stored
}

export interface RecallParams {
  query: string;
  k?: number; // default 5
  kinds?: NodeKind[];
  tags?: string[];
  namespaces?: string[]; // default: personal and team, then linked packs
  spread?: number; // hops of spreading activation, default 2
  asOf?: string; // answer as of this time (bitemporal query)
  includePaths?: boolean;
}
export interface RecallResult {
  memories: ScoredMemory[];
}

export interface GetParams {
  ids: string[];
}
export interface GetResult {
  memories: Memory[];
}

export interface PathParams {
  from: string;
  to: string;
  rule?: "maximin" | "most_probable"; // default "maximin"
}
export interface PathResult {
  path: string[];
  weakestLink?: Edge;
}

export interface AttendParams {
  context: string;
  goals?: string[];
  k?: number;
  mode?: "detail" | "explore" | "focus" | "generalize";
}
export interface AttendResult {
  memories: ScoredMemory[];
  conflicts: { a: string; b: string; summary: string }[];
  pending?: PendingNotification[]; // unread push notifications for this client
}
/** A stored push, as returned by attend and by reading engram://attention over MCP. */
export interface PendingNotification {
  id: number;
  subscriptionId: string;
  eventId: string; // the episode that triggered it
  reason: string;
  salience: number;
  memories: Memory[]; // the event and related memories the subscriber may read
  createdAt: string;
}

export interface FeedbackParams {
  memoryIds: string[];
  outcome: "accepted" | "overridden";
  correction?: Decision; // stored as a new decision when overridden
  objective?: "succeeded" | "failed"; // real-world outcome, weighted above approval
}

export interface ForgetParams {
  ids: string[];
  cascade?: boolean; // also remove nodes derived only from these
}
export interface ForgetResult {
  removed: number;
}

export interface SubscribeParams {
  context: string;
  tags?: string[];
  threshold?: number; // minimum salience 0..1, default 0.6
  namespaces?: string[]; // only events in these namespaces; each must be readable
}
export interface SubscribeResult {
  subscriptionId: string;
}

export interface UnsubscribeParams {
  subscriptionId: string;
}

export interface ConsolidateParams {
  namespace?: string;
  light?: boolean; // decay and dedupe only
}

/* Sessions and handoff */

export interface Checkpoint {
  sessionId: string;
  goal: string;
  plan: string[];
  done: string[];
  next?: string;
  openQuestions?: string[];
  files?: string[];
  environment?: { repo?: string; tools?: string[] };
  namespace: string;
  status?: "open" | "done"; // default "open"; "done" records the outcome as an episode
  result?: string; // what was achieved, when done
  parentId?: string; // set on delegated subtasks
  delegatedTo?: string;
  createdBy?: string; // server-set
  lastClient?: string; // server-set
  createdAt?: string; // server-set
  updatedAt: string;
}
export interface ResumeParams {
  sessionId: string;
  environment?: { repo?: string; tools?: string[] }; // what the resuming client has
}
export interface ResumeResult {
  checkpoint: Checkpoint;
  missing: string[]; // "repo: …", "tool: …", "permission: …"
  subtasks?: { sessionId: string; goal: string; status: "open" | "done"; delegatedTo?: string; result?: string }[];
  memories?: Memory[]; // recent memories recorded under this session
}
export interface SessionListParams {
  status?: "open" | "done";
}
export interface SessionListResult {
  sessions: Checkpoint[];
}
export interface DelegateParams {
  sessionId: string;
  subtask: string;
  toAgent?: string;
}

export type ECPRequest =
  | JSONRPCRequest<"initialize", InitializeParams>
  | JSONRPCRequest<"engram/observe", ObserveParams>
  | JSONRPCRequest<"engram/recall", RecallParams>
  | JSONRPCRequest<"engram/get", GetParams>
  | JSONRPCRequest<"engram/path", PathParams>
  | JSONRPCRequest<"engram/attend", AttendParams>
  | JSONRPCRequest<"engram/feedback", FeedbackParams>
  | JSONRPCRequest<"engram/forget", ForgetParams>
  | JSONRPCRequest<"engram/subscribe", SubscribeParams>
  | JSONRPCRequest<"engram/unsubscribe", UnsubscribeParams>
  | JSONRPCRequest<"engram/consolidate", ConsolidateParams>
  | JSONRPCRequest<"engram/session/checkpoint", Checkpoint>
  | JSONRPCRequest<"engram/session/resume", ResumeParams>
  | JSONRPCRequest<"engram/session/list", SessionListParams>
  | JSONRPCRequest<"engram/session/delegate", DelegateParams>;

/* ------------------------------------------------------------------ */
/* Notifications                                                      */
/* ------------------------------------------------------------------ */

export interface InitializedNotification {
  jsonrpc: "2.0";
  method: "notifications/initialized";
}

export interface RelevantNotification {
  jsonrpc: "2.0";
  method: "notifications/engram/relevant";
  params: { subscriptionId: string; memories: ScoredMemory[]; reason: string };
}

export interface ConflictNotification {
  jsonrpc: "2.0";
  method: "notifications/engram/conflict";
  params: { a: Memory; b: Memory; summary: string };
}

export interface ConsolidatedNotification {
  jsonrpc: "2.0";
  method: "notifications/engram/consolidated";
  params: { created: number; pruned: number; traits: string[]; runId?: number; light?: boolean };
}

/* ------------------------------------------------------------------ */
/* Errors                                                             */
/* ------------------------------------------------------------------ */

export enum ErrorCode {
  ParseError = -32700,
  InvalidRequest = -32600,
  MethodNotFound = -32601,
  InvalidParams = -32602,
  InternalError = -32603,
  NotInitialized = -32001,
  UnsupportedVersion = -32002,
  Unauthorized = -32003,
  ScopeDenied = -32004,
  MemoryNotFound = -32005,
  InvalidProvenance = -32006,
  RateLimited = -32007,
  ContentRejected = -32008,
}
