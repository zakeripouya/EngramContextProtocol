package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// connect runs the server in-process and returns a connected MCP client session.
func connect(t *testing.T, st *store.Store, client string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := New(st, client).Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return res, b.String()
}

func open(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestToolsAreListed(t *testing.T) {
	cs := connect(t, open(t, ":memory:"), "claude-code")
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"observe", "recall", "get", "forget", "feedback", "checkpoint", "sessions", "resume", "delegate", "attend", "subscribe", "unsubscribe"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

func TestUnpairedClientGetsPairingInstructions(t *testing.T) {
	cs := connect(t, open(t, ":memory:"), "claude-code")
	res, text := call(t, cs, "recall", map[string]any{"query": "branch naming"})
	if !res.IsError || !strings.Contains(text, "-32003") || !strings.Contains(text, "engram pair claude-code") {
		t.Fatalf("want an unauthorized error with pairing instructions, got %q", text)
	}
}

// The M1 done-check: a fact told in one session is recalled in a fresh one, here by a different agent.
func TestFactToldInOneSessionIsRecalledInAnother(t *testing.T) {
	db := filepath.Join(t.TempDir(), "engram.db")
	setup := open(t, db)
	if err := setup.Pair("claude-code", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := setup.Pair("codex", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}

	first := connect(t, open(t, db), "claude-code")
	if res, text := call(t, first, "observe", map[string]any{
		"kind": "fact", "content": "Branches start with the Jira ticket number",
	}); res.IsError {
		t.Fatalf("observe failed: %s", text)
	}

	second := connect(t, open(t, db), "codex")
	res, text := call(t, second, "recall", map[string]any{"query": "how do I name a branch"})
	if res.IsError || !strings.Contains(text, "Jira ticket number") {
		t.Fatalf("recall in a fresh session did not find the fact: %s", text)
	}
}

func TestAgentCannotRecordADecisionAsItsOwn(t *testing.T) {
	st := open(t, ":memory:")
	if err := st.Pair("claude-code", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, st, "claude-code")
	res, text := call(t, cs, "observe", map[string]any{
		"kind": "decision", "content": "Use a separate table",
		"decision": map[string]any{"situation": "artists", "choice": "separate table"},
	})
	if !res.IsError || !strings.Contains(text, "-32006") {
		t.Fatalf("want an invalid-provenance error, got %q", text)
	}
}

func TestClientInfoNameIsUsedWhenNoClientIsFixed(t *testing.T) {
	st := open(t, ":memory:")
	if err := st.Pair("test-client", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, st, "")
	if res, text := call(t, cs, "observe", map[string]any{"kind": "fact", "content": "hello from the test client"}); res.IsError {
		t.Fatalf("identity from clientInfo was not used: %s", text)
	}
}

// M7 done-when: a task started in Claude Code is finished in Codex without re-explaining.
func TestHandoffFromClaudeCodeToCodex(t *testing.T) {
	st := open(t, filepath.Join(t.TempDir(), "engram.db"))
	for _, c := range []string{"claude-code", "codex"} {
		if err := st.Pair(c, []string{"me"}, []string{"me"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	cc := connect(t, st, "claude-code")
	res, _ := call(t, cc, "checkpoint", map[string]any{
		"goal": "Migrate the billing service to Postgres", "plan": []string{"schema", "dual writes", "cutover"},
		"done": []string{"schema"}, "next": "enable dual writes behind the flag",
		"environment": map[string]any{"repo": "github.com/acme/billing", "tools": []string{"psql"}},
	})
	if res.IsError {
		t.Fatalf("checkpoint failed: %+v", res)
	}
	id := res.StructuredContent.(map[string]any)["sessionId"].(string)

	cx := connect(t, st, "codex")
	res, text := call(t, cx, "sessions", map[string]any{})
	if res.IsError || !strings.Contains(text, id) {
		t.Fatalf("codex should list the session: %s", text)
	}
	res, text = call(t, cx, "resume", map[string]any{"session_id": id, "environment": map[string]any{"repo": "acme/billing"}})
	if res.IsError || !strings.Contains(text, "enable dual writes") || !strings.Contains(text, "tool: psql") {
		t.Fatalf("resume should carry the next step and report psql missing: %s", text)
	}
	res, _ = call(t, cx, "checkpoint", map[string]any{"session_id": id, "status": "done", "result": "cut over on Friday"})
	if res.IsError {
		t.Fatalf("finishing failed: %+v", res)
	}
	_, text = call(t, cc, "recall", map[string]any{"query": "billing Postgres migration"})
	if !strings.Contains(text, "cut over on Friday") {
		t.Fatalf("claude-code should recall the outcome: %s", text)
	}
}

// M8: a failed job is pushed to a subscribed agent over MCP as an update to engram://attention.
func TestPushNotificationOverMCP(t *testing.T) {
	st := open(t, ":memory:")
	if err := st.Pair("claude-code", []string{"me", "flow/payouts"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Observe("claude-code", store.ObserveInput{Kind: "decision", Author: "user",
		Content:  "Payouts retry Stripe timeouts with backoff, at most 3 attempts",
		Decision: &store.Decision{Situation: "payouts Stripe timeouts", Choice: "backoff, 3 attempts"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverT, clientT := mcp.NewInMemoryTransports()
	srv := New(st, "claude-code")
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	updated := make(chan string, 4)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) { updated <- req.Params.URI },
	}).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	old := RateWindow
	RateWindow = 0
	defer func() { RateWindow = old }()
	watched := make(chan struct{})
	go func() { srv.Watch(ctx, 20*time.Millisecond); close(watched) }()

	if res, text := call(t, cs, "subscribe", map[string]any{"context": "payouts job Stripe"}); res.IsError {
		t.Fatal(text)
	}
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: AttentionURI}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTrigger("payouts", store.TriggerJob, ""); err != nil {
		t.Fatal(err)
	}
	ev, _, err := st.InsertSystem(store.SystemMemory{Key: "t1", Kind: "episode", Namespace: "flow/payouts", Service: "trigger:payouts",
		Content: "Job payouts failed: Stripe timeout in batch 7"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := st.Dispatch(ev, 0.25); err != nil || len(n) != 1 {
		t.Fatalf("dispatch: %v %v", n, err)
	}
	select {
	case uri := <-updated:
		if uri != AttentionURI {
			t.Fatalf("unexpected uri %q", uri)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no resource update was pushed")
	}
	rr, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: AttentionURI})
	if err != nil {
		t.Fatal(err)
	}
	if body := rr.Contents[0].Text; !strings.Contains(body, "Stripe timeout in batch 7") || !strings.Contains(body, "at most 3 attempts") {
		t.Fatalf("attention should carry the event and the decision: %s", body)
	}
	_, text := call(t, cs, "attend", map[string]any{"context": "payouts"})
	if strings.Contains(text, `"pending":[{`) {
		t.Fatalf("delivered notifications should not be pending again: %s", text)
	}
	cancel()
	<-watched
	if subs, _ := st.Subscriptions(); len(subs) != 0 {
		t.Fatalf("subscriptions end with the connection, got %d", len(subs))
	}
}
