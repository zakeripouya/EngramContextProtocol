"""Python client for the Engram Context Protocol (ECP).

Standard library only. Connect over HTTP (an ECP server's /ecp endpoint and a
bearer token) or stdio (a server process such as ``engram serve``)::

    from engram_ecp import Client

    with Client.http("http://127.0.0.1:7079/ecp", token, name="my-script") as c:
        mem = c.observe(kind="fact", content="Tabs over spaces")
        hits = c.recall(query="indentation")

Results are plain dicts shaped like spec/schema.ts. Errors raise ECPError with
the spec's code.
"""

from __future__ import annotations

import itertools
import json
import queue
import subprocess
import threading
import urllib.error
import urllib.request
from typing import Any, Callable, Dict, List, Optional

PROTOCOL_VERSION = "2026-11-01"

__all__ = ["Client", "ECPError", "Codes", "PROTOCOL_VERSION"]


class Codes:
    """Error codes from the spec."""

    PARSE_ERROR = -32700
    INVALID_REQUEST = -32600
    METHOD_NOT_FOUND = -32601
    INVALID_PARAMS = -32602
    NOT_INITIALIZED = -32001
    UNSUPPORTED_VERSION = -32002
    UNAUTHORIZED = -32003
    SCOPE_DENIED = -32004
    MEMORY_NOT_FOUND = -32005
    INVALID_PROVENANCE = -32006
    RATE_LIMITED = -32007
    CONTENT_REJECTED = -32008


class ECPError(Exception):
    """An ECP error response."""

    def __init__(self, code: int, message: str, data: Any = None):
        super().__init__(f"ecp error {code}: {message}")
        self.code = code
        self.message = message
        self.data = data


def _drop_none(d: Dict[str, Any]) -> Dict[str, Any]:
    return {k: v for k, v in d.items() if v is not None}


class _HTTP:
    def __init__(self, url: str, token: str, timeout: float):
        self.url, self.token, self.timeout = url, token, timeout
        self.session = ""
        self._stream: Optional[Any] = None

    def _headers(self) -> Dict[str, str]:
        h = {"Authorization": f"Bearer {self.token}", "Content-Type": "application/json"}
        if self.session:
            h["Ecp-Session-Id"] = self.session
        return h

    def send(self, payload: bytes, expect_reply: bool) -> Optional[dict]:
        req = urllib.request.Request(self.url, data=payload, headers=self._headers(), method="POST")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as res:
                sid = res.headers.get("Ecp-Session-Id")
                if sid:
                    self.session = sid
                body = res.read()
                if res.status == 202 or not expect_reply:
                    return None
                return json.loads(body)
        except urllib.error.HTTPError as e:
            body = e.read().decode("utf-8", "replace")
            if e.code == 401:
                msg = "unauthorized"
                try:
                    msg = json.loads(body)["error"]["message"]
                except Exception:
                    pass
                raise ECPError(Codes.UNAUTHORIZED, msg) from None
            raise RuntimeError(f"http {e.code}: {body.strip()}") from None

    def listen(self, on_message: Callable[[dict], None]) -> None:
        req = urllib.request.Request(self.url, headers={**self._headers(), "Accept": "text/event-stream"}, method="GET")
        res = urllib.request.urlopen(req, timeout=None)  # held open
        self._stream = res

        def loop() -> None:
            try:
                for raw in res:
                    line = raw.decode("utf-8").rstrip("\n")
                    if line.startswith("data: "):
                        try:
                            on_message(json.loads(line[6:]))
                        except ValueError:
                            pass
            except Exception:
                pass  # closed

        threading.Thread(target=loop, daemon=True).start()

    def close(self) -> None:
        # End the session first: the server then closes the event stream, so the
        # reader thread sees end-of-stream instead of blocking until a keep-alive.
        if self.session:
            req = urllib.request.Request(self.url, headers=self._headers(), method="DELETE")
            try:
                urllib.request.urlopen(req, timeout=self.timeout).close()
            except Exception:
                pass
        if self._stream is not None:
            try:
                self._stream.close()
            except Exception:
                pass


class _Stdio:
    def __init__(self, args: List[str], env: Optional[Dict[str, str]], on_notification: Callable[[dict], None]):
        self.proc = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=env)
        self.waiting: Dict[str, "queue.Queue[dict]"] = {}
        self.lock = threading.Lock()
        self.on_notification = on_notification
        threading.Thread(target=self._read, daemon=True).start()

    def _read(self) -> None:
        assert self.proc.stdout is not None
        for raw in self.proc.stdout:
            try:
                m = json.loads(raw)
            except ValueError:
                continue
            if "id" not in m and m.get("method"):
                self.on_notification(m)
                continue
            with self.lock:
                q = self.waiting.pop(json.dumps(m.get("id")), None)
            if q is not None:
                q.put(m)
        with self.lock:
            for q in self.waiting.values():
                q.put({"error": {"code": Codes.INVALID_REQUEST, "message": "server exited"}})
            self.waiting.clear()

    def send(self, payload: bytes, expect_reply: bool) -> Optional[dict]:
        q: "queue.Queue[dict]" = queue.Queue(maxsize=1)
        if expect_reply:
            with self.lock:
                self.waiting[json.dumps(json.loads(payload)["id"])] = q
        assert self.proc.stdin is not None
        self.proc.stdin.write(payload + b"\n")
        self.proc.stdin.flush()
        return q.get(timeout=120) if expect_reply else None

    def listen(self, on_message: Callable[[dict], None]) -> None:
        pass  # stdio delivers notifications as they arrive

    def close(self) -> None:
        if self.proc.stdin:
            self.proc.stdin.close()
        try:
            self.proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.proc.kill()


