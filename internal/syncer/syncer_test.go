package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/secrets"
	"github.com/runbear-io/beardrive/internal/store"
)

// TestPushProgress verifies the push phase reports upload progress: the total
// is the number of unique blobs, Done climbs to that total, and byte totals
// are populated. (Done isn't strictly ordered across the parallel workers, so
// we only assert it reaches the total.)
func TestPushProgress(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))
	const n = 25
	for i := 0; i < n; i++ {
		write(t, a.Folder, fmt.Sprintf("f%02d.txt", i), fmt.Sprintf("unique content for file %d — pad pad pad", i))
	}
	var mu sync.Mutex
	var total, maxDone int
	var toBytes int64
	a.OnProgress = func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		total = p.Total
		toBytes = p.ToBytes
		if p.Done > maxDone {
			maxDone = p.Done
		}
	}
	cycle(t, a)
	if total != n {
		t.Fatalf("progress Total = %d, want %d", total, n)
	}
	if maxDone != n {
		t.Fatalf("progress reached Done = %d, want %d", maxDone, n)
	}
	if toBytes == 0 {
		t.Fatal("progress ToBytes should be > 0")
	}
}

// newDevice simulates one device: its own folder, volume store, and identity,
// all syncing through a shared file:// remote.
func newDevice(t *testing.T, name string, backend remote.Backend) *Session {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	return &Session{
		Folder:  t.TempDir(),
		Store:   st,
		Device:  config.Device{ID: name, Name: name, Author: name + "@test"},
		Backend: backend,
	}
}

func sharedRemote(t *testing.T) remote.Backend {
	t.Helper()
	be, err := remote.Open(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return be
}

func write(t *testing.T, folder, rel, content string) {
	t.Helper()
	abs := filepath.Join(folder, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, folder, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(folder, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func cycle(t *testing.T, s *Session) *Result {
	t.Helper()
	res, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Offline {
		t.Fatalf("unexpected offline: %v", res.OfflineErr)
	}
	return res
}

func TestOfflineCycle(t *testing.T) {
	a := newDevice(t, "deva", nil)
	write(t, a.Folder, "notes/hello.md", "hi")
	res := cycle(t, a)
	if res.LocalOps != 1 {
		t.Fatalf("LocalOps = %d, want 1", res.LocalOps)
	}
	// idempotent: second cycle sees no changes
	res = cycle(t, a)
	if res.Activity() {
		t.Fatalf("second cycle should be quiet, got %+v", res)
	}
}

func TestTwoDeviceSync(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	// A creates files, B receives them
	write(t, a.Folder, "doc.txt", "v1")
	write(t, a.Folder, "sub/nested.txt", "deep")
	cycle(t, a)
	res := cycle(t, b)
	if res.PulledOps != 2 || res.Materialized != 2 {
		t.Fatalf("b pull: %+v", res)
	}
	if read(t, b.Folder, "doc.txt") != "v1" || read(t, b.Folder, "sub/nested.txt") != "deep" {
		t.Fatal("content mismatch after sync")
	}

	// B edits, A receives the update
	time.Sleep(10 * time.Millisecond) // ensure mtime moves
	write(t, b.Folder, "doc.txt", "v2 from b")
	cycle(t, b)
	cycle(t, a)
	if got := read(t, a.Folder, "doc.txt"); got != "v2 from b" {
		t.Fatalf("a got %q", got)
	}

	// B deletes, A's copy disappears
	os.Remove(filepath.Join(b.Folder, "sub", "nested.txt"))
	cycle(t, b)
	cycle(t, a)
	if _, err := os.Stat(filepath.Join(a.Folder, "sub", "nested.txt")); !os.IsNotExist(err) {
		t.Fatal("delete did not propagate")
	}
	// empty dir pruned
	if _, err := os.Stat(filepath.Join(a.Folder, "sub")); !os.IsNotExist(err) {
		t.Fatal("empty dir not pruned")
	}
}

func TestHistoryTracksDeviceAndAuthor(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "f.txt", "from a")
	cycle(t, a)
	cycle(t, b)
	time.Sleep(10 * time.Millisecond)
	write(t, b.Folder, "f.txt", "from b")
	cycle(t, b)
	cycle(t, a)

	entries, err := LogEntries(a.Store, "f.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 history entries, got %d: %+v", len(entries), entries)
	}
	// newest first
	if entries[0].Author != "devb@test" || entries[0].DeviceName != "devb" {
		t.Fatalf("newest entry should be devb's: %+v", entries[0])
	}
	if entries[1].Author != "deva@test" {
		t.Fatalf("oldest entry should be deva's: %+v", entries[1])
	}
}

func TestConcurrentEditConflictPreserved(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	// shared base
	write(t, a.Folder, "shared.txt", "base")
	cycle(t, a)
	cycle(t, b)

	// both edit before syncing
	time.Sleep(10 * time.Millisecond)
	write(t, a.Folder, "shared.txt", "edit from a")
	write(t, b.Folder, "shared.txt", "edit from b")
	cycle(t, a) // a pushes first
	cycle(t, b) // b scans its edit, pulls a's, loses or wins deterministically
	cycle(t, a) // a converges
	cycle(t, b)

	aContent := read(t, a.Folder, "shared.txt")
	bContent := read(t, b.Folder, "shared.txt")
	if aContent != bContent {
		t.Fatalf("devices diverged: %q vs %q", aContent, bContent)
	}

	// both versions must survive somewhere (winner at path, loser as conflict copy)
	all := map[string]bool{aContent: true}
	for _, folder := range []string{a.Folder, b.Folder} {
		entries, err := os.ReadDir(folder)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".bdrive-conflict-") {
				all[read(t, folder, e.Name())] = true
			}
		}
	}
	if !all["edit from a"] || !all["edit from b"] {
		t.Fatalf("a version was lost; surviving: %v", all)
	}
}

