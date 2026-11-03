// Package text turns text into normalized content words. It is shared by
// keyword ranking (internal/store) and the offline hashing embedder (internal/embed).
package text

import (
	"strings"
	"unicode"
)

var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true,
	"do": true, "does": true, "for": true, "from": true, "how": true, "i": true, "in": true, "is": true,
	"it": true, "me": true, "my": true, "of": true, "on": true, "or": true, "our": true, "should": true,
	"so": true, "that": true, "the": true, "this": true, "to": true, "was": true, "we": true,
	"what": true, "when": true, "where": true, "which": true, "who": true, "why": true, "with": true,
	"you": true, "your": true,
}

// Tokenize lowercases text and returns content words with simple plural folding.
func Tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, w := range fields {
		if len(w) < 2 || stopwords[w] {
			continue
		}
		out = append(out, Stem(w))
	}
	return out
}

// Stem folds common English plurals: branches → branch, categories → category, passes → pass.
func Stem(w string) string {
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 4 && (strings.HasSuffix(w, "ches") || strings.HasSuffix(w, "shes") ||
		strings.HasSuffix(w, "sses") || strings.HasSuffix(w, "xes") || strings.HasSuffix(w, "zes")):
		return w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us"):
		return w[:len(w)-1]
	}
	return w
}
