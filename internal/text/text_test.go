package text

import (
	"strings"
	"testing"
)

func TestTokenize(t *testing.T) {
	cases := map[string]string{
		"How should I name the Branches for tickets?": "name branch ticket",
		"Categories, boxes and passes":                "category box pass",
		"the class status":                            "class status",
		"":                                            "",
	}
	for in, want := range cases {
		if got := strings.Join(Tokenize(in), " "); got != want {
			t.Errorf("Tokenize(%q) = %q, want %q", in, got, want)
		}
	}
}
