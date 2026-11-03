// Integration tests against a live Engram server. Run through sdk/test.sh, which
// starts `engram serve --http` with a temporary database and sets these variables.
import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { ECPClient, ECPError, Codes, type Notification } from "../src/index.js";

const url = process.env.ECP_URL!;
const token = process.env.ECP_TOKEN!;
const bin = process.env.ENGRAM_BIN!;

test("observe, recall, get and errors over HTTP", async () => {
  const c = await ECPClient.http(url, token, { name: "sdk-test" });
  assert.equal(c.init.serverInfo.name, "engram");
  assert.ok(c.init.capabilities.recall);
  const { id } = await c.observe({ kind: "fact", content: "TypeScript SDK: deploys use blue green rollouts" });
  const hits = await c.recall({ query: "how do deploys roll out" });
  assert.ok(hits.some((h) => h.memory.id === id));
  const [m] = await c.get([id]);
  assert.equal(m.provenance.client, "sdk-test");
  await assert.rejects(c.observe({ kind: "fact", content: "x", namespace: "team/secret" }),
    (e: unknown) => e instanceof ECPError && e.code === Codes.ScopeDenied);
  await assert.rejects(c.call("engram/nope"), (e: unknown) => e instanceof ECPError && e.code === Codes.MethodNotFound);
  await c.close();
});

test("sessions and attend", async () => {
  const c = await ECPClient.http(url, token, { name: "sdk-test" });
  const cp = await c.checkpoint({ goal: "TypeScript SDK task", next: "write the docs" });
  const r = await c.resume(cp.sessionId, { tools: [] });
  assert.equal(r.checkpoint.next, "write the docs");
  const a = await c.attend({ context: "deploy rollouts" });
  assert.ok(Array.isArray(a.memories) && Array.isArray(a.conflicts));
  await c.close();
});

test("a failed job is pushed as notifications/engram/relevant", async () => {
  const c = await ECPClient.http(url, token, { name: "sdk-test", notifications: { relevant: true } });
  const got = new Promise<Notification>((resolve) => c.onNotification((n) => n.method === "notifications/engram/relevant" && resolve(n)));
  await c.listen();
  await c.subscribe({ context: "payouts job stripe", threshold: 0.2 });
  execFileSync(bin, ["job", "report", "payouts", "--status", "failed", "--summary", "Stripe timeout in batch 7"], { env: process.env });
  const n = await Promise.race([got, new Promise<never>((_, rej) => setTimeout(() => rej(new Error("no notification in 10s")), 10000))]);
  assert.match(JSON.stringify(n.params), /Stripe timeout/);
  await c.close();
});

test("stdio", async () => {
  const c = await ECPClient.stdio(bin, ["serve", "--client", "sdk-test"], { name: "sdk-test" });
  const { id } = await c.observe({ kind: "fact", content: "TypeScript SDK over stdio" });
  assert.match(id, /^mem_/);
  await c.close();
});
