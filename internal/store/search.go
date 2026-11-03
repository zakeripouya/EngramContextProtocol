package store

import (
	"math"
	"strings"
	"unicode"
)

// M1 ranks recall candidates with BM25 over words, computed in Go so the build
// needs no SQLite extensions. Milestone M2 replaces this with embeddings and
// kNN wiring, and M4 adds spreading activation (docs/design.md, "Retrieval").

var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true,
	"do": true, "does": true, "for": true, "from": true, "how": true, "i": true, "in": true, "is": true,
	"it": true, "me": true, "my": true, "of": true, "on": true, "or": true, "should": true, "so": true,
	"that": true, "the": true, "this": true, "to": true, "was": true, "we": true, "what": true,
	"when": true, "where": true, "which": true, "who": true, "why": true, "with": true, "you": true,
}

// Tokenize lowercases text and returns content words with simple plural folding.
func Tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, w := range fields {
		if len(w) < 2 || stopwords[w] {
			continue
		}
		out = append(out, stem(w))
	}
	return out
}

func stem(w string) string {
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 4 && (strings.HasSuffix(w, "ches") || strings.HasSuffix(w, "shes") ||
		strings.HasSuffix(w, "sses") || strings.HasSuffix(w, "xes") || strings.HasSuffix(w, "zes")):
		return w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:len(w)-1]
	}
	return w
}

type doc struct {
	terms map[string]int
	n     int
}

func newDoc(text string) doc {
	d := doc{terms: map[string]int{}}
	for _, t := range Tokenize(text) {
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
