// Package secrets is the one credential-shaped-string rule set, shared by the
// two places that look: the hub's share gate (internal/webapp/shares.go, which
// refuses to mint) and the sync scan (internal/syncer, which only warns). It
// imports the standard library and nothing else, so the syncer can use it
// without depending on the server.
//
// Both callers say the same thing about time: the check ran when the bytes
// were read — at the moment you shared it, or the moment the file last
// changed. Neither one ever claims a file is clean.
//
// The matched text never leaves this package. Rule ids and line numbers only,
// in a 409 body, in `bdrive status`, in an agent's context, in a log line.
package secrets

import (
	"bytes"
	"regexp"
	"sort"
)

// ScanLimit is how much of a file the share gate reads. The boundary is
// a decision, not an accident: a key past the first MiB mints silently, which
// is asserted in shares_test.go so nobody "fixes" it by accident.
const ScanLimit = 1 << 20

// Finding is one credential-shaped string: which rule fired, and where.
// Never the matched text — see Scan.
type Finding struct {
	Rule string `json:"rule"`
	Line int    `json:"line"`
}

var secretRules = []struct {
	id string
	re *regexp.Regexp
}{
	{"aws_access_key_id", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	// The bodies below are what keep the prefixes off prose: a bare `sk-` in a
	// sentence is not a key. If one still fires on real docs, tighten the body
	// rather than dropping the rule — `--force` and Share anyway are the
	// escape hatch, which is why they ship in the same change.
	{"openai_api_key", regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`)},
	{"github_pat", regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`)},
	{"slack_token", regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"gitlab_pat", regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`)},
}

// labels are the rule ids in words. They existed only in the frontend
// (SECRET_LABELS in Browser.tsx) while `bdrive share` printed the bare id, so
// the same finding read as "an AWS access key" in the browser and
// "aws_access_key_id" in a terminal. TestLabelCoversEveryRule is what stops a
// seventh rule from shipping as a bare id again.
var labels = map[string]string{
	"aws_access_key_id": "an AWS access key",
	"openai_api_key":    "an OpenAI API key",
	"github_pat":        "a GitHub token",
	"slack_token":       "a Slack token",
	"private_key":       "a private key",
	"gitlab_pat":        "a GitLab token",
}

// Label renders a rule id for a human. An id with no label renders as itself —
// a bare id is ugly, but a rule that reports nothing is worse.
func Label(rule string) string {
	if l, ok := labels[rule]; ok {
		return l
	}
	return rule
}

// Scan reports credential-shaped strings in buf, as rule ids and line
// numbers ONLY. The matched text must never reach a response body, a log line,
// or a metric label — the same argument reads.go:28-40 makes for actor
// identity, and a 409 body is the easiest place in the codebase to leak it.
//
// Byte-oriented on purpose: a bufio.Scanner over a 1 MiB minified file with no
// newline blows its 64 KiB token limit and returns nothing at all, which is a
// check that silently passes everything.
func Scan(buf []byte) []Finding {
	seen := map[Finding]bool{}
	var out []Finding
	for _, rule := range secretRules {
		for _, m := range rule.re.FindAllIndex(buf, -1) {
			f := Finding{Rule: rule.id, Line: bytes.Count(buf[:m[0]], []byte("\n")) + 1}
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}
