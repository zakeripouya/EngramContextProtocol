package store

import (
	"regexp"
	"strings"
)

// Engram must never store credentials, secrets or payment card numbers
// (spec, "Security and privacy requirements"). These are conservative pattern
// matches: they reduce risk but are not a guarantee.
var sensitivePatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----`)},
	{"AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}\b`)},
	{"API key", regexp.MustCompile(`\bsk-(?:ant-|proj-|live_|test_)?[A-Za-z0-9_\-]{20,}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{"password assignment", regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret|api[_-]?key)\s*[:=]\s*\S{6,}`)},
}

var cardCandidate = regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)

func luhnOK(digits string) bool {
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

// FindSensitive returns a short label for the first sensitive item in text, or "".
func FindSensitive(text string) string {
	for _, p := range sensitivePatterns {
		if p.re.MatchString(text) {
			return p.label
		}
	}
	for _, m := range cardCandidate.FindAllString(text, -1) {
		digits := strings.NewReplacer(" ", "", "-", "").Replace(m)
		if len(digits) >= 13 && len(digits) <= 19 && luhnOK(digits) {
			return "payment card number"
		}
	}
	return ""
}
