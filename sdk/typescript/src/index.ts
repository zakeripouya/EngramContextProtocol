// TypeScript client for the Engram Context Protocol (ECP).
//
//   const c = await ECPClient.http("http://127.0.0.1:7079/ecp", token, { name: "my-tool" });
//   const { id } = await c.observe({ kind: "fact", content: "Tabs over spaces" });
//   const hits = await c.recall({ query: "indentation" });
//
// Types come from spec/schema.ts, the normative ECP schema (copied into src/schema.ts).

import { spawn, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";
import type {
  AttendParams, AttendResult, Checkpoint, FeedbackParams, ForgetResult, GetResult, InitializeResult, Memory,
  ObserveParams, ObserveResult, PathResult, RecallParams, ScoredMemory, ResumeResult, SubscribeParams,
} from "./schema.js";

export * from "./schema.js";

export const PROTOCOL_VERSION = "2026-11-01";

/** Error codes from the spec. */
export const Codes = {
  ParseError: -32700, InvalidRequest: -32600, MethodNotFound: -32601, InvalidParams: -32602,
  NotInitialized: -32001, UnsupportedVersion: -32002, Unauthorized: -32003, ScopeDenied: -32004,
  MemoryNotFound: -32005, InvalidProvenance: -32006, RateLimited: -32007, ContentRejected: -32008,
} as const;

/** An ECP error response. */
export class ECPError extends Error {
  constructor(public code: number, message: string, public data?: unknown) {
    super(`ecp error ${code}: ${message}`);
    this.name = "ECPError";
  }
}

export interface ClientInfo {
  name: string;
  version?: string;
  agent?: string;
  /** Notifications to receive. */
  notifications?: { relevant?: boolean; conflict?: boolean; consolidated?: boolean };
}

export interface Notification {
  method: string;
  params: any;
}

type Handler = (n: Notification) => void;

interface Transport {
  send(payload: string, expectReply: boolean): Promise<any>;
  close(): Promise<void>;
}

class HTTPTransport implements Transport {
  session = "";
  private stream?: AbortController;
  constructor(private url: string, private token: string) {}

  async send(payload: string, expectReply: boolean): Promise<any> {
    const headers: Record<string, string> = { "Content-Type": "application/json", Authorization: `Bearer ${this.token}` };
    if (this.session) headers["Ecp-Session-Id"] = this.session;
    const res = await fetch(this.url, { method: "POST", headers, body: payload });
    const sid = res.headers.get("Ecp-Session-Id");
    if (sid) this.session = sid;
    if (res.status === 202) return undefined;
    const text = await res.text();
    if (res.status === 401) {
      let msg = "unauthorized";
      try { msg = JSON.parse(text).error?.message ?? msg; } catch {}
      throw new ECPError(Codes.Unauthorized, msg);
    }
    if (!res.ok) throw new Error(`http ${res.status}: ${text.trim()}`);
    return expectReply ? JSON.parse(text) : undefined;
  }

  /** Opens the server-sent event stream; resolves once it is open. */
  async listen(onMessage: (m: any) => void): Promise<void> {
    this.stream = new AbortController();
    const res = await fetch(this.url, {
      headers: { Accept: "text/event-stream", Authorization: `Bearer ${this.token}`, "Ecp-Session-Id": this.session },
      signal: this.stream.signal,
    });
    if (!res.ok || !res.body) throw new Error(`event stream: http ${res.status}`);
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    (async () => {
      let buf = "";
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buf += decoder.decode(value, { stream: true });
          let i: number;
          while ((i = buf.indexOf("\n\n")) >= 0) {
            const event = buf.slice(0, i);
            buf = buf.slice(i + 2);
            for (const line of event.split("\n")) {
              if (line.startsWith("data: ")) {
                try { onMessage(JSON.parse(line.slice(6))); } catch {}
              }
            }
          }
        }
      } catch {
        // aborted or disconnected
      }
    })();
  }

  async close(): Promise<void> {
    if (this.session) {
      await fetch(this.url, { method: "DELETE", headers: { Authorization: `Bearer ${this.token}`, "Ecp-Session-Id": this.session } }).catch(() => {});
    }
    this.stream?.abort();
  }
}

class StdioTransport implements Transport {
  private waiting = new Map<string, (m: any) => void>();
  private closed: Promise<void>;
  constructor(private proc: ChildProcess, onNotification: (m: any) => void) {
    const rl = createInterface({ input: proc.stdout! });
    rl.on("line", (line) => {
      let m: any;
      try { m = JSON.parse(line); } catch { return; }
      if (m.id === undefined && m.method) return onNotification(m);
      const cb = this.waiting.get(JSON.stringify(m.id));
      if (cb) { this.waiting.delete(JSON.stringify(m.id)); cb(m); }
    });
    this.closed = new Promise((resolve) => proc.on("exit", () => {
      for (const cb of this.waiting.values()) cb({ error: { code: Codes.InvalidRequest, message: "server exited" } });
      this.waiting.clear();
      resolve();
    }));
  }
  send(payload: string, expectReply: boolean): Promise<any> {
    let reply: Promise<any> = Promise.resolve(undefined);
    if (expectReply) {
      const id = JSON.stringify(JSON.parse(payload).id);
      reply = new Promise((resolve) => this.waiting.set(id, resolve));
    }
    this.proc.stdin!.write(payload + "\n");
    return reply;
  }
  async close(): Promise<void> {
    this.proc.stdin!.end();
    await this.closed;
  }
}

