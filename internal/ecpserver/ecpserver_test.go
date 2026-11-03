package ecpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecp"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

type rpcResp struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func call(t *testing.T, s *Server, sess *Session, id int, method string, params any) rpcResp {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	out := s.Handle(context.Background(), sess, b)
	var r rpcResp
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("%s: bad response %s", method, out)
	}
	return r
}

func initParams(name string) map[string]any {
	return map[string]any{"protocolVersion": ecp.ProtocolVersion, "clientInfo": map[string]any{"name": name, "version": "1"},
		"capabilities": map[string]any{"notifications": map[string]any{"relevant": true, "conflict": true, "consolidated": true}}}
}

func TestLifecycleAndErrors(t *testing.T) {
	st := newStore(t)
	s := New(st, Options{Version: "test"})
	sess := s.NewSession("", false)

	if r := call(t, s, sess, 1, "engram/recall", map[string]any{"query": "x"}); r.Error == nil || r.Error.Code != int(ecp.NotInitialized) {
		t.Fatalf("before initialize: want -32001, got %+v", r)
	}
	if r := call(t, s, sess, 2, "initialize", map[string]any{"protocolVersion": "1999-01-01", "clientInfo": map[string]any{"name": "x"}}); r.Error == nil || r.Error.Code != int(ecp.UnsupportedVersion) {
		t.Fatalf("unknown version: want -32002, got %+v", r)
	}
	r := call(t, s, sess, 3, "initialize", map[string]any{"protocolVersion": "2030-01-01", "supportedVersions": []string{"2030-01-01", ecp.ProtocolVersion},
		"clientInfo": map[string]any{"name": "cli", "version": "1"}, "capabilities": map[string]any{}})
	var init struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      map[string]any `json:"serverInfo"`
	}
	_ = json.Unmarshal(r.Result, &init)
	if r.Error != nil || init.ProtocolVersion != ecp.ProtocolVersion || init.Capabilities["recall"] == nil || init.ServerInfo["name"] != "engram" {
		t.Fatalf("negotiation should pick the shared version: %+v %s", r.Error, r.Result)
	}
	if r := call(t, s, sess, 4, "initialize", initParams("cli")); r.Error == nil {
		t.Fatal("initialize twice must fail")
	}
	if r := call(t, s, sess, 5, "engram/recall", map[string]any{"query": "x"}); r.Error == nil || r.Error.Code != int(ecp.Unauthorized) {
		t.Fatalf("unpaired client: want -32003, got %+v", r)
	}
	if r := call(t, s, sess, 6, "engram/nope", nil); r.Error == nil || r.Error.Code != int(ecp.MethodNotFound) {
		t.Fatalf("unknown method: want -32601, got %+v", r)
	}
	if r := call(t, s, sess, 7, "engram/recall", map[string]any{"query": 5}); r.Error == nil || r.Error.Code != int(ecp.InvalidParams) {
		t.Fatalf("bad params: want -32602, got %+v", r)
	}
	if out := s.Handle(context.Background(), sess, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); out != nil {
		t.Fatalf("notifications get no response, got %s", out)
	}
	if out := s.Handle(context.Background(), sess, []byte(`{nope`)); !strings.Contains(string(out), "-32700") {
		t.Fatalf("parse error: %s", out)
	}
	batch := `[{"jsonrpc":"2.0","id":10,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":11,"method":"ping"}]`
	var resps []rpcResp
	if err := json.Unmarshal(s.Handle(context.Background(), sess, []byte(batch)), &resps); err != nil || len(resps) != 2 {
		t.Fatalf("a batch returns one response per request: %v %d", err, len(resps))
	}
}

