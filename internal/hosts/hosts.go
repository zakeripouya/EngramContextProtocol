// Package hosts connects Engram to agent hosts (Claude Code, Claude Desktop,
// Codex, Cursor) by editing their MCP configuration, for "engram init"
// (milestone M11). Every edit is idempotent, keeps the rest of the file intact,
// and writes a backup before changing an existing file.
package hosts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Host is one agent application.
type Host struct {
	Name   string // the client name it is paired as, e.g. "claude-code"
	Title  string
	Found  bool   // the host appears to be installed
	Config string // the config file Engram edits, if any
	Note   string
}

// Detect reports which hosts are installed, given the user's home directory.
func Detect(home string) []Host {
	var out []Host
	_, err := exec.LookPath("claude")
	out = append(out, Host{Name: "claude-code", Title: "Claude Code", Found: err == nil, Note: "connected with: claude mcp add --scope user"})
	desktop := DesktopConfigPath(home)
	out = append(out, Host{Name: "claude-desktop", Title: "Claude Desktop", Found: exists(filepath.Dir(desktop)), Config: desktop})
	codex := filepath.Join(home, ".codex", "config.toml")
	_, cerr := exec.LookPath("codex")
	out = append(out, Host{Name: "codex", Title: "Codex", Found: cerr == nil || exists(filepath.Dir(codex)), Config: codex})
	cursor := filepath.Join(home, ".cursor", "mcp.json")
	out = append(out, Host{Name: "cursor", Title: "Cursor", Found: exists(filepath.Dir(cursor)), Config: cursor})
	return out
}

// DesktopConfigPath is Claude Desktop's config file on this OS.
func DesktopConfigPath(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			return filepath.Join(appdata, "Claude", "claude_desktop_config.json")
		}
		return filepath.Join(home, "AppData", "Roaming", "Claude", "claude_desktop_config.json")
	}
	return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Connect adds Engram to a host. exe is the engram binary. It returns what it did.
func Connect(h Host, exe string) (string, error) {
	args := []string{"serve", "--client", h.Name}
	switch h.Name {
	case "claude-code":
		if !h.Found {
			return "", fmt.Errorf("the claude CLI is not on PATH")
		}
		cmd := exec.Command("claude", append([]string{"mcp", "add", "--scope", "user", "engram", "--", exe}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			if strings.Contains(string(out), "already exists") {
				return "Claude Code already has an engram server", nil
			}
			return "", fmt.Errorf("claude mcp add: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return "added engram to Claude Code (user scope)", nil
	case "claude-desktop", "cursor":
		changed, err := MergeJSONServer(h.Config, exe, args)
		if err != nil {
			return "", err
		}
		if !changed {
			return h.Title + " already has an engram server", nil
		}
		return "added engram to " + h.Config, nil
	case "codex":
		changed, err := AppendTOMLServer(h.Config, exe, args)
		if err != nil {
			return "", err
		}
		if !changed {
			return "Codex already has an engram server", nil
		}
		return "added engram to " + h.Config, nil
	}
	return "", fmt.Errorf("unknown host %q", h.Name)
}

func backup(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(path+".engram-backup", b, 0o600)
}

// MergeJSONServer adds mcpServers.engram to a JSON config (Claude Desktop,
// Cursor), creating the file if needed. Other keys are preserved.
func MergeJSONServer(path, exe string, args []string) (bool, error) {
	doc := map[string]any{}
	b, err := os.ReadFile(path)
	if err == nil && len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return false, fmt.Errorf("%s is not valid JSON; edit it by hand: %v", path, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if _, ok := servers["engram"]; ok {
		return false, nil
	}
	servers["engram"] = map[string]any{"command": exe, "args": args}
	doc["mcpServers"] = servers
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := backup(path); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(out, '\n'), 0o644)
}

// AppendTOMLServer adds an [mcp_servers.engram] table to Codex's config.toml,
// creating the file if needed. Existing content is left untouched.
func AppendTOMLServer(path, exe string, args []string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if strings.Contains(string(b), "[mcp_servers.engram]") {
		return false, nil
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = fmt.Sprintf("%q", a)
	}
	block := fmt.Sprintf("\n[mcp_servers.engram]\ncommand = %q\nargs = [%s]\n", exe, strings.Join(quoted, ", "))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := backup(path); err != nil {
		return false, err
	}
	content := string(b)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return true, os.WriteFile(path, []byte(content+block), 0o644)
}

// ClaudeHookSettings is the hook block for the permission assist (shadow mode).
func ClaudeHookSettings(exe string) map[string]any {
	entry := []map[string]any{{"matcher": "*", "hooks": []map[string]any{{"type": "command", "command": exe + " hook claude-code", "timeout": 5}}}}
	return map[string]any{"PermissionRequest": entry, "PostToolUse": entry, "PostToolUseFailure": entry}
}

// AddClaudeHooks merges the permission-assist hooks into ~/.claude/settings.json.
// Hooks already pointing at engram are not duplicated.
func AddClaudeHooks(path, exe string) (bool, error) {
	doc := map[string]any{}
	b, err := os.ReadFile(path)
	if err == nil && len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return false, fmt.Errorf("%s is not valid JSON; edit it by hand: %v", path, err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	changed := false
	for event, entries := range ClaudeHookSettings(exe) {
		existing, _ := hooks[event].([]any)
		raw, _ := json.Marshal(existing)
		if strings.Contains(string(raw), " hook claude-code") {
			continue
		}
		for _, e := range entries.([]map[string]any) {
			existing = append(existing, e)
		}
		hooks[event] = existing
		changed = true
	}
	if !changed {
		return false, nil
	}
	doc["hooks"] = hooks
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := backup(path); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(out, '\n'), 0o644)
}