/** A connection to an ECP server. */
export class ECPClient {
  private nextId = 0;
  private handlers: Handler[] = [];
  init!: InitializeResult;

  private constructor(private t: Transport) {}

  /** Connects to an ECP HTTP endpoint (such as http://127.0.0.1:7079/ecp) with a bearer token. */
  static async http(url: string, token: string, info: ClientInfo): Promise<ECPClient> {
    const c = new ECPClient(new HTTPTransport(url, token));
    await c.initialize(info);
    return c;
  }

  /** Starts a server process (such as `engram serve --client NAME`) and speaks ECP over its stdio. */
  static async stdio(command: string, args: string[], info: ClientInfo, env?: NodeJS.ProcessEnv): Promise<ECPClient> {
    const proc = spawn(command, args, { stdio: ["pipe", "pipe", "ignore"], env: env ?? process.env });
    let c!: ECPClient;
    const t = new StdioTransport(proc, (m) => c.emit(m));
    c = new ECPClient(t);
    await c.initialize(info);
    return c;
  }

  private emit(m: any) {
    for (const h of this.handlers) h({ method: m.method, params: m.params });
  }

  /** Registers a notification handler. Over HTTP, call listen() to open the stream. */
  onNotification(h: Handler): void {
    this.handlers.push(h);
  }

  /** Opens the HTTP notification stream (no-op over stdio). */
  async listen(): Promise<void> {
    if (this.t instanceof HTTPTransport) await this.t.listen((m) => this.emit(m));
  }

  /** Sends a request and returns its result, or throws ECPError. */
  async call<T = any>(method: string, params?: unknown): Promise<T> {
    const id = ++this.nextId;
    const resp = await this.t.send(JSON.stringify({ jsonrpc: "2.0", id, method, params }), true);
    if (resp.error) throw new ECPError(resp.error.code, resp.error.message, resp.error.data);
    return resp.result as T;
  }

  async notify(method: string, params?: unknown): Promise<void> {
    await this.t.send(JSON.stringify({ jsonrpc: "2.0", method, params }), false);
  }

  private async initialize(info: ClientInfo) {
    this.init = await this.call<InitializeResult>("initialize", {
      protocolVersion: PROTOCOL_VERSION,
      supportedVersions: [PROTOCOL_VERSION],
      clientInfo: { name: info.name, version: info.version ?? "0", agent: info.agent },
      capabilities: { notifications: info.notifications ?? {} },
    });
    await this.notify("notifications/initialized");
  }

  close(): Promise<void> {
    return this.t.close();
  }

  observe(p: ObserveParams): Promise<ObserveResult> { return this.call("engram/observe", p); }
  async recall(p: RecallParams): Promise<ScoredMemory[]> { return (await this.call("engram/recall", p)).memories; }
  async get(ids: string[]): Promise<Memory[]> { return (await this.call<GetResult>("engram/get", { ids })).memories; }
  path(from: string, to: string, rule: "maximin" | "most_probable" = "maximin"): Promise<PathResult> {
    return this.call("engram/path", { from, to, rule });
  }
  attend(p: AttendParams): Promise<AttendResult> { return this.call("engram/attend", p); }
  feedback(p: FeedbackParams): Promise<{ updated: number; direction: string; correctionId?: string }> {
    return this.call("engram/feedback", p);
  }
  async forget(ids: string[], cascade = true): Promise<number> {
    return (await this.call<ForgetResult>("engram/forget", { ids, cascade })).removed;
  }
  async subscribe(p: SubscribeParams): Promise<string> { return (await this.call("engram/subscribe", p)).subscriptionId; }
  async unsubscribe(subscriptionId: string): Promise<void> { await this.call("engram/unsubscribe", { subscriptionId }); }
  consolidate(light = false): Promise<any> { return this.call("engram/consolidate", { light }); }
  checkpoint(cp: Partial<Checkpoint>): Promise<Checkpoint> { return this.call("engram/session/checkpoint", cp); }
  resume(sessionId: string, environment?: { repo?: string; tools?: string[] }): Promise<ResumeResult> {
    return this.call("engram/session/resume", { sessionId, environment });
  }
  async sessions(status?: "open" | "done"): Promise<Checkpoint[]> { return (await this.call("engram/session/list", { status })).sessions; }
  delegate(sessionId: string, subtask: string, toAgent?: string): Promise<Checkpoint> {
    return this.call("engram/session/delegate", { sessionId, subtask, toAgent });
  }
}