func TestMethods(t *testing.T) {
	st := newStore(t)
	if err := st.Pair("cli", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	s := New(st, Options{})
	sess := s.NewSession("", false)
	call(t, s, sess, 1, "initialize", initParams("cli"))

	obs := func(content string) string {
		r := call(t, s, sess, 2, "engram/observe", map[string]any{"kind": "fact", "content": content})
		var o struct{ ID string }
		if r.Error != nil || json.Unmarshal(r.Result, &o) != nil || o.ID == "" {
			t.Fatalf("observe: %+v %s", r.Error, r.Result)
		}
		return o.ID
	}
	a := obs("Branches start with the Jira ticket number")
	b := obs("Release branches are cut on Mondays")
	c := obs("Release branches are cut on Thursdays")
	if err := st.AddEdge(a, b, "coactive", 0.7); err != nil {
		t.Fatal(err)
	}
	r := call(t, s, sess, 3, "engram/observe", map[string]any{"kind": "decision", "content": "x", "author": "agent",
		"decision": map[string]any{"situation": "s", "choice": "c"}})
	if r.Error == nil || r.Error.Code != int(ecp.InvalidProvenance) {
		t.Fatalf("a decision not by the user: want -32006, got %+v", r)
	}
	r = call(t, s, sess, 4, "engram/recall", map[string]any{"query": "branch naming jira", "includePaths": true})
	if r.Error != nil || !strings.Contains(string(r.Result), a) {
		t.Fatalf("recall: %+v %s", r.Error, r.Result)
	}
	if r = call(t, s, sess, 5, "engram/get", map[string]any{"ids": []string{a}}); r.Error != nil || !strings.Contains(string(r.Result), "Jira") {
		t.Fatalf("get: %+v", r)
	}
	r = call(t, s, sess, 6, "engram/path", map[string]any{"from": a, "to": b})
	if r.Error != nil || !strings.Contains(string(r.Result), `"weakestLink"`) {
		t.Fatalf("path: %+v %s", r.Error, r.Result)
	}
	call(t, s, sess, 7, "engram/observe", map[string]any{"kind": "fact", "content": "Release branches are cut on Fridays", "contradicts": []string{b, c}})
	r = call(t, s, sess, 8, "engram/attend", map[string]any{"context": "cutting a release branch", "mode": "focus"})
	if r.Error != nil || !strings.Contains(string(r.Result), `"conflicts":[{`) {
		t.Fatalf("attend should report conflicts: %+v %s", r.Error, r.Result)
	}
	if r = call(t, s, sess, 9, "engram/feedback", map[string]any{"memoryIds": []string{a}, "outcome": "accepted", "objective": "succeeded"}); r.Error != nil {
		t.Fatalf("feedback: %+v", r)
	}
	r = call(t, s, sess, 10, "engram/session/checkpoint", map[string]any{"goal": "Ship the release", "next": "cut the branch"})
	var cp struct{ SessionID string }
	_ = json.Unmarshal(r.Result, &cp)
	if r.Error != nil || cp.SessionID == "" {
		t.Fatalf("checkpoint: %+v %s", r.Error, r.Result)
	}
	if r = call(t, s, sess, 11, "engram/session/list", map[string]any{"status": "open"}); !strings.Contains(string(r.Result), cp.SessionID) {
		t.Fatalf("list: %s", r.Result)
	}
	if r = call(t, s, sess, 12, "engram/session/resume", map[string]any{"sessionId": cp.SessionID}); !strings.Contains(string(r.Result), "cut the branch") {
		t.Fatalf("resume: %s", r.Result)
	}
	if r = call(t, s, sess, 13, "engram/session/delegate", map[string]any{"sessionId": cp.SessionID, "subtask": "Write notes", "toAgent": "codex"}); r.Error != nil {
		t.Fatalf("delegate: %+v", r)
	}
	if r = call(t, s, sess, 14, "engram/consolidate", map[string]any{"light": true}); r.Error == nil || r.Error.Code != int(ecp.ScopeDenied) {
		t.Fatalf("consolidate without a '*' write grant must be denied: %+v", r)
	}
	if r = call(t, s, sess, 15, "engram/forget", map[string]any{"ids": []string{a}}); r.Error != nil || !strings.Contains(string(r.Result), `"removed":1`) {
		t.Fatalf("forget: %+v %s", r.Error, r.Result)
	}
}

func TestServiceTokenWritesAsSystem(t *testing.T) {
	st := newStore(t)
	if err := st.Pair("ci", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	s := New(st, Options{})
	sess := s.NewSession("ci", true)
	call(t, s, sess, 1, "initialize", initParams("ignored-name"))
	r := call(t, s, sess, 2, "engram/observe", map[string]any{"kind": "episode", "content": "Nightly build passed"})
	var o struct{ ID string }
	_ = json.Unmarshal(r.Result, &o)
	mems, err := st.Get("ci", []string{o.ID})
	if err != nil || mems[0].Provenance.Author != "system" || mems[0].Provenance.Client != "ci" {
		t.Fatalf("a service writes as system under its token's identity: %+v %v", mems, err)
	}
}

// sse reads server-sent events into a channel.
func sse(t *testing.T, body io.Reader) <-chan map[string]any {
	ch := make(chan map[string]any, 16)
	go func() {
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var m map[string]any
				if json.Unmarshal([]byte(data), &m) == nil {
					ch <- m
				}
			}
		}
		close(ch)
	}()
	return ch
}

func waitFor(t *testing.T, ch <-chan map[string]any, method string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before %s", method)
			}
			if m["method"] == method {
				return m
			}
		case <-deadline:
			t.Fatalf("no %s within 5s", method)
		}
	}
}

