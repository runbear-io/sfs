package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
)

// journalOps reads the ops this device wrote for a project.
func journalOps(t *testing.T, projectID, deviceID string) []journal.Op {
	t.Helper()
	vdir, err := config.VolumeDir(projectID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(vdir, "journal", deviceID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ops, err := journal.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// stampFixture is a mount with one file, ready to commit.
func stampFixture(t *testing.T) (folder string, proj config.Project) {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder = t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	var err error
	proj, err = config.SaveProject(folder, config.Project{
		Volume: "wiki",
		Remote: "https://hub.example.com/p/p-12345678", // unreachable: the cycle degrades offline, the scan still commits
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "a.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return folder, proj
}

func thisDevice(t *testing.T) string {
	t.Helper()
	dev, err := config.LoadDevice()
	if err != nil {
		t.Fatal(err)
	}
	return dev.ID
}

// The hook sets BOTH the note and the session id — the note is the label a
// reader sees, the session is the key a run card joins its reads on.
func TestHookStampsSession(t *testing.T) {
	folder, proj := stampFixture(t)

	c := syncCmd()
	c.SetOut(&bytes.Buffer{})
	c.SetIn(strings.NewReader(`{"session_id":"sess-42"}`))
	c.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c.Execute(); err != nil {
		t.Fatalf("hook mode must never fail: %v", err)
	}

	ops := journalOps(t, proj.ID, thisDevice(t))
	if len(ops) == 0 {
		t.Fatal("the hook run committed nothing")
	}
	for _, op := range ops {
		if op.Session != "sess-42" {
			t.Errorf("op %q Session = %q, want sess-42", op.Path, op.Session)
		}
		if op.Note != "claude-code session sess-42" {
			t.Errorf("op %q Note = %q", op.Path, op.Note)
		}
	}
}

// Landmine 1, tested explicitly: Op.Note is user-settable, so `bdrive sync
// --note` can spell out any other member's session card verbatim. It must
// still produce an EMPTY Op.Session, so nothing it writes can attach to that
// member's run — the join reads the session, never the note.
func TestSyncNoteCannotForgeASession(t *testing.T) {
	folder, proj := stampFixture(t)

	c := syncCmd()
	c.SetOut(&bytes.Buffer{})
	c.SetArgs([]string{folder, "--note", "claude-code session sess-42"})
	if err := c.Execute(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	ops := journalOps(t, proj.ID, thisDevice(t))
	if len(ops) == 0 {
		t.Fatal("the sync committed nothing")
	}
	for _, op := range ops {
		if op.Session != "" {
			t.Errorf("--note forged a session id on %q: %q", op.Path, op.Session)
		}
		if op.Note != "claude-code session sess-42" {
			t.Errorf("op %q Note = %q, want the note to still be settable", op.Path, op.Note)
		}
	}
}

// An explicit `bdrive sync` is a human act, so it must not inherit the note
// the last agent session left behind — otherwise a hand edit lands in history
// (and in that session's group, and in its rollback) attributed to the agent.
// The daemon's own scans still inherit it: that is what the TTL is for.
func TestPlainSyncClearsThePreviousSessionNote(t *testing.T) {
	folder, proj := stampFixture(t)
	dev := thisDevice(t)

	runSync := func(args ...string) {
		t.Helper()
		c := syncCmd()
		c.SetOut(&bytes.Buffer{})
		c.SetArgs(append([]string{folder}, args...))
		if err := c.Execute(); err != nil {
			t.Fatalf("sync %v: %v", args, err)
		}
	}
	noteFor := func(path string) string {
		t.Helper()
		for _, op := range journalOps(t, proj.ID, dev) {
			if op.Path == path {
				return op.Note
			}
		}
		t.Fatalf("no op for %q", path)
		return ""
	}

	// An agent session stamps its note.
	runSync("--note", "s1")
	if got := noteFor("a.md"); got != "s1" {
		t.Fatalf("a.md Note = %q, want s1", got)
	}

	// A hand edit committed by a plain `bdrive sync` is unattributed, and the
	// stored note is gone — clearing the store is what makes it unstamped,
	// since scan falls back to LoadNote whenever Session.Note is empty.
	if err := os.WriteFile(filepath.Join(folder, "b.md"), []byte("by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSync()
	if got := noteFor("b.md"); got != "" {
		t.Errorf("plain sync stamped the previous session's note: b.md Note = %q", got)
	}
	sess, _, err := openSession(context.Background(), folder, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := sess.Store.LoadNote(); got != "" {
		t.Errorf("stored note = %q after a plain sync, want it cleared", got)
	}
	closeSession(sess)

	// The daemon's path (Session.Cycle direct, no CLI) still inherits a live
	// note inside its TTL.
	runSync("--note", "s2")
	if err := os.WriteFile(filepath.Join(folder, "c.md"), []byte("daemon tick\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess, _, err = openSession(context.Background(), folder, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	closeSession(sess)
	if got := noteFor("c.md"); got != "s2" {
		t.Errorf("daemon-committed change lost the live note: c.md Note = %q, want s2", got)
	}
}