func TestMountExistingFolderImports(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "pre-existing.txt", "I was here first")
	res := cycle(t, a)
	if res.LocalOps != 1 || !res.Pushed {
		t.Fatalf("import failed: %+v", res)
	}

	b := newDevice(t, "devb", be)
	cycle(t, b)
	if read(t, b.Folder, "pre-existing.txt") != "I was here first" {
		t.Fatal("existing file not imported/synced")
	}
}

func TestIgnoredFiles(t *testing.T) {
	a := newDevice(t, "deva", nil)
	write(t, a.Folder, ".DS_Store", "junk")
	write(t, a.Folder, ".git/config", "gitstuff")
	write(t, a.Folder, "real.txt", "data")
	res := cycle(t, a)
	if res.LocalOps != 1 {
		t.Fatalf("ignores leaked into journal: %+v", res)
	}
}

func TestOfflineThenReconnect(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)

	// work offline
	a.Backend = nil
	write(t, a.Folder, "offline.txt", "written offline")
	cycle(t, a)

	// reconnect: pending ops push
	a.Backend = be
	res := cycle(t, a)
	if !res.Pushed {
		t.Fatalf("reconnect should push pending ops: %+v", res)
	}

	b := newDevice(t, "devb", be)
	cycle(t, b)
	if read(t, b.Folder, "offline.txt") != "written offline" {
		t.Fatal("offline edit did not propagate after reconnect")
	}
}

func TestSameVolumeMountedAtTwoFolders(t *testing.T) {
	// One device mounts the same volume at two folders (e.g. ./shared in two
	// repos). They share the store (blobs+journals) but have separate mount
	// caches, and content propagates between them even with no remote.
	st, err := store.Open(filepath.Join(t.TempDir(), "volume"))
	if err != nil {
		t.Fatal(err)
	}
	dev := config.Device{ID: "dev1", Name: "dev1", Author: "dev1@test"}
	m1 := &Session{Folder: t.TempDir(), MountID: "mount1", Store: st, Device: dev}
	m2 := &Session{Folder: t.TempDir(), MountID: "mount2", Store: st, Device: dev}

	write(t, m1.Folder, "shared.md", "from folder one")
	cycle(t, m1)
	res := cycle(t, m2)
	if res.Materialized != 1 {
		t.Fatalf("folder two should materialize the file: %+v", res)
	}
	if read(t, m2.Folder, "shared.md") != "from folder one" {
		t.Fatal("content did not propagate between mounts")
	}

	// edit in folder two propagates back
	time.Sleep(10 * time.Millisecond)
	write(t, m2.Folder, "shared.md", "edited in folder two")
	cycle(t, m2)
	cycle(t, m1)
	if read(t, m1.Folder, "shared.md") != "edited in folder two" {
		t.Fatal("edit did not propagate back to folder one")
	}
}

