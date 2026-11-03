package store

import (
	"math"

	"github.com/zakeripouya/EngramContextProtocol/internal/text"
)

// Keyword relevance (BM25), computed in Go so the build needs no SQLite
// extensions. Since M2 it is one signal among several in recall, alongside
// vector similarity, strength and pathways.

// Tokenize returns normalized content words. Kept for callers of the M1 API.
func Tokenize(s string) []string { return text.Tokenize(s) }

type doc struct {
	terms map[string]int
	n     int
}

func newDoc(s string) doc {
	d := doc{terms: map[string]int{}}
	for _, t := range text.Tokenize(s) {
		d.terms[t]++
		d.n++
	}
	return d
}

// bm25 scores each document against the query terms. Scores are >= 0; 0 means no overlap.
func bm25(query []string, docs []doc) []float64 {
	const k1, b = 1.2, 0.75
	scores := make([]float64, len(docs))
	if len(docs) == 0 || len(query) == 0 {
		return scores
	}
	avg := 0.0
	for _, d := range docs {
		avg += float64(d.n)
	}
	avg /= float64(len(docs))
	if avg == 0 {
		avg = 1
	}
	seen := map[string]bool{}
	for _, q := range query {
		if seen[q] {
			continue
		}
		seen[q] = true
		df := 0
		for _, d := range docs {
			if d.terms[q] > 0 {
				df++
			}
		}
		if df == 0 {
			continue
		}
		idf := math.Log(1 + (float64(len(docs))-float64(df)+0.5)/(float64(df)+0.5))
		for i, d := range docs {
			tf := float64(d.terms[q])
			if tf == 0 {
				continue
			}
			scores[i] += idf * tf * (k1 + 1) / (tf + k1*(1-b+b*float64(d.n)/avg))
		}
	}
	return scores
}
