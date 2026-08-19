package syncer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func drift(t *testing.T, s *Session) (added, modified, removed int) {
	t.Helper()
	cache, err := s.Store.LoadCache(s.MountID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Store.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	a, m, r, err := Drift(s.Folder, nil, st.IgnoreAccepted, cache)
	if err != nil {
		t.Fatal(err)
	}
	return a, m, r
}

func fileSum(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "(absent)"
		}
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestDriftSeesUnscannedWork is the bug BEA-106 reports: with no cycle in
// between (the daemon stopped), work on disk is in neither the cache nor the
// journal, and `bdrive status` called it clean.
func TestDriftSeesUnscannedWork(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))
	a.MountID = "m1"
	write(t, a.Folder, "index.md", "one")
	write(t, a.Folder, "docs/keep.md", "keep")
	write(t, a.Folder, "docs/gone.md", "gone")
	cycle(t, a)

	if add, mod, rm := drift(t, a); add|mod|rm != 0 {
		t.Fatalf("clean folder drifted: %d added, %d modified, %d removed", add, mod, rm)
	}

	// No cycle from here on: this is the stopped-daemon case.
	write(t, a.Folder, "index.md", "one\ntwo")         // modified
	write(t, a.Folder, "notes/new.md", "new")          // added
	os.Remove(filepath.Join(a.Folder, "docs/gone.md")) // removed

	add, mod, rm := drift(t, a)
	if add != 1 || mod != 1 || rm != 1 {
		t.Fatalf("drift = %d added, %d modified, %d removed; want 1, 1, 1", add, mod, rm)
	}

	// And the cycle that follows agrees: three ops, no more, no fewer.
	res := cycle(t, a)
	if res.LocalOps != 3 {
		t.Fatalf("cycle after drift journalled %d ops, want 3", res.LocalOps)
	}
	if add, mod, rm := drift(t, a); add|mod|rm != 0 {
		t.Fatalf("drift after cycle = %d, %d, %d; want all zero", add, mod, rm)
	}
}

// TestDriftWritesNothing is the load-bearing one: `status` must stay a pure
// read. Any op, any journal line, any cache rewrite from this call would mean
// the command someone runs when sync is stuck changed what it was describing.
func TestDriftWritesNothing(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))
	a.MountID = "m1"
	write(t, a.Folder, "index.md", "one")
	cycle(t, a)

	statePath := filepath.Join(a.Store.Dir(), "state-m1.json")
	journalPath := a.Store.JournalPath(a.Device.ID)
	beforeState, beforeJournal := fileSum(t, statePath), fileSum(t, journalPath)

	write(t, a.Folder, "index.md", "one\ntwo")
	write(t, a.Folder, "brand-new.md", "new")

	cache, err := a.Store.LoadCache("m1")
	if err != nil {
		t.Fatal(err)
	}
	nCache := len(cache)
	if _, _, _, err := Drift(a.Folder, nil, "", cache); err != nil {
		t.Fatal(err)
	}
	if len(cache) != nCache {
		t.Fatalf("Drift mutated the cache it was handed: %d entries, was %d", len(cache), nCache)
	}
	if got := fileSum(t, statePath); got != beforeState {
		t.Fatal("Drift rewrote the state cache")
	}
	if got := fileSum(t, journalPath); got != beforeJournal {
		t.Fatal("Drift wrote to the device journal")
	}
	// Nothing was committed, so the very next cycle still has both changes.
	if res := cycle(t, a); res.LocalOps != 2 {
		t.Fatalf("cycle after Drift journalled %d ops, want 2", res.LocalOps)
	}
}

// TestDriftRespectsIgnore: an edit the cycle would never send is not drift.
func TestDriftRespectsIgnore(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))
	a.MountID = "m1"
	write(t, a.Folder, ".bdriveignore", "build/\n")
	write(t, a.Folder, "index.md", "one")
	cycle(t, a)

	write(t, a.Folder, "build/out.bin", "junk")
	write(t, a.Folder, "build/nested/more.bin", "junk")
	if add, mod, rm := drift(t, a); add|mod|rm != 0 {
		t.Fatalf("ignored paths counted as drift: %d, %d, %d", add, mod, rm)
	}

	// A path that becomes ignored after it was synced is dropped from the
	// cache without a delete op — so it is not "removed" drift either.
	write(t, a.Folder, "secret.md", "s")
	cycle(t, a)
	write(t, a.Folder, ".bdriveignore", "build/\nsecret.md\n")
	if _, _, rm := drift(t, a); rm != 0 {
		t.Fatalf("newly ignored path counted as removed: %d", rm)
	}
}
