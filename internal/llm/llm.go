// Package llm calls a chat model for the sleep cycle's language steps
// (distillation and verification). Like embeddings, it speaks the
// OpenAI-compatible API, so Ollama, llama.cpp, LM Studio or a hosted provider
// all work. When no model is available, those steps are skipped, never faked.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Chat sends a system and user message and returns the model's reply.
type Chat interface {
	Complete(ctx context.Context, system, user string) (string, error)
	Model() string
}

// Config selects a chat model. FromEnv fills it from ENGRAM_LLM* variables.
type Config struct {
	Provider string // "auto" (default), "openai" or "off"
	URL      string // default http://localhost:11434 (Ollama)
	Model    string // default llama3.2
	APIKey   string
	Timeout  time.Duration
}

// FromEnv reads ENGRAM_LLM (auto|openai|off), ENGRAM_LLM_URL, ENGRAM_LLM_MODEL and ENGRAM_LLM_KEY.
func FromEnv() Config {
	c := Config{
		Provider: strings.ToLower(os.Getenv("ENGRAM_LLM")),
		URL:      os.Getenv("ENGRAM_LLM_URL"),
		Model:    os.Getenv("ENGRAM_LLM_MODEL"),
		APIKey:   os.Getenv("ENGRAM_LLM_KEY"),
		Timeout:  2 * time.Minute,
	}
	if c.Provider == "" {
		c.Provider = "auto"
	}
	if c.URL == "" {
		c.URL = "http://localhost:11434"
	}
	if c.Model == "" {
		c.Model = "llama3.2"
	}
	return c
}

// New returns the configured chat model, or nil with a note when none is available.
func New(ctx context.Context, c Config) (Chat, string) {
	if c.Provider == "off" {
		return nil, "language steps off"
	}
	o := &OpenAI{url: strings.TrimRight(c.URL, "/") + "/v1/chat/completions", model: c.Model, key: c.APIKey,
		client: &http.Client{Timeout: c.Timeout}}
	if c.Provider == "openai" {
		return o, "using " + c.Model + " at " + c.URL
	}
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := o.Complete(probe, "Reply with the single word: ok", "ping"); err != nil {
		return nil, "no chat model at " + c.URL + " (" + c.Model + "); distillation and verification will be skipped"
	}
	return o, "using " + c.Model + " at " + c.URL
}

// OpenAI is a client for an OpenAI-compatible /v1/chat/completions endpoint.
type OpenAI struct {
	url, model, key string
	client          *http.Client
}

// Model returns the model name.
func (o *OpenAI) Model() string { return o.model }

// Complete returns the reply to one system + user exchange, at temperature 0.
func (o *OpenAI) Complete(ctx context.Context, system, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       o.model,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		req.Header.Set("Authorization", "Bearer "+o.key)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("chat server returned HTTP %d with an unreadable body", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || len(out.Choices) == 0 {
		msg := http.StatusText(resp.StatusCode)
		if out.Error != nil && out.Error.Message != "" {
			msg = out.Error.Message
		}
		return "", fmt.Errorf("chat server returned HTTP %d: %s", resp.StatusCode, msg)
	}
	return out.Choices[0].Message.Content, nil
}

// ExtractJSON returns the first JSON object in a reply, tolerating code fences and prose around it.
func ExtractJSON(reply string) string {
	start := strings.Index(reply, "{")
	end := strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return ""
	}
	return reply[start : end+1]
}