func TestExecutableBitPreserved(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	abs := filepath.Join(a.Folder, "run.sh")
	os.WriteFile(abs, []byte("#!/bin/sh\necho hi\n"), 0o755)
	cycle(t, a)

	b := newDevice(t, "devb", be)
	cycle(t, b)
	fi, err := os.Stat(filepath.Join(b.Folder, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("exec bit lost: %v", fi.Mode())
	}
}

// TestNestedMountExcluded verifies that a subdirectory which is a BearDrive
// mount of its own (has .bdrive/config.json) is fenced off from the parent
// mount: the parent scanner never journals its files, dropping it emits no
// delete ops toward peers, and remote state is never materialized into it.
func TestNestedMountExcluded(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	// Both devices converge on a folder that includes team/.
	write(t, a.Folder, "readme.md", "root")
	write(t, a.Folder, "team/notes.md", "v1")
	cycle(t, a)
	cycle(t, b)
	if got := read(t, b.Folder, "team/notes.md"); got != "v1" {
		t.Fatalf("b team/notes.md = %q, want v1", got)
	}

	// team/ becomes a nested mount on A (its own project).
	write(t, a.Folder, "team/.bdrive/config.json", `{"mount_id":"m-nested"}`)
	write(t, a.Folder, "team/local.md", "only for the nested project")
	res := cycle(t, a)
	if res.LocalOps != 0 {
		t.Fatalf("a journaled %d ops for nested-mount content, want 0", res.LocalOps)
	}

	// B must keep its copy (no delete propagated) and never see new files.
	res = cycle(t, b)
	if got := read(t, b.Folder, "team/notes.md"); got != "v1" {
		t.Fatalf("b team/notes.md = %q after a's cycle, want v1 (no deletes)", got)
	}
	if _, err := os.Stat(filepath.Join(b.Folder, "team/local.md")); !os.IsNotExist(err) {
		t.Fatal("nested-mount file leaked to peer")
	}

	// B edits inside team/; A must not materialize over its nested mount.
	write(t, b.Folder, "team/notes.md", "v2")
	cycle(t, b)
	cycle(t, a)
	if got := read(t, a.Folder, "team/notes.md"); got != "v1" {
		t.Fatalf("a team/notes.md = %q, want v1 (nested mount not written)", got)
	}

	// Paths outside the nested mount keep syncing both ways.
	write(t, a.Folder, "readme.md", "root v2")
	cycle(t, a)
	cycle(t, b)
	if got := read(t, b.Folder, "readme.md"); got != "root v2" {
		t.Fatalf("b readme.md = %q, want root v2", got)
	}
}

// gated wraps a backend and refuses the operations the hub would refuse for a
// given permission level, with the same sentinel the http backend produces.
// The flags are read on every call so a test can revoke (or restore) access
// mid-run, which is exactly the case that used to look like a network fault.
type gated struct {
	remote.Backend
	read  *atomic.Bool // pulls allowed (List/Get)
	write *atomic.Bool // pushes allowed (Put)
}

func newGated(be remote.Backend) *gated {
	g := &gated{Backend: be, read: &atomic.Bool{}, write: &atomic.Bool{}}
	g.read.Store(true)
	g.write.Store(true)
	return g
}

func (g *gated) List(ctx context.Context, prefix string) ([]remote.Object, error) {
	if !g.read.Load() {
		return nil, fmt.Errorf("%w: server: 403 Forbidden", remote.ErrForbidden)
	}
	return g.Backend.List(ctx, prefix)
}

func (g *gated) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if !g.read.Load() {
		return nil, fmt.Errorf("%w: server: 403 Forbidden", remote.ErrForbidden)
	}
	return g.Backend.Get(ctx, key)
}

func (g *gated) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if !g.write.Load() {
		return fmt.Errorf("%w: server: 403 Forbidden", remote.ErrForbidden)
	}
	return g.Backend.Put(ctx, key, r, size)
}

