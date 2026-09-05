package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// scopedRemote is a hub that answers "what may this account write here?" —
// the remote.Scoper capability a real bdrive hub implements and a raw object
// store does not.
type scopedRemote struct {
	remote.Backend
	readOnly []string
	tag      string
	err      error
}

func (s scopedRemote) Scope(context.Context) (remote.Scope, error) {
	if s.err != nil {
		return remote.Scope{}, s.err
	}
	return remote.Scope{Tag: s.tag, ReadOnly: s.readOnly}, nil
}

// TestReadOnlyFolderRevertsLocalEditAndNeverPushes is Phase 1 of
// docs/folder-permissions-prd.md at the level that matters: two devices, one
// shared remote, and a folder device B may read but not write.
//
// The property under test is not only "B's change does not reach A". It is
// that B's sync KEEPS WORKING — the reason the client half of this feature
// exists at all. The hub refuses a journal PUT whole, and a journal is
// append-only, so a device that journals one refused op wedges forever.
func TestReadOnlyFolderRevertsLocalEditAndNeverPushes(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}, tag: "t1"})

	write(t, a.Folder, "locked/policy.md", "v1")
	write(t, a.Folder, "notes/open.md", "hello")
	cycle(t, a)

	cycle(t, b)
	if got := read(t, b.Folder, "locked/policy.md"); got != "v1" {
		t.Fatalf("B did not receive the read-only folder: %q", got)
	}

	// B edits the read-only file, and something outside the rule in the same
	// cycle — the control that says this narrows one subtree, not the mount.
	write(t, b.Folder, "locked/policy.md", "hacked")
	write(t, b.Folder, "notes/mine.md", "b was here")
	res := cycle(t, b)

	if got := read(t, b.Folder, "locked/policy.md"); got != "v1" {
		t.Errorf("B's edit was not reverted: %q", got)
	}
	// The user's bytes are never dropped.
	if got := read(t, b.Folder, "locked/policy (local, not synced).md"); got != "hacked" {
		t.Errorf("B's own edit was not preserved beside the file: %q", got)
	}
	if len(res.Reverted) != 1 || res.Reverted[0] != "locked/policy.md" {
		t.Errorf("Reverted = %v, want [locked/policy.md]", res.Reverted)
	}
	// Nothing under the rule was journaled — not the edit, and not the copy.
	ops, err := b.Store.DeviceOps(b.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if strings.HasPrefix(op.Path, "locked/") {
			t.Errorf("B journaled %q under a read-only folder", op.Path)
		}
	}
	if len(ops) == 0 {
		t.Fatal("B journaled nothing at all — the rule swallowed the whole cycle")
	}

	// B's sync is not wedged: the work outside the rule reaches A.
	cycle(t, a)
	if got := read(t, a.Folder, "notes/mine.md"); got != "b was here" {
		t.Errorf("B's writable work did not reach A: %q", got)
	}
	if got := read(t, a.Folder, "locked/policy.md"); got != "v1" {
		t.Errorf("B's read-only edit reached A: %q", got)
	}
	if _, err := os.Stat(filepath.Join(a.Folder, "locked/policy (local, not synced).md")); err == nil {
		t.Error("B's local copy was synced to A — it is that machine's business")
	}
}

// Deleting a read-only file is a local edit like any other: not journaled, and
// put back on the same cycle. Without this a member could remove a folder they
// may only read and simply stop receiving it, with no error anywhere.
func TestReadOnlyFolderRestoresALocalDelete(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}})

	write(t, a.Folder, "locked/policy.md", "v1")
	cycle(t, a)
	cycle(t, b)

	if err := os.Remove(filepath.Join(b.Folder, "locked", "policy.md")); err != nil {
		t.Fatal(err)
	}
	cycle(t, b)
	if got := read(t, b.Folder, "locked/policy.md"); got != "v1" {
		t.Fatalf("a locally deleted read-only file was not restored: %q", got)
	}
	ops, err := b.Store.DeviceOps(b.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if strings.HasPrefix(op.Path, "locked/") {
			t.Fatalf("B journaled %q — a delete under a read-only folder", op.Path)
		}
	}
	cycle(t, a)
	if got := read(t, a.Folder, "locked/policy.md"); got != "v1" {
		t.Fatalf("B's delete reached A: %q", got)
	}
}

