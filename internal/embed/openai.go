package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAI calls an OpenAI-compatible /v1/embeddings endpoint (Ollama, llama.cpp,
// LM Studio, OpenAI, Voyage and others).
type OpenAI struct {
	url    string
	model  string
	apiKey string
	client *http.Client
}

// NewOpenAI builds a client from a config.
func NewOpenAI(c Config) *OpenAI {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	return &OpenAI{
		url:    strings.TrimRight(c.URL, "/") + "/v1/embeddings",
		model:  c.Model,
		apiKey: c.APIKey,
		client: &http.Client{Timeout: timeout},
	}
}

// Model returns the model name, which identifies the vector space.
func (o *OpenAI) Model() string { return o.model }

// prefix applies the task prefixes that nomic-embed-text expects.
func (o *OpenAI) prefix(texts []string, p Purpose) []string {
	if !strings.Contains(o.model, "nomic") {
		return texts
	}
	pre := "search_document: "
	if p == Query {
		pre = "search_query: "
	}
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = pre + t
	}
	return out
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embed returns one unit vector per input text.
func (o *OpenAI) Embed(ctx context.Context, texts []string, p Purpose) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: o.model, Input: o.prefix(texts, p)})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	var out embedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embedding server returned HTTP %d with an unreadable body", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		msg := http.StatusText(resp.StatusCode)
		if out.Error != nil && out.Error.Message != "" {
			msg = out.Error.Message
		}
		return nil, fmt.Errorf("embedding server returned HTTP %d: %s", resp.StatusCode, msg)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embedding server returned %d vectors for %d inputs", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for i, d := range out.Data {
		idx := d.Index
		if idx < 0 || idx >= len(vecs) {
			idx = i
		}
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embedding server returned an empty vector")
		}
		vecs[idx] = Normalize(d.Embedding)
	}
	return vecs, nil
}