// A read-only device keeps pulling its teammates' changes and journals its own
// edits locally, but nothing of its own ever reaches the remote — and the
// cycle says ReadOnly, never Offline, so the user is told rather than left
// watching a silent retry loop.
func TestReadOnlyDevicePullsOnly(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	gate := newGated(be)
	b := newDevice(t, "devb", gate)

	write(t, a.Folder, "shared.md", "from A")
	cycle(t, a)

	gate.write.Store(false) // B is downgraded to read
	write(t, b.Folder, "mine.md", "local only")
	res, err := b.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.ReadOnly || res.Offline {
		t.Fatalf("read-only push: %+v, want ReadOnly and not Offline", res)
	}
	if got := read(t, b.Folder, "shared.md"); got != "from A" {
		t.Fatalf("b shared.md = %q — a read-only device must still pull", got)
	}
	// B's own edit is journaled locally...
	ops, err := b.Store.DeviceOps(b.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Path != "mine.md" {
		t.Fatalf("b's local journal = %+v, want one op for mine.md", ops)
	}
	// ...and never lands in the shared remote, however many cycles run.
	for i := 0; i < 3; i++ {
		if _, err := b.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	c := newDevice(t, "devc", be)
	cycle(t, c)
	if _, err := os.Stat(filepath.Join(c.Folder, "mine.md")); !os.IsNotExist(err) {
		t.Fatal("a read-only device's edit reached the remote")
	}
	// The state is persisted so `bdrive status` can report it without a cycle.
	if st, err := b.Store.LoadSync(); err != nil || st.Access != store.AccessReadOnly {
		t.Fatalf("persisted access = %q (%v), want read-only", st.Access, err)
	}

	// Restoring write self-heals: the held-back op finally goes out.
	gate.write.Store(true)
	if res := cycle(t, b); !res.Pushed {
		t.Fatalf("re-granted device did not push: %+v", res)
	}
	cycle(t, c)
	if got := read(t, c.Folder, "mine.md"); got != "local only" {
		t.Fatalf("c mine.md = %q after the re-grant", got)
	}
	if st, _ := b.Store.LoadSync(); st.Access != store.AccessOK {
		t.Fatalf("persisted access = %q after re-grant, want cleared", st.Access)
	}
}

// refusingPush is a hub that answers every push with a real 403 body. The one
// that matters is the device-registration refusal: it is not about project
// permissions at all, so the user who checks their permissions finds `write`
// and has nowhere left to look — the hub's sentence is the only thing that
// points at the fix.
type refusingPush struct {
	remote.Backend
	msg string
}

func (p refusingPush) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	return fmt.Errorf("%w: server: 403 Forbidden: %s", remote.ErrForbidden, p.msg)
}

// A refused device must stay refused in its own records until the hub says
// otherwise — and it must be able to say WHY.
//
// Both halves come from one report: a mount whose journal pushes 403'd on every
// remote pass, while `bdrive status` said access was fine and the daemon log
// alternated "read-only on this project" / "access restored; syncing normally"
// every few seconds. The cycle recomputed access from scratch at the end of
// every pass, including the cheap local-only ones the daemon runs three of
// between remote passes, so the hub's answer was overwritten by a cycle that
// never asked it anything. And the answer itself — "this device is not
// registered to your account on this hub; run `bdrive login`" — was summarized
// into "read-only (pull only)" and lost.
func TestRefusedPushKeepsItsVerdictAndItsReason(t *testing.T) {
	const refusal = "this device is not registered to your account on this hub; run `bdrive login` on this machine"
	be := sharedRemote(t)
	b := newDevice(t, "devb", refusingPush{Backend: be, msg: refusal})

	write(t, b.Folder, "mine.md", "local only")
	res, err := b.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.ReadOnly {
		t.Fatalf("a refused push should report ReadOnly: %+v", res)
	}
	if res.Reason() != refusal {
		t.Fatalf("Result.Reason() = %q, want the hub's own sentence %q", res.Reason(), refusal)
	}
	st, err := b.Store.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	if st.Access != store.AccessReadOnly || st.AccessReason != refusal {
		t.Fatalf("persisted access = %q/%q, want read-only + the hub's reason", st.Access, st.AccessReason)
	}

	// The daemon's local-only tick: same session, no backend at all. It learns
	// nothing about the hub, so it must not clear the hub's last answer.
	b.Backend = nil
	write(t, b.Folder, "second.md", "still local")
	if res := cycle(t, b); res.ReadOnly {
		t.Fatalf("a cycle with no remote leg cannot discover a refusal: %+v", res)
	}
	st, _ = b.Store.LoadSync()
	if st.Access != store.AccessReadOnly || st.AccessReason != refusal {
		t.Fatalf("a local-only tick reset access to %q/%q — `bdrive status` reports healthy "+
			"sync moments after the push the hub refused, and the daemon logs "+
			"\"access restored\" between every pair of remote passes", st.Access, st.AccessReason)
	}

	// Only the hub clears it: a push that lands does, and takes the stale
	// reason with it.
	b.Backend = be
	if res := cycle(t, b); !res.Pushed {
		t.Fatalf("re-granted device did not push: %+v", res)
	}
	if st, _ := b.Store.LoadSync(); st.Access != store.AccessOK || st.AccessReason != "" {
		t.Fatalf("after a successful push, access = %q/%q, want cleared", st.Access, st.AccessReason)
	}
}

// A hub message that could repaint the terminal it lands in is dropped rather
// than rendered: it reaches a log file, `bdrive status`, and an agent's context
// verbatim, and the hub is the one string source here nobody local vouches for.
func TestAccessReasonRefusesTerminalControls(t *testing.T) {
	esc := fmt.Errorf("%w: server: 403 Forbidden: nope\x1b[2Kaccess restored", remote.ErrForbidden)
	if got := accessReason(esc); got != "" {
		t.Errorf("accessReason kept a control sequence: %q", got)
	}
	long := fmt.Errorf("%w: server: 403 Forbidden: %s", remote.ErrForbidden, strings.Repeat("x", 400))
	if got := accessReason(long); got != "" {
		t.Errorf("accessReason kept a %d-rune message", len([]rune(got)))
	}
	if got := accessReason(nil); got != "" {
		t.Errorf("accessReason(nil) = %q", got)
	}
}

// A device whose access is revoked entirely pauses: the cycle reports
// NoAccess (not Offline), the working folder is left byte-for-byte alone —
// revoking access must never look like the hub deleting someone's files — and
// re-granting resumes normal sync with no manual step.
func TestNoAccessPausesSync(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	gate := newGated(be)
	c := newDevice(t, "devc", gate)

	write(t, a.Folder, "doc.md", "v1")
	cycle(t, a)
	cycle(t, c)
	if got := read(t, c.Folder, "doc.md"); got != "v1" {
		t.Fatalf("c doc.md = %q, want v1", got)
	}

	// A moves on while C's access is cut.
	write(t, a.Folder, "doc.md", "v2")
	write(t, a.Folder, "new.md", "after the cut")
	cycle(t, a)

	gate.read.Store(false)
	gate.write.Store(false)
	before := snapshotDir(t, c.Folder)
	write(t, c.Folder, "cs-own.md", "written while cut off")
	before["cs-own.md"] = "written while cut off"

	for i := 0; i < 3; i++ {
		res, err := c.Cycle(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !res.NoAccess || res.Offline {
			t.Fatalf("cycle %d: %+v, want NoAccess and not Offline", i, res)
		}
		if res.Materialized != 0 || res.Pushed {
			t.Fatalf("cycle %d touched the folder or pushed: %+v", i, res)
		}
	}
	if got := snapshotDir(t, c.Folder); !maps.Equal(got, before) {
		t.Fatalf("working folder changed while access was revoked:\n got %v\nwant %v", got, before)
	}
	if st, _ := c.Store.LoadSync(); st.Access != store.AccessNone {
		t.Fatalf("persisted access = %q, want no-access", st.Access)
	}

	// Re-granting needs no intervention: the next cycle converges both ways.
	gate.read.Store(true)
	gate.write.Store(true)
	cycle(t, c)
	if got := read(t, c.Folder, "doc.md"); got != "v2" {
		t.Fatalf("c doc.md = %q after the re-grant, want v2", got)
	}
	if got := read(t, c.Folder, "new.md"); got != "after the cut" {
		t.Fatalf("c new.md = %q after the re-grant", got)
	}
	cycle(t, a)
	if got := read(t, a.Folder, "cs-own.md"); got != "written while cut off" {
		t.Fatalf("a cs-own.md = %q — C's held-back edit should arrive", got)
	}
}

// snapshotDir reads every file under folder (excluding .bdrive) as path→content.
func snapshotDir(t *testing.T, folder string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(folder, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, config.ProjectDir+"/") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func touch(t *testing.T, folder, rel string, when time.Time) {
	t.Helper()
	abs := filepath.Join(folder, filepath.FromSlash(rel))
	if err := os.Chtimes(abs, when, when); err != nil {
		t.Fatal(err)
	}
}

// TestLogDisplayOrder builds the skew that made `bdrive log` unreadable:
// device B commits after pulling A, so B's op carries the HIGHER lamport,
// while B's file was written hours EARLIER on the wall clock.
//
// The display sort follows the commit clock: early.md was written two hours
// ago but only ARRIVED in the project on B's cycle, and "what changed" means
// what arrived. Its own write time still reaches the reader — `bdrive log`
// prints it alongside — but it is not what orders the list. Before BEA-112
// the write time was the sort key, which is how the two halves of one rename
// landed a minute apart.
func TestLogDisplayOrder(t *testing.T) {
	be := sharedRemote(t)
	a, b := newDevice(t, "deva", be), newDevice(t, "devb", be)

	write(t, b.Folder, "early.md", "written two hours ago")
	touch(t, b.Folder, "early.md", time.Now().Add(-2*time.Hour))
	write(t, a.Folder, "late.md", "written just now")

	cycle(t, a) // lamport 1
	cycle(t, b) // pulls A (lamport 1), commits its own op at lamport 2
	cycle(t, a) // pull B, so A's store holds both journals

	entries, err := LogEntries(a.Store, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	// Precondition: the two orders really do disagree. Without this, the
	// assertion below would pass even if SortForDisplay did nothing.
	if entries[0].Path != "early.md" {
		t.Fatalf("causal order should lead with the higher-lamport op early.md, got %q", entries[0].Path)
	}

	SortForDisplay(entries)
	if entries[0].Path != "early.md" {
		t.Fatalf("display order should lead with the most recently journaled file early.md, got %q", entries[0].Path)
	}
	// Its write time is two hours old and still available to print.
	if gap := entries[0].Time.Sub(DisplayTime(entries[0])); gap < time.Hour {
		t.Fatalf("early.md's write time was lost: commit %v, display %v", entries[0].Time, DisplayTime(entries[0]))
	}
	assertNonIncreasing(t, entries)
}

// TestLogDisplayTimeIsEditTime covers the other half of the report: 22 ops of
// one agent run all printed the same stamp because Time is the commit time.
// Two files written a minute apart and committed in ONE cycle must carry two
// different display times.
func TestLogDisplayTimeIsEditTime(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))
	write(t, a.Folder, "first.md", "one")
	write(t, a.Folder, "second.md", "two")
	touch(t, a.Folder, "first.md", time.Now().Add(-2*time.Minute))
	touch(t, a.Folder, "second.md", time.Now().Add(-1*time.Minute))
	cycle(t, a)

	entries, err := LogEntries(a.Store, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	byPath := map[string]journal.Op{}
	for _, op := range entries {
		if op.Mtime.IsZero() {
			t.Fatalf("put op %s has no Mtime", op.Path)
		}
		// Commit times are microseconds apart — indistinguishable at the
		// second resolution `bdrive log` prints, which is why one scan used
		// to collapse a whole agent run onto a single stamp.
		if d := op.Time.Sub(entries[0].Time); d > time.Second || d < -time.Second {
			t.Fatalf("expected both ops committed in one batch, Time differs by %v", d)
		}
		byPath[op.Path] = op
	}
	gap := DisplayTime(byPath["second.md"]).Sub(DisplayTime(byPath["first.md"]))
	if gap < 30*time.Second {
		t.Fatalf("display times only %v apart; one scan collapsed them again", gap)
	}

	SortForDisplay(entries)
	if entries[0].Path != "second.md" {
		t.Fatalf("newest edit should lead, got %q", entries[0].Path)
	}
}

// TestSortForDisplayFallsBackToTime covers journals written before Op.Mtime
// existed, and deletes, which never carry one: they sort and print by Time and
// must not sink to the bottom as zero-time rows.
func TestSortForDisplayFallsBackToTime(t *testing.T) {
	base := time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC)
	mk := func(path string, lamport int64, kind string, commit, mtime time.Time) journal.Op {
		return journal.Op{
			Seq: lamport, Lamport: lamport, Time: commit, Mtime: mtime,
			Device: "deva", Kind: kind, Path: path,
		}
	}
	ops := []journal.Op{
		mk("legacy-old.md", 1, journal.KindPut, base, time.Time{}), // no mtime
		mk("gone.md", 2, journal.KindDelete, base.Add(3*time.Minute), time.Time{}),
		mk("legacy-new.md", 3, journal.KindPut, base.Add(4*time.Minute), time.Time{}),
		mk("fresh.md", 4, journal.KindPut, base.Add(9*time.Minute), base.Add(2*time.Minute)),
	}
	SortForDisplay(ops)

	// Ordered by commit time. legacy ops and deletes carry no mtime, and the
	// point of the fallback is that they sort on their commit time like
	// everything else rather than sinking to the bottom as zero-time rows.
	want := []string{"fresh.md", "legacy-new.md", "gone.md", "legacy-old.md"}
	for i, w := range want {
		if ops[i].Path != w {
			t.Fatalf("order[%d] = %q, want %q (full: %v)", i, ops[i].Path, w, paths(ops))
		}
	}
	assertNonIncreasing(t, ops)
}

func paths(ops []journal.Op) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.Path
	}
	return out
}

