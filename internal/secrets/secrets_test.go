package secrets

import (
	"reflect"
	"strings"
	"testing"
)

// Fabricated, structurally-valid-looking strings. None is a real credential.
const (
	fakeAWSKey    = "AKIAIOSFODNN7EXAMPLE"
	fakeOpenAIKey = "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	fakeGitHubPAT = "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789ab"
	fakeSlackTok  = "xoxb-1234567890-abcdefghij"
	fakeGitLabPAT = "glpat-abcdefghij0123456789XY"
	fakePrivKey   = "-----BEGIN RSA PRIVATE KEY-----"
)

func TestScanSecrets(t *testing.T) {
	for _, tc := range []struct {
		name string
		buf  string
		want []Finding
	}{
		{"clean", "# Notes\n\nnothing to see, sk- is just a prefix here\n", nil},
		{"aws", "line1\nline2\nkey = " + fakeAWSKey + "\n", []Finding{{"aws_access_key_id", 3}}},
		{"openai", "OPENAI=" + fakeOpenAIKey, []Finding{{"openai_api_key", 1}}},
		{"github", "\n\n" + fakeGitHubPAT, []Finding{{"github_pat", 3}}},
		{"slack", "token: " + fakeSlackTok, []Finding{{"slack_token", 1}}},
		{"gitlab", "x\n" + fakeGitLabPAT, []Finding{{"gitlab_pat", 2}}},
		{"private key", "a\nb\nc\n" + fakePrivKey + "\nMIIE...\n", []Finding{{"private_key", 4}}},
		{
			// One line, one rule, three keys: one finding, not three.
			"multi key line deduped",
			"a=" + fakeAWSKey + " b=AKIAZZZZZZZZZZZZZZZZ c=AKIAYYYYYYYYYYYYYYYY",
			[]Finding{{"aws_access_key_id", 1}},
		},
		{
			"two rules same line",
			"env: " + fakeAWSKey + " " + fakeSlackTok,
			[]Finding{{"aws_access_key_id", 1}, {"slack_token", 1}},
		},
		{
			// A bufio.Scanner would blow its 64 KiB token limit here and report
			// nothing at all — which is why Scan is byte-oriented.
			"no newline in a big buffer",
			strings.Repeat("x", 300_000) + fakeAWSKey,
			[]Finding{{"aws_access_key_id", 1}},
		},
		{
			"sorted by line then rule",
			fakeSlackTok + "\n" + fakeAWSKey,
			[]Finding{{"slack_token", 1}, {"aws_access_key_id", 2}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Scan([]byte(tc.buf))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Scan = %v, want %v", got, tc.want)
			}
		})
	}
}

// A rule id reaches a human in three places (the share dialog, `bdrive share`,
// `bdrive status`), and two of them go through Label. A new rule without a
// label ships as `openai_api_key_v2` in a terminal, which is the failure this
// catches at the only point that knows both lists.
func TestLabelCoversEveryRule(t *testing.T) {
	for _, r := range secretRules {
		if Label(r.id) == r.id {
			t.Errorf("rule %q has no human label", r.id)
		}
	}
	if got := Label("not_a_rule"); got != "not_a_rule" {
		t.Errorf("Label(unknown) = %q, want the id back", got)
	}
}
