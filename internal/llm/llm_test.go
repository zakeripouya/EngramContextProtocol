package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompleteSendsMessagesAndReadsReply(t *testing.T) {
	var got struct {
		Model       string              `json:"model"`
		Temperature float64             `json:"temperature"`
		Messages    []map[string]string `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}]}`))
	}))
	defer srv.Close()
	c, note := New(context.Background(), Config{Provider: "openai", URL: srv.URL, Model: "llama3.2"})
	if c == nil {
		t.Fatal(note)
	}
	reply, err := c.Complete(context.Background(), "sys", "user")
	if err != nil || reply != "hello" {
		t.Fatalf("got %q, %v", reply, err)
	}
	if got.Model != "llama3.2" || got.Temperature != 0 || got.Messages[0]["role"] != "system" || got.Messages[1]["content"] != "user" {
		t.Fatalf("unexpected request %+v", got)
	}
}

func TestAutoWithoutServerReturnsNil(t *testing.T) {
	c, note := New(context.Background(), Config{Provider: "auto", URL: "http://127.0.0.1:1", Model: "x"})
	if c != nil || !strings.Contains(note, "skipped") {
		t.Fatalf("want nil chat and a skip note, got %v %q", c, note)
	}
}

func TestExtractJSON(t *testing.T) {
	if got := ExtractJSON("Sure!\n```json\n{\"a\": 1}\n```"); got != `{"a": 1}` {
		t.Fatalf("got %q", got)
	}
	if ExtractJSON("no json here") != "" {
		t.Fatal("expected empty")
	}
}