// assertNonIncreasing pins BEA-40's guarantee on the key the display sort now
// uses: `bdrive log` reads strictly newest-first by commit time, the column it
// prints. DisplayTime is deliberately not monotone down the list — an old file
// journaled today belongs at the top wearing its old write time.
func assertNonIncreasing(t *testing.T, ops []journal.Op) {
	t.Helper()
	for i := 1; i < len(ops); i++ {
		if CommitTime(ops[i]).After(CommitTime(ops[i-1])) {
			t.Fatalf("display order not newest-first at %d: %v then %v",
				i, CommitTime(ops[i-1]), CommitTime(ops[i]))
		}
	}
}

// A peer's journal is untrusted input: the scan-side exclusions (.bdrive/,
// .git/) never ran on the device that wrote it. Materializing an op that
// names one would let a compromised peer — or a poisoned hub — repoint this
// mount's own settings or drop a git hook that runs on the next commit.
func TestSec_Sync_PeerJournalCannotMaterializeReservedPaths(t *testing.T) {
	ctx := context.Background()
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const content = "hostile"
	sum := sha256hex(content)
	if err := be.Put(ctx, "blobs/"+sum, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	op := func(seq int64, p string) journal.Op {
		return journal.Op{
			Seq: seq, Lamport: seq, Time: time.Now().UTC(),
			Device: "attacker", DeviceName: "attacker", Author: "attacker@test",
			Kind: journal.KindPut, Path: p, Blob: sum, Size: int64(len(content)), Mode: 0o644,
		}
	}
	data, err := journal.Marshal([]journal.Op{
		op(1, ".bdrive/config.json"),
		op(2, ".git/hooks/pre-commit"),
		op(3, "docs/.bdrive/config.json"),
		op(4, "notes/ok.md"), // control: an ordinary op from the same journal
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Put(ctx, "journal/attacker.jsonl", strings.NewReader(string(data)), int64(len(data))); err != nil {
		t.Fatal(err)
	}

	cycle(t, victim)

	// Control first: if this fails the pull didn't happen and the rest proves
	// nothing.
	if got := read(t, victim.Folder, "notes/ok.md"); got != content {
		t.Fatalf("control op did not materialize: %q", got)
	}
	for _, rel := range []string{".bdrive/config.json", ".git/hooks/pre-commit", "docs/.bdrive/config.json"} {
		if _, err := os.Stat(filepath.Join(victim.Folder, filepath.FromSlash(rel))); err == nil {
			t.Errorf("a peer journal materialized %s into the mount", rel)
		}
	}
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A rename is not an op — the scanner emits a put at the new path and a
// delete at the old, in one cycle, carrying the same blob. The hub infers
// moves from exactly that shape (internal/webapp/moves.go), which only stays
// true while sync keeps producing it. Nothing in the move index touches
// journal.Less or Replay; this is what pins that.
func TestRenameConvergesAsPutPlusDelete(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "plan.md", "the plan")
	cycle(t, a)
	cycle(t, b)
	if got := read(t, b.Folder, "plan.md"); got != "the plan" {
		t.Fatalf("b before the rename = %q", got)
	}

	// The rename, exactly as a person or an editor does it.
	if err := os.MkdirAll(filepath.Join(a.Folder, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(a.Folder, "plan.md"), filepath.Join(a.Folder, "notes", "plan.md")); err != nil {
		t.Fatal(err)
	}
	res := cycle(t, a)
	if res.LocalOps != 2 {
		t.Fatalf("LocalOps = %d, want 2 (the put and the delete)", res.LocalOps)
	}
	cycle(t, b)

	if got := read(t, b.Folder, "notes/plan.md"); got != "the plan" {
		t.Fatalf("b after the rename = %q, want the plan", got)
	}
	if _, err := os.Stat(filepath.Join(b.Folder, "plan.md")); !os.IsNotExist(err) {
		t.Fatalf("the old path survived on b: %v", err)
	}

	// The two halves the hub pairs on: one device, same blob, same cycle.
	ops, err := journal.ReadFile(a.Store.JournalPath(a.Device.ID))
	if err != nil {
		t.Fatal(err)
	}
	var put, del *journal.Op
	for i := range ops {
		switch {
		case ops[i].Kind == journal.KindPut && ops[i].Path == "notes/plan.md":
			put = &ops[i]
		case ops[i].Kind == journal.KindDelete && ops[i].Path == "plan.md":
			del = &ops[i]
		}
	}
	if put == nil || del == nil {
		t.Fatalf("rename did not journal a put+delete pair: %+v", ops)
	}
	if put.Device != del.Device {
		t.Fatalf("halves on different devices: %q vs %q", put.Device, del.Device)
	}
	if want := ops[0].Blob; put.Blob != want {
		t.Fatalf("the moved file's blob changed: %q, want %q", put.Blob, want)
	}
	if d := del.Time.Sub(put.Time); d > 30*time.Second || d < -30*time.Second {
		t.Fatalf("the halves landed %v apart — wider than the hub's pairing window", d)
	}
}

// TestInboundSpool is the test that matters for the turn-start warning: the
// paths a cycle materializes from a peer are spooled for the next agent turn,
// this device's own edits never are, and the drain clears.
func TestInboundSpool(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	// B writes, A pulls: A's spool names the path.
	write(t, b.Folder, "notes/readme.md", "from b")
	cycle(t, b)
	cycle(t, a)
	evs, err := a.Store.DrainInbound()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Path != "notes/readme.md" || evs[0].Deleted {
		t.Fatalf("a inbound = %+v, want notes/readme.md written", evs)
	}

	// A's own edit is not inbound — it is scanned, not materialized.
	write(t, a.Folder, "local.md", "mine")
	cycle(t, a)
	if evs, _ := a.Store.DrainInbound(); len(evs) != 0 {
		t.Fatalf("a inbound = %+v, want own edits absent", evs)
	}

	// A peer delete is reported as removed, not as changed.
	cycle(t, b)
	os.Remove(filepath.Join(b.Folder, "notes", "readme.md"))
	cycle(t, b)
	cycle(t, a)
	evs, err = a.Store.DrainInbound()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Path != "notes/readme.md" || !evs[0].Deleted {
		t.Fatalf("a inbound = %+v, want notes/readme.md deleted", evs)
	}

	// The drain cleared: a quiet cycle reports nothing.
	cycle(t, a)
	if evs, _ := a.Store.DrainInbound(); len(evs) != 0 {
		t.Fatalf("a inbound = %+v, want empty after a quiet cycle", evs)
	}
}

// TestInboundSpoolOutlivesItsCycle is the reason this is a spool and not a
// Result field: in the ordinary case the daemon materializes a peer's change
// seconds before the turn starts, so the cycle the agent hook runs sees
// nothing. A second Session on the same volume — which is what the daemon and
// the hook are — still finds the path waiting.
func TestInboundSpoolOutlivesItsCycle(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, b.Folder, "notes/readme.md", "from b")
	cycle(t, b)

	// The "daemon" cycle materializes it.
	res := cycle(t, a)
	if res.Materialized != 1 {
		t.Fatalf("Materialized = %d, want 1", res.Materialized)
	}

	// A later, quiet cycle — the hook's — reports nothing itself...
	later := &Session{Folder: a.Folder, Store: a.Store, Device: a.Device, Backend: be}
	if res := cycle(t, later); res.Materialized != 0 {
		t.Fatalf("second cycle Materialized = %d, want 0", res.Materialized)
	}
	// ...but the spool still names what arrived.
	evs, err := later.Store.DrainInbound()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Path != "notes/readme.md" {
		t.Fatalf("inbound = %+v, want notes/readme.md from the earlier cycle", evs)
	}
}

// A fabricated, structurally-valid-looking AWS key. Not a real credential.
const testAWSKey = "AKIAIOSFODNN7EXAMPLE"

// TestSecretWarnsButStillSyncs is the whole posture of the credential check in
// one test: the file with the key in it converges exactly as any other file
// does — journaled, pushed, materialized on the peer — and the ONLY difference
// is a record on the writing device naming the path, the rule and the line.
// A regression that makes this hold the op fails here, not in production.
func TestSecretWarnsButStillSyncs(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)

	write(t, a.Folder, "deploy.md", "# Deploy\n\nrun it\n\nexport AWS_ACCESS_KEY_ID="+testAWSKey+"\n")
	res := cycle(t, a)
	if res.LocalOps != 1 {
		t.Fatalf("LocalOps = %d, want the op journaled anyway (warn, never hold)", res.LocalOps)
	}
	if !res.Pushed {
		t.Fatalf("Pushed = false, want the blob pushed anyway")
	}
	cycle(t, b)
	if got := read(t, b.Folder, "deploy.md"); !strings.Contains(got, testAWSKey) {
		t.Fatalf("peer content = %q, want the file to have synced normally", got)
	}

	found, err := a.Store.LoadSecrets(a.mountID())
	if err != nil {
		t.Fatal(err)
	}
	want := []secrets.Finding{{Rule: "aws_access_key_id", Line: 5}}
	if !reflect.DeepEqual(found["deploy.md"], want) {
		t.Fatalf("findings = %+v, want %+v for deploy.md", found, want)
	}

	// The record names the rule and the line and never the matched bytes —
	// the same contract the share-time 409 has always had.
	raw, err := os.ReadFile(filepath.Join(a.Store.Dir(), "secrets-"+a.mountID()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testAWSKey) {
		t.Fatalf("the persisted record echoes the key: %s", raw)
	}

	// The peer that only RECEIVED the file records nothing: v1 warns the
	// device that wrote the credential, and materialize is not a scan.
	if found, _ := b.Store.LoadSecrets(b.mountID()); len(found) != 0 {
		t.Fatalf("peer findings = %+v, want none (the writing device only)", found)
	}
}

// TestSecretClearsWhenFixed is the reason the record is merged per path rather
// than rewritten whole. Nearly every cycle scans zero changed files, so a
// whole-set rewrite erases the warning seconds after it appears — here the
// quiet cycle must leave it standing, and only editing the key out clears it.
func TestSecretClearsWhenFixed(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))

	write(t, a.Folder, "deploy.md", "key = "+testAWSKey+"\n")
	cycle(t, a)

	// A quiet cycle: nothing changed, so nothing was scanned.
	cycle(t, a)
	if found, _ := a.Store.LoadSecrets(a.mountID()); len(found["deploy.md"]) == 0 {
		t.Fatalf("the warning vanished on a quiet cycle: %+v", found)
	}

	// Fixing the file is the whole remedy: no command, no flag.
	write(t, a.Folder, "deploy.md", "key = read it from the environment\n")
	cycle(t, a)
	found, err := a.Store.LoadSecrets(a.mountID())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("findings = %+v, want empty once the key is gone", found)
	}

	// And a deleted file takes its warning with it.
	write(t, a.Folder, "deploy.md", "key = "+testAWSKey+"\n")
	cycle(t, a)
	if err := os.Remove(filepath.Join(a.Folder, "deploy.md")); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	if found, _ := a.Store.LoadSecrets(a.mountID()); len(found) != 0 {
		t.Fatalf("findings = %+v, want empty once the file is gone", found)
	}
}

