package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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
	for _, want := range []string{"observe", "recall", "get", "forget"} {
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
