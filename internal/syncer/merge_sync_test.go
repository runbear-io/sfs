package syncer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The point of the merge, at the level that matters: two real devices, one
// file, edits in different places, no hand-merging afterwards.
func TestTwoDevicesEditingDifferentPartsMergeInsteadOfForking(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "alice", be)
	b := newDevice(t, "bob", be)

	write(t, a.Folder, "doc.md", "# Title\n\nintro\n\nbody\n\noutro\n")
	cycle(t, a)
	cycle(t, b) // bob now has the same file

	// Both edit, neither has synced: a genuine concurrent edit.
	write(t, a.Folder, "doc.md", "# TITLE FROM ALICE\n\nintro\n\nbody\n\noutro\n")
	write(t, b.Folder, "doc.md", "# Title\n\nintro\n\nbody\n\nOUTRO FROM BOB\n")

	cycle(t, a) // alice pushes first
	res := cycle(t, b)

	got := read(t, b.Folder, "doc.md")
	if !strings.Contains(got, "TITLE FROM ALICE") || !strings.Contains(got, "OUTRO FROM BOB") {
		t.Fatalf("merged file lost an edit:\n%s", got)
	}
	if res.Conflicts != 0 {
		t.Errorf("Conflicts = %d, want 0 for a clean merge", res.Conflicts)
	}
	if names := conflictCopiesIn(t, b.Folder); len(names) != 0 {
		t.Errorf("clean merge still produced conflict copies: %v", names)
	}

	// And it converges: alice pulls the merge rather than keeping her own.
	cycle(t, a)
	if a := read(t, a.Folder, "doc.md"); a != got {
		t.Fatalf("devices diverged:\n alice: %q\n bob:   %q", a, got)
	}
}

// Overlapping edits are NOT merged. A machine guessing between two rewrites of
// the same sentence is how people lose work quietly, so this keeps the
// conflict copy exactly as before.
func TestOverlappingEditsStillMakeAConflictCopy(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "alice", be)
	b := newDevice(t, "bob", be)

	write(t, a.Folder, "doc.md", "one\ntwo\nthree\n")
	cycle(t, a)
	cycle(t, b)

	write(t, a.Folder, "doc.md", "one\nALICE\nthree\n")
	write(t, b.Folder, "doc.md", "one\nBOB\nthree\n")

	cycle(t, a)
	res := cycle(t, b)

	if res.Conflicts == 0 {
		t.Fatal("two rewrites of the same line must not be merged")
	}
	if names := conflictCopiesIn(t, b.Folder); len(names) == 0 {
		t.Fatal("no conflict copy was preserved")
	}
}

// Binary content is never merged: line-merging bytes that are not lines
// produces a file that is neither side's, which for a binary format means a
// corrupt one.
func TestBinaryConflictsAreNotMerged(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "alice", be)
	b := newDevice(t, "bob", be)

	base := string([]byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02, '\n'})
	write(t, a.Folder, "img.bin", base)
	cycle(t, a)
	cycle(t, b)

	write(t, a.Folder, "img.bin", base+string([]byte{0xAA, 0x00}))
	write(t, b.Folder, "img.bin", base+string([]byte{0xBB, 0x00}))
	cycle(t, a)
	res := cycle(t, b)

	if res.Conflicts == 0 {
		t.Fatal("binary files must fall back to a conflict copy")
	}
}

func conflictCopiesIn(t *testing.T, folder string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(folder, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(info.Name(), ".bdrive-conflict-") {
			out = append(out, info.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
