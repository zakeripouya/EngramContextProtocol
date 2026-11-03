package embed

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/zakeripouya/EngramContextProtocol/internal/text"
)

// DefaultHashDims is the vector size of the offline hashing embedder.
const DefaultHashDims = 384

// Hashing is an offline embedder: it hashes content words and word pairs into
// a fixed-size vector (the "hashing trick"). Similar texts share words and so
// land close together, but it has no notion of meaning: "car" and "vehicle"
// are unrelated to it. It exists so Engram works with no embedding server, and
// it makes tests deterministic.
type Hashing struct{ dims int }

// NewHashing returns a hashing embedder with the given number of dimensions.
func NewHashing(dims int) *Hashing { return &Hashing{dims: dims} }

// Model identifies the hashing vector space.
func (h *Hashing) Model() string { return fmt.Sprintf("hash-%d", h.dims) }

// Embed returns one unit vector per text. Purpose is ignored.
func (h *Hashing) Embed(_ context.Context, texts []string, _ Purpose) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, h.dims)
		words := text.Tokenize(t)
		for j, w := range words {
			h.add(v, w, 1)
			if j > 0 {
				h.add(v, words[j-1]+" "+w, 0.5)
			}
		}
		out[i] = Normalize(v)
	}
	return out, nil
}

func (h *Hashing) add(v []float32, feature string, weight float32) {
	f := fnv.New64a()
	_, _ = f.Write([]byte(feature))
	sum := f.Sum64()
	idx := int(sum % uint64(h.dims))
	if sum>>63 == 1 { // a sign bit keeps collisions from only ever adding up
		weight = -weight
	}
	v[idx] += weight
}
