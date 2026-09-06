package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
)

// verifyLocal runs Verify against a device the way `bdrive verify` does with
// no --remote: the device's own store and ID, no backend.
func verifyLocal(t *testing.T, s *Session) VerifyReport {
	t.Helper()
	rep, err := Verify(context.Background(), s.Folder, nil, s.Store, s.Device.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func verifyRemoteRun(t *testing.T, s *Session) VerifyReport {
	t.Helper()
	rep, err := Verify(context.Background(), s.Folder, nil, s.Store, s.Device.ID, s.Backend)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RemoteErr != nil {
		t.Fatalf("unexpected RemoteErr: %v", rep.RemoteErr)
	}
	return rep
}

func wantEmpty(t *testing.T, name string, got []string) {
	t.Helper()
	if len(got) != 0 {
		t.Fatalf("%s = %v, want empty", name, got)
	}
}

func wantOnly(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

// TestVerifyCleanAfterConverge: two devices that have converged both report a
// fully empty verdict — nothing drifted, nothing pending, nothing missing.
func TestVerifyCleanAfterConverge(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "doc.txt", "v1")
	write(t, a.Folder, "sub/nested.md", "deep")
	cycle(t, a)
	cycle(t, b)

	for _, d := range []*Session{a, b} {
		rep := verifyLocal(t, d)
		if rep.Problems() != 0 {
			t.Fatalf("%s: Problems() = %d, want 0 (%+v)", d.Device.ID, rep.Problems(), rep)
		}
		if rep.Files != 2 {
			t.Fatalf("%s: hashed %d file(s), want 2", d.Device.ID, rep.Files)
		}
		if rep.Bytes == 0 {
			t.Fatalf("%s: Bytes should be > 0", d.Device.ID)
		}
	}
}

// TestVerifyCatchesRestoredMtime is the criterion Drift cannot meet: bytes
// changed, size and mtime put back. Drift compares size+mtime and sees
// nothing; Verify hashes and catches it.
func TestVerifyCatchesRestoredMtime(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))
	write(t, a.Folder, "doc.txt", "aaaa")
	cycle(t, a)

	abs := filepath.Join(a.Folder, "doc.txt")
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	// Same length, different bytes, original mtime restored.
	if err := os.WriteFile(abs, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(abs, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}

	rep := verifyLocal(t, a)
	wantOnly(t, "Drifted", rep.Drifted, "doc.txt")

	// And the whole point: the cheap check reports nothing.
	added, modified, removed, err := Drift(a.Folder, nil, "", cacheOf(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if added+modified+removed != 0 {
		t.Fatalf("Drift saw %d/%d/%d changes; it is supposed to be blind to this", added, modified, removed)
	}
}

// cacheOf is the materialization cache Drift compares against, for the mount
// the test session materializes into.
func cacheOf(t *testing.T, s *Session) map[string]store.CachedFile {
	t.Helper()
	c, err := s.Store.LoadCache(s.mountID())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestVerifyMissingLocally: a path the journal holds and this folder does not.
func TestVerifyMissingLocally(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "keep.txt", "k")
	write(t, a.Folder, "gone.txt", "g")
	cycle(t, a)
	cycle(t, b)

	// Delete behind the daemon's back — no cycle, so no delete op.
	if err := os.Remove(filepath.Join(b.Folder, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	rep := verifyLocal(t, b)
	wantOnly(t, "MissingLocally", rep.MissingLocally, "gone.txt")
	wantEmpty(t, "Drifted", rep.Drifted)
	if rep.NotFetched != 0 {
		t.Fatalf("NotFetched = %d, want 0 — the blob is right here", rep.NotFetched)
	}
}

// TestVerifyIgnoredPathNotMissing is the --only false-alarm case: a path the
// LOCAL filter excludes is legitimately absent from disk, because the rules
// are applied symmetrically in scan and materialize.
func TestVerifyIgnoredPathNotMissing(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "keep.txt", "k")
	write(t, a.Folder, "secret/thing.txt", "s")
	cycle(t, a)
	cycle(t, b)

	// B narrows its scope and drops the excluded path, the way materialize
	// would on the next cycle.
	if err := os.RemoveAll(filepath.Join(b.Folder, "secret")); err != nil {
		t.Fatal(err)
	}
	rules := "secret/\n"
	write(t, b.Folder, IgnoreFile, rules)
	sync, err := b.Store.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	sync.IgnoreAccepted = rules
	if err := b.Store.SaveSync(sync); err != nil {
		t.Fatal(err)
	}

	rep := verifyLocal(t, b)
	wantEmpty(t, "MissingLocally", rep.MissingLocally)

	// Negative control: without the rule the very same folder DOES report it,
	// so the assertion above is about the filter and not about the path
	// having quietly fallen out of the journal state.
	if err := os.Remove(filepath.Join(b.Folder, IgnoreFile)); err != nil {
		t.Fatal(err)
	}
	sync.IgnoreAccepted = ""
	if err := b.Store.SaveSync(sync); err != nil {
		t.Fatal(err)
	}
	if rep := verifyLocal(t, b); len(rep.MissingLocally) != 1 || rep.MissingLocally[0] != "secret/thing.txt" {
		t.Fatalf("without the rule MissingLocally = %v, want [secret/thing.txt]", rep.MissingLocally)
	}
}

// TestVerifyNeverPushed: an offline cycle commits ops the hub never sees.
func TestVerifyNeverPushed(t *testing.T) {
	a := newDevice(t, "deva", nil) // nil backend: nothing can be pushed
	write(t, a.Folder, "notes/draft.md", "wip")
	cycle(t, a)

	rep := verifyLocal(t, a)
	wantOnly(t, "NeverPushed", rep.NeverPushed, "notes/draft.md")
	wantEmpty(t, "Drifted", rep.Drifted)
	wantEmpty(t, "NotYetScanned", rep.NotYetScanned)
}

// TestVerifyNotYetScanned: a file on disk the filter syncs, with no op at all.
func TestVerifyNotYetScanned(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))
	write(t, a.Folder, "doc.txt", "v1")
	cycle(t, a)

	write(t, a.Folder, "fresh.txt", "brand new") // no cycle
	rep := verifyLocal(t, a)
	wantOnly(t, "NotYetScanned", rep.NotYetScanned, "fresh.txt")
	wantEmpty(t, "Drifted", rep.Drifted)
	wantEmpty(t, "MissingLocally", rep.MissingLocally)
}

// TestVerifyRemoteChunkedFile is the manifests/<sha> criterion: a file over
// chunkThreshold is pushed as chunks plus a manifest keyed by the file's own
// sha, never blobs/<sha>. A --remote check that asked only blobs/ would report
// every large file as missing from the hub.
func TestVerifyRemoteChunkedFile(t *testing.T) {
	dir := t.TempDir()
	a := newDevice(t, "deva", fileRemote(t, dir))
	big := strings.Repeat("beardrive chunked payload — 64 bytes of filler here!!\n", (chunkThreshold/53)+200_000/53)
	if len(big) <= chunkThreshold {
		t.Fatalf("test payload is %d bytes, need > %d", len(big), chunkThreshold)
	}
	write(t, a.Folder, "video/demo.bin", big)
	write(t, a.Folder, "small.txt", "tiny")
	cycle(t, a)

	// The test is only meaningful if the big file really took the chunked
	// path: its content must be reachable ONLY under manifests/<sha>, so a
	// check that asked blobs/<sha> alone would have to report it missing.
	sum, err := hashFile(filepath.Join(a.Folder, "video/demo.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifests", sum)); err != nil {
		t.Fatalf("the >4MiB file was not chunked, so this test proves nothing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs", sum)); err == nil {
		t.Fatal("the >4MiB file is also a whole blob, so this test proves nothing")
	}

	rep := verifyRemoteRun(t, a)
	wantEmpty(t, "MissingOnHub", rep.MissingOnHub)
	if rep.Problems() != 0 {
		t.Fatalf("Problems() = %d, want 0 (%+v)", rep.Problems(), rep)
	}
}

// TestVerifyRemoteMissingBlob: content this device believes is synced, gone
// from the hub's storage.
func TestVerifyRemoteMissingBlob(t *testing.T) {
	dir := t.TempDir()
	be := fileRemote(t, dir)
	a := newDevice(t, "deva", be)

	write(t, a.Folder, "doc.txt", "v1")
	cycle(t, a)
	if rep := verifyRemoteRun(t, a); rep.Problems() != 0 {
		t.Fatalf("before removal: Problems() = %d, want 0 (%+v)", rep.Problems(), rep)
	}

	// Blow the blob out of the remote directory.
	blobs := filepath.Join(dir, "blobs")
	entries, err := os.ReadDir(blobs)
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(blobs, e.Name())); err != nil {
			t.Fatal(err)
		}
		removed++
	}
	if removed == 0 {
		t.Fatal("no blobs in the remote to remove")
	}

	rep := verifyRemoteRun(t, a)
	wantOnly(t, "MissingOnHub", rep.MissingOnHub, "doc.txt")
	// The local half is still clean — the bytes are on this disk.
	wantEmpty(t, "Drifted", rep.Drifted)
	if rep.Elapsed <= 0 {
		t.Fatal("Elapsed should be measured")
	}
}

// fileRemote is sharedRemote against a directory the test can reach into, so
// it can delete a blob out from under a converged device.
func fileRemote(t *testing.T, dir string) remote.Backend {
	t.Helper()
	be, err := remote.Open(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	return be
}
