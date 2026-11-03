package hosts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeJSONServerKeepsOtherServers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"mcpServers":{"github":{"command":"gh-mcp"}},"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := MergeJSONServer(p, "/usr/local/bin/engram", []string{"serve", "--client", "cursor"})
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	var doc map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	servers := doc["mcpServers"].(map[string]any)
	if servers["github"] == nil || servers["engram"] == nil || doc["theme"] != "dark" {
		t.Fatalf("other settings must survive: %s", b)
	}
	if _, err := os.Stat(p + ".engram-backup"); err != nil {
		t.Fatal("a backup should be written before editing")
	}
	if changed, _ := MergeJSONServer(p, "/x", nil); changed {
		t.Fatal("second run must change nothing")
	}
	if err := os.WriteFile(p, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeJSONServer(p, "/x", nil); err == nil {
		t.Fatal("invalid JSON must not be overwritten")
	}
}

func TestAppendTOMLServer(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("model = \"o4\""), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := AppendTOMLServer(p, "/opt/engram", []string{"serve", "--client", "codex"})
	if err != nil || !changed {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	want := "model = \"o4\"\n\n[mcp_servers.engram]\ncommand = \"/opt/engram\"\nargs = [\"serve\", \"--client\", \"codex\"]\n"
	if string(b) != want {
		t.Fatalf("got:\n%s", b)
	}
	if changed, _ := AppendTOMLServer(p, "/opt/engram", nil); changed {
		t.Fatal("second run must change nothing")
	}
}

func TestAddClaudeHooksIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"hooks":{"PostToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"prettier"}]}]},"model":"opus"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := AddClaudeHooks(p, "/bin/engram"); err != nil || !changed {
		t.Fatal(err)
	}
	if changed, _ := AddClaudeHooks(p, "/bin/engram"); changed {
		t.Fatal("second run must change nothing")
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if !strings.Contains(s, "prettier") || !strings.Contains(s, `"model": "opus"`) || strings.Count(s, "hook claude-code") != 3 {
		t.Fatalf("unexpected settings:\n%s", s)
	}
}
