package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/runbear-io/beardrive/internal/secrets"
)

func TestSecretsRoundtripAndIsolation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]secrets.Finding{"deploy.md": {{Rule: "aws_access_key_id", Line: 12}}}
	if err := s.SaveSecrets("m1", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSecrets("m1")
	if err != nil || len(got["deploy.md"]) != 1 || got["deploy.md"][0].Line != 12 {
		t.Fatalf("roundtrip = %+v, %v", got, err)
	}
	if other, err := s.LoadSecrets("m2"); err != nil || len(other) != 0 {
		t.Fatalf("mounts must be isolated: %+v %v", other, err)
	}
	// A mount id with a separator would put the file wherever its author
	// chose — .bdrive/config.json travels with the folder, so it is checked.
	if _, err := s.LoadSecrets("../evil"); err == nil {
		t.Fatal("LoadSecrets accepted a mount id with a separator")
	}
}

// The record is plain JSON in $BDRIVE_HOME that anything running as the user
// can write, and its keys are rendered into a terminal and matched against
// cache keys. Same treatment as LoadCache's: an out-of-volume key is dropped
// on the way in, not on the way out.
func TestLoadSecretsDropsForeignKeys(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.secretsPath("m1")
	if err != nil {
		t.Fatal(err)
	}
	const raw = `{
	  "notes.md":        [{"rule":"private_key","line":1}],
	  "../../secret.md": [{"rule":"private_key","line":1}],
	  "/etc/shadow":     [{"rule":"private_key","line":1}],
	  "empty.md":        []
	}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSecrets("m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got["notes.md"]) != 1 {
		t.Fatalf("LoadSecrets = %+v, want only notes.md", got)
	}

	// A torn file starts the mount empty rather than wedging every later
	// cycle: these findings are advisory, and the next scan rewrites them.
	if err := os.WriteFile(p, []byte(`{"notes.md": [{"rule":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSecrets("m1"); err == nil {
		t.Fatal("a torn record must report its error to the caller that ignores it")
	}

	// And the file it wrote is 0600 — it lists a private project's paths.
	if err := s.SaveSecrets("m1", map[string][]secrets.Finding{"notes.md": {{Rule: "private_key", Line: 1}}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(s.Dir(), "secrets-m1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}