// The check must ride the branch that already reads the file, never add a pass
// of its own: an unchanged file is not re-read, so the daemon's 3-second tick
// costs nothing on a quiet folder. Clearing the record by hand and cycling is
// the direct test — a scan that re-read unchanged files would put it back.
func TestSecretScanSkipsUnchangedFiles(t *testing.T) {
	a := newDevice(t, "deva", sharedRemote(t))

	write(t, a.Folder, "deploy.md", "key = "+testAWSKey+"\n")
	cycle(t, a)
	if found, _ := a.Store.LoadSecrets(a.mountID()); len(found) != 1 {
		t.Fatalf("findings = %+v, want deploy.md flagged", found)
	}
	if err := a.Store.SaveSecrets(a.mountID(), map[string][]secrets.Finding{}); err != nil {
		t.Fatal(err)
	}

	cycle(t, a) // nothing changed on disk
	if found, _ := a.Store.LoadSecrets(a.mountID()); len(found) != 0 {
		t.Fatalf("findings = %+v — an unchanged file was read again", found)
	}
	// Touching it back into the changed branch does report it again.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(a.Folder, "deploy.md"), future, future); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	if found, _ := a.Store.LoadSecrets(a.mountID()); len(found) != 1 {
		t.Fatalf("findings = %+v, want the touched file flagged again", found)
	}
}