func TestHTTPTransportAndNotifications(t *testing.T) {
	st := newStore(t)
	if err := st.Pair("claude-code", []string{"*"}, []string{"*"}, ""); err != nil {
		t.Fatal(err)
	}
	tok, _ := st.CreateToken("claude-code", false)
	s := New(st, Options{PollEvery: 20 * time.Millisecond, RateWindow: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	ts := httptest.NewServer(s.HTTPHandler())
	defer ts.Close()

	post := func(sid string, body any, auth string) (*http.Response, rpcResp) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/ecp", bytes.NewReader(b))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		if sid != "" {
			req.Header.Set(SessionHeader, sid)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var r rpcResp
		_ = json.NewDecoder(res.Body).Decode(&r)
		return res, r
	}
	msg := func(id int, method string, params any) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	}
	if res, _ := post("", msg(1, "initialize", initParams("x")), ""); res.StatusCode != 401 {
		t.Fatalf("no token: want 401, got %d", res.StatusCode)
	}
	if res, _ := post("", msg(1, "ping", nil), tok); res.StatusCode != 400 {
		t.Fatalf("first request must be initialize: got %d", res.StatusCode)
	}
	res, r := post("", msg(1, "initialize", initParams("pretends-to-be-someone-else")), tok)
	sid := res.Header.Get(SessionHeader)
	if r.Error != nil || sid == "" {
		t.Fatalf("initialize over HTTP: %+v %q", r.Error, sid)
	}
	if res, _ := post("bogus", msg(2, "ping", nil), tok); res.StatusCode != 404 {
		t.Fatalf("unknown session: want 404, got %d", res.StatusCode)
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/ecp", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(SessionHeader, sid)
	stream, err := http.DefaultClient.Do(req)
	if err != nil || stream.StatusCode != 200 {
		t.Fatalf("event stream: %v %v", err, stream)
	}
	defer stream.Body.Close()
	events := sse(t, stream.Body)

	_, r = post(sid, msg(3, "engram/observe", map[string]any{"kind": "fact", "namespace": "team/ink",
		"content": "Payout retries use exponential backoff"}), tok)
	var obs struct{ ID string }
	_ = json.Unmarshal(r.Result, &obs)
	mems, _ := st.Get("claude-code", []string{obs.ID})
	if mems[0].Provenance.Client != "claude-code" {
		t.Fatal("the token decides identity, never clientInfo")
	}
	if _, r = post(sid, msg(4, "engram/subscribe", map[string]any{"context": "payouts job failures", "threshold": 0.1}), tok); r.Error != nil {
		t.Fatalf("subscribe: %+v", r.Error)
	}

	// An event recorded elsewhere (a job) reaches the subscriber as notifications/engram/relevant.
	if _, err := st.AddTrigger("payouts", store.TriggerJob, "team/ink"); err != nil {
		t.Fatal(err)
	}
	ev, _, err := st.InsertSystem(store.SystemMemory{Key: "e1", Kind: "episode", Namespace: "team/ink", Service: "trigger:payouts",
		Content: "Job payouts failed: Stripe timeout"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := st.Dispatch(ev, 0.25); err != nil || len(n) != 1 {
		t.Fatalf("dispatch: %v %v", n, err)
	}
	rel := waitFor(t, events, "notifications/engram/relevant")
	if !strings.Contains(mustString(rel), "Stripe timeout") {
		t.Fatalf("relevant notification should carry the event: %v", rel)
	}

	// A conflict recorded by another client is pushed as notifications/engram/conflict.
	other, _ := st.Observe("claude-code", store.ObserveInput{Kind: "fact", Namespace: "team/ink", Content: "Payout retries are linear"})
	if err := st.MarkConflict("claude-code", other.ID, obs.ID); err != nil {
		t.Fatal(err)
	}
	conf := waitFor(t, events, "notifications/engram/conflict")
	if !strings.Contains(mustString(conf), "linear") {
		t.Fatalf("conflict notification: %v", conf)
	}

	// A sleep cycle run anywhere is announced as notifications/engram/consolidated.
	_, r = post(sid, msg(5, "engram/consolidate", map[string]any{"light": true}), tok)
	if r.Error != nil {
		t.Fatalf("consolidate: %+v", r.Error)
	}
	waitFor(t, events, "notifications/engram/consolidated")

	// The MCP compatibility profile is served over HTTP on the same port and token.
	hc := &http.Client{Transport: bearer{tok, http.DefaultTransport}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	cr, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "recall", Arguments: map[string]any{"query": "payout retries"}})
	if err != nil || cr.IsError {
		t.Fatalf("MCP over HTTP: %v %+v", err, cr)
	}
}

type bearer struct {
	tok string
	rt  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return b.rt.RoundTrip(r)
}

func mustString(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestStdio(t *testing.T) {
	st := newStore(t)
	if err := st.Pair("cli", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	s := New(st, Options{})
	init, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": initParams("cli")})
	if !IsECPInitialize(init) {
		t.Fatal("an ECP initialize should be recognized")
	}
	if IsECPInitialize([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`)) {
		t.Fatal("an MCP initialize must go to the MCP server")
	}
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"engram/observe","params":{"kind":"fact","content":"Tabs over spaces"}}` + "\n")
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), init, in, &out, ""); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"protocolVersion"`) || !strings.Contains(lines[1], `"id":"mem_`) {
		t.Fatalf("unexpected stdio output:\n%s", out.String())
	}
}