// A read-only file nobody touched must not be rewritten every cycle: the drift
// check is one Stat, and a revert that fires on an untouched file would make
// every cycle report activity and re-log an inbound event to every agent turn.
func TestReadOnlyFolderIsQuietWhenUntouched(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}})
	write(t, a.Folder, "locked/policy.md", "v1")
	cycle(t, a)
	cycle(t, b)
	res := cycle(t, b)
	if res.Activity() || len(res.Reverted) != 0 {
		t.Fatalf("an untouched read-only folder produced activity: %+v", res)
	}
}

// The scope is persisted, so a cycle that cannot reach the hub keeps honouring
// the last answer. Widening to "everything is writable" the moment the network
// blinks would build up a journal the hub refuses whole when it returns —
// which is the wedge this whole mechanism exists to avoid.
func TestReadOnlyScopeSurvivesAnUnreachableHub(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}, tag: "t1"})
	write(t, a.Folder, "locked/policy.md", "v1")
	cycle(t, a)
	cycle(t, b)

	// The hub is now unreachable for the scope question specifically.
	b.Backend = scopedRemote{Backend: be, err: context.DeadlineExceeded}
	write(t, b.Folder, "locked/policy.md", "hacked")
	cycle(t, b)

	if got := read(t, b.Folder, "locked/policy.md"); got != "v1" {
		t.Fatalf("an unreachable hub widened the scope: %q", got)
	}
	ops, err := b.Store.DeviceOps(b.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if strings.HasPrefix(op.Path, "locked/") {
			t.Fatalf("B journaled %q while the hub was unreachable", op.Path)
		}
	}
}

// A hub too old to know about folder permissions answers ErrNoScope, which is
// "nothing is restricted here" — distinct from "unreachable", and it must
// clear a stale list rather than leave a device restricted forever by an
// answer no hub is giving any more.
func TestNoScopeSupportClearsTheRestriction(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}})
	write(t, a.Folder, "locked/policy.md", "v1")
	cycle(t, a)
	cycle(t, b)

	b.Backend = scopedRemote{Backend: be, err: remote.ErrNoScope}
	write(t, b.Folder, "locked/policy.md", "v2")
	cycle(t, b)

	if got := read(t, b.Folder, "locked/policy.md"); got != "v2" {
		t.Fatalf("a hub with no folder permissions still restricted the write: %q", got)
	}
	cycle(t, a)
	if got := read(t, a.Folder, "locked/policy.md"); got != "v2" {
		t.Fatalf("the edit did not reach A: %q", got)
	}
}

// The remote may be a hostile hub — the premise pull already applies to its
// journal listing. This list decides what the device will never journal again,
// so an empty prefix would match every path and stop the device syncing its
// own work at all, silently. One bad prefix must not disarm the good ones
// either.
func TestHostileScopeAnswerIsNotObeyed(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{
		"",         // matches everything
		"/",        // ditto
		"no-slash", // would match "no-slashed.md" too
		"../etc/",  // not a path this mount can hold
		"locked/",  // the only real one
	}})

	write(t, a.Folder, "locked/policy.md", "v1")
	cycle(t, a)
	cycle(t, b)

	// The good prefix still applies...
	write(t, b.Folder, "locked/policy.md", "hacked")
	// ...and everything else still syncs, which the empty prefix would have
	// stopped without a single error anywhere.
	write(t, b.Folder, "notes/mine.md", "b was here")
	write(t, b.Folder, "no-slashed.md", "not under a rule")
	cycle(t, b)

	if got := read(t, b.Folder, "locked/policy.md"); got != "v1" {
		t.Errorf("the real rule stopped applying: %q", got)
	}
	cycle(t, a)
	if got := read(t, a.Folder, "notes/mine.md"); got != "b was here" {
		t.Errorf("an empty prefix silently stopped B syncing: %q", got)
	}
	if got := read(t, a.Folder, "no-slashed.md"); got != "not under a rule" {
		t.Errorf("a non-slash-terminated prefix swallowed a sibling path: %q", got)
	}
}
