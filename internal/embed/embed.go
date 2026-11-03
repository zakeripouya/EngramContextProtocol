// Package embed turns text into vectors for similarity search (milestone M2).
//
// Engram does not run neural networks itself. It calls any embedding service
// that speaks the OpenAI-compatible /v1/embeddings API: Ollama by default, or
// llama.cpp, LM Studio, or a hosted provider. When none is available it falls
// back to a built-in hashing embedder, which works offline and matches on
// shared words rather than meaning.
package embed

import (
	"context"
	"math"
	"os"
	"strings"
	"time"
)

// Purpose tells the embedder whether text is being stored or searched for.
// Some models (such as nomic-embed-text) expect different prefixes for each.
type Purpose int

const (
	Document Purpose = iota
	Query
)

// Embedder turns text into unit-length vectors.
type Embedder interface {
	// Embed returns one vector per input, normalized to unit length.
	Embed(ctx context.Context, texts []string, purpose Purpose) ([][]float32, error)
	// Model identifies the vector space. Vectors from different models are never compared.
	Model() string
}

// Config selects an embedder. FromEnv fills it from ENGRAM_EMBED* variables.
type Config struct {
	Provider string // "auto" (default), "openai", "hash" or "off"
	URL      string // base URL of an OpenAI-compatible server; default http://localhost:11434 (Ollama)
	Model    string // model name; default nomic-embed-text
	APIKey   string // optional bearer token for hosted providers
	Timeout  time.Duration
}

// FromEnv reads ENGRAM_EMBED (auto|openai|hash|off), ENGRAM_EMBED_URL, ENGRAM_EMBED_MODEL and ENGRAM_EMBED_KEY.
func FromEnv() Config {
	c := Config{
		Provider: strings.ToLower(os.Getenv("ENGRAM_EMBED")),
		URL:      os.Getenv("ENGRAM_EMBED_URL"),
		Model:    os.Getenv("ENGRAM_EMBED_MODEL"),
		APIKey:   os.Getenv("ENGRAM_EMBED_KEY"),
		Timeout:  15 * time.Second,
	}
	if c.Provider == "" {
		c.Provider = "auto"
	}
	if c.URL == "" {
		c.URL = "http://localhost:11434"
	}
	if c.Model == "" {
		c.Model = "nomic-embed-text"
	}
	return c
}

// New builds the embedder a config asks for. With "auto" it uses the
// OpenAI-compatible server if it answers a test request within a few seconds,
// and otherwise the offline hashing embedder. It returns nil for "off".
func New(ctx context.Context, c Config) (Embedder, string) {
	switch c.Provider {
	case "off":
		return nil, "embeddings off; recall uses keyword ranking only"
	case "hash":
		return NewHashing(DefaultHashDims), "using the offline hashing embedder"
	case "openai":
		return NewOpenAI(c), "using " + c.Model + " at " + c.URL
	default:
		o := NewOpenAI(c)
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if _, err := o.Embed(probe, []string{"engram"}, Document); err == nil {
			return o, "using " + c.Model + " at " + c.URL
		}
		return NewHashing(DefaultHashDims), "no embedding server at " + c.URL +
			"; using the offline hashing embedder (run `engram reembed` after starting one)"
	}
}

// Normalize scales v to unit length in place and returns it. A zero vector stays zero.
func Normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	n := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= n
	}
	return v
}

// Cosine returns the cosine similarity of two unit vectors (their dot product).
// Vectors of different length have similarity 0.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}