class Client:
    """A connection to an ECP server."""

    def __init__(self, transport: Any):
        self._t = transport
        self._ids = itertools.count(1)
        self._handlers: List[Callable[[dict], None]] = []
        self.init: Dict[str, Any] = {}

    # -- connecting --------------------------------------------------------

    @classmethod
    def http(cls, url: str, token: str, name: str, version: str = "0", notifications: Optional[Dict[str, bool]] = None,
             timeout: float = 120) -> "Client":
        """Connect to an ECP HTTP endpoint, such as http://127.0.0.1:7079/ecp."""
        c = cls(_HTTP(url, token, timeout))
        c._initialize(name, version, notifications)
        return c

    @classmethod
    def stdio(cls, args: List[str], name: str, version: str = "0", notifications: Optional[Dict[str, bool]] = None,
              env: Optional[Dict[str, str]] = None) -> "Client":
        """Start a server process (such as ["engram", "serve", "--client", name]) and speak ECP over its stdio."""
        c = cls(None)
        c._t = _Stdio(args, env, c._emit)
        c._initialize(name, version, notifications)
        return c

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    def close(self) -> None:
        self._t.close()

    # -- protocol ----------------------------------------------------------

    def call(self, method: str, params: Optional[dict] = None) -> Any:
        """Send a request and return its result, or raise ECPError."""
        msg: Dict[str, Any] = {"jsonrpc": "2.0", "id": next(self._ids), "method": method}
        if params is not None:
            msg["params"] = params
        resp = self._t.send(json.dumps(msg).encode(), True)
        if resp.get("error"):
            e = resp["error"]
            raise ECPError(e["code"], e["message"], e.get("data"))
        return resp.get("result")

    def notify(self, method: str, params: Optional[dict] = None) -> None:
        msg: Dict[str, Any] = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            msg["params"] = params
        self._t.send(json.dumps(msg).encode(), False)

    def on_notification(self, handler: Callable[[dict], None]) -> None:
        """Register a handler for server notifications ({"method", "params"})."""
        self._handlers.append(handler)

    def listen(self) -> None:
        """Open the HTTP notification stream (no-op over stdio)."""
        self._t.listen(self._emit)

    def _emit(self, m: dict) -> None:
        for h in self._handlers:
            h({"method": m.get("method"), "params": m.get("params")})

    def _initialize(self, name: str, version: str, notifications: Optional[Dict[str, bool]]) -> None:
        self.init = self.call("initialize", {
            "protocolVersion": PROTOCOL_VERSION,
            "supportedVersions": [PROTOCOL_VERSION],
            "clientInfo": {"name": name, "version": version},
            "capabilities": {"notifications": notifications or {}},
        })
        self.notify("notifications/initialized")

    # -- methods -----------------------------------------------------------

    def observe(self, kind: str, content: str, **fields: Any) -> dict:
        """Write a memory. Fields: decision, tags, scope, namespace, service, sessionId, author, validFrom, supersedes, contradicts."""
        return self.call("engram/observe", _drop_none({"kind": kind, "content": content, **fields}))

    def recall(self, query: str, **fields: Any) -> List[dict]:
        """Retrieve related memories. Fields: k, kinds, tags, namespaces, spread, asOf, includePaths."""
        return self.call("engram/recall", _drop_none({"query": query, **fields}))["memories"]

    def get(self, *ids: str) -> List[dict]:
        return self.call("engram/get", {"ids": list(ids)})["memories"]

    def path(self, from_id: str, to_id: str, rule: str = "maximin") -> dict:
        return self.call("engram/path", {"from": from_id, "to": to_id, "rule": rule})

    def attend(self, context: str, **fields: Any) -> dict:
        """What matters now. Fields: goals, k, mode, namespaces."""
        return self.call("engram/attend", _drop_none({"context": context, **fields}))

    def feedback(self, memory_ids: List[str], outcome: str, **fields: Any) -> dict:
        """Report how recalled memories worked out. Fields: objective, correction, query."""
        return self.call("engram/feedback", _drop_none({"memoryIds": memory_ids, "outcome": outcome, **fields}))

    def forget(self, *ids: str, cascade: bool = True) -> int:
        return self.call("engram/forget", {"ids": list(ids), "cascade": cascade})["removed"]

    def subscribe(self, context: str, **fields: Any) -> str:
        """Register interest. Fields: tags, threshold, namespaces."""
        return self.call("engram/subscribe", _drop_none({"context": context, **fields}))["subscriptionId"]

    def unsubscribe(self, subscription_id: str) -> None:
        self.call("engram/unsubscribe", {"subscriptionId": subscription_id})

    def consolidate(self, light: bool = False) -> dict:
        return self.call("engram/consolidate", {"light": light})

    def checkpoint(self, **checkpoint: Any) -> dict:
        """Save a task: goal, plan, done, next, openQuestions, files, environment, namespace, status, result, sessionId."""
        return self.call("engram/session/checkpoint", _drop_none(checkpoint))

    def resume(self, session_id: str, environment: Optional[dict] = None) -> dict:
        return self.call("engram/session/resume", _drop_none({"sessionId": session_id, "environment": environment}))

    def sessions(self, status: Optional[str] = None) -> List[dict]:
        return self.call("engram/session/list", _drop_none({"status": status}))["sessions"]

    def delegate(self, session_id: str, subtask: str, to_agent: Optional[str] = None) -> dict:
        return self.call("engram/session/delegate", _drop_none({"sessionId": session_id, "subtask": subtask, "toAgent": to_agent}))
