package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// mkProject writes a project folder under root and returns its path.
func mkProject(t *testing.T, root, name, id string) string {
	t.Helper()
	folder := filepath.Join(root, name)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveProject(folder, Project{ID: id, Volume: name}); err != nil {
		t.Fatal(err)
	}
	return folder
}

// TestWorkspaceManifestRoundTrip: a root indexes its project children, the
// paths it stores are relative to the root (so the root can be renamed or
// moved), and the kind is written whether or not the caller set it.
func TestWorkspaceManifestRoundTrip(t *testing.T) {
	root := t.TempDir()
	mkProject(t, root, "beardrive-folder-1", "m-aaaaaaaa")
	mkProject(t, root, "beardrive-folder-2", "m-bbbbbbbb")
	if err := os.MkdirAll(filepath.Join(root, "non-beardrive-folder-1"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := InitWorkspace(root); err != nil {
		t.Fatalf("InitWorkspace: %v", err)
	}
	w, ok, err := LoadWorkspace(root)
	if err != nil || !ok {
		t.Fatalf("LoadWorkspace: %v (ok=%v)", err, ok)
	}
	if w.Kind != WorkspaceKind {
		t.Fatalf("kind = %q, want %q", w.Kind, WorkspaceKind)
	}
	if len(w.Projects) != 2 {
		t.Fatalf("projects = %+v, want the two mounts and not the plain folder", w.Projects)
	}
	for _, p := range w.Projects {
		if filepath.IsAbs(p.Path) || strings.Contains(p.Path, root) {
			t.Fatalf("path %q is not relative to the root", p.Path)
		}
	}
	if w.Projects[0].Path != "beardrive-folder-1" || w.Projects[0].ID != "m-aaaaaaaa" {
		t.Fatalf("first entry = %+v", w.Projects[0])
	}
	if w.Projects[1].Path != "beardrive-folder-2" || w.Projects[1].ID != "m-bbbbbbbb" {
		t.Fatalf("second entry = %+v", w.Projects[1])
	}

	// Moving the root keeps every entry resolvable: nothing stored an
	// absolute path.
	moved := filepath.Join(t.TempDir(), "renamed")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	w2, ok, err := LoadWorkspace(moved)
	if err != nil || !ok {
		t.Fatalf("LoadWorkspace after move: %v (ok=%v)", err, ok)
	}
	for _, p := range w2.Projects {
		if _, ok, err := LoadProject(filepath.Join(moved, p.Path)); err != nil || !ok {
			t.Fatalf("entry %q does not resolve under the moved root: %v (ok=%v)", p.Path, err, ok)
		}
	}

	// A project folder is not a root: its config.json is a different file, so
	// there is nothing here to mistake for a manifest.
	if _, ok, err := LoadWorkspace(filepath.Join(moved, "beardrive-folder-1")); err != nil || ok {
		t.Fatalf("LoadWorkspace on a project folder: %v (ok=%v), want (nil, false)", err, ok)
	}
	// A folder with nothing in it is simply not a root.
	if _, ok, err := LoadWorkspace(t.TempDir()); err != nil || ok {
		t.Fatalf("LoadWorkspace on a plain folder: %v (ok=%v), want (nil, false)", err, ok)
	}
	// A manifest that does not describe itself as one is an error, not an
	// empty workspace: something else is using the name.
	odd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(odd, ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspaceConfigPath(odd), []byte(`{"kind":"something-else"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadWorkspace(odd); err == nil {
		t.Fatal("LoadWorkspace on a foreign workspace.json: want an error")
	}
	if IsWorkspaceRoot(odd) {
		t.Fatal("IsWorkspaceRoot on a foreign workspace.json: want false")
	}
}

// writeRootConfig plants a manifest at the path a PROJECT config lives at —
// the layout DESIGN.md first specified, and the one a hand-edited or older
// root can still produce. It must never read as a project.
func writeRootConfig(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfigPath(root), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadProjectRefusesWorkspace: a manifest carries no id, which the legacy
// empty-id rule reads as a valid project with no identity — handing every
// caller downstream a project whose volume path is built from "". LoadProject
// must refuse it and name what it found.
func TestLoadProjectRefusesWorkspace(t *testing.T) {
	root := t.TempDir()
	writeRootConfig(t, root, `{"kind":"workspace","projects":[]}`)

	p, ok, err := LoadProject(root)
	if err == nil {
		t.Fatalf("LoadProject at a workspace config: want an error, got %+v (ok=%v)", p, ok)
	}
	if !strings.Contains(err.Error(), "workspace root") {
		t.Fatalf("error %q does not name a workspace root", err)
	}
	if ok || p.ID != "" {
		t.Fatalf("refused load still returned a project: %+v (ok=%v)", p, ok)
	}
	// Not merely "no id": a manifest listing projects is refused the same.
	writeRootConfig(t, root, `{"kind":"workspace","projects":[{"path":"a","id":"m-aaaaaaaa"}]}`)
	if _, _, err := LoadProject(root); err == nil {
		t.Fatal("LoadProject at a populated workspace config: want an error")
	}

	// The real layout keeps the two files apart, so a root simply has no
	// project config at all.
	clean := t.TempDir()
	if err := InitWorkspace(clean); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LoadProject(clean); err != nil || ok {
		t.Fatalf("LoadProject at a real workspace root: %v (ok=%v), want (nil, false)", err, ok)
	}
	if _, err := os.Stat(projectConfigPath(clean)); !os.IsNotExist(err) {
		t.Fatal("a workspace root wrote a project config.json")
	}
}

// TestIsMountFalseAtWorkspaceRoot: a root must not read as a mount to any
// caller (the scanner, the nesting guard, syncTargets). It does not, and needs
// no code to say so: the manifest has its own file name, so a real root has no
// config.json to stat. IsMount stays a stat on purpose — see the collided case
// below.
func TestIsMountFalseAtWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	if err := InitWorkspace(root); err != nil {
		t.Fatal(err)
	}
	if IsMount(root) {
		t.Fatal("IsMount at a workspace root: want false")
	}
	if !IsWorkspaceRoot(root) {
		t.Fatal("IsWorkspaceRoot at a workspace root: want true")
	}

	// A manifest hand-planted in config.json instead DOES read as a mount, and
	// deliberately so: IsMount runs per directory in the syncer's walk, where
	// reading each config to check its kind hangs a scan on one wedged file
	// that a stat completes. The agent-hook walk-up matches on the same file
	// name for the same reason, so Go and shell give the same answer rather
	// than disagreeing about a layout nothing writes. LoadProject — which has
	// already read the bytes, so the check is free there — still refuses it.
	collided := t.TempDir()
	writeRootConfig(t, collided, `{"kind":"workspace","projects":[]}`)
	if !IsMount(collided) {
		t.Fatal("IsMount at a hand-planted workspace config.json: want true (a stat is all it does)")
	}
	if _, _, err := LoadProject(collided); err == nil {
		t.Fatal("LoadProject must still refuse a workspace config")
	}

	project := mkProject(t, t.TempDir(), "wiki", "m-cccccccc")
	if !IsMount(project) {
		t.Fatal("IsMount at a project: want true")
	}
	if IsWorkspaceRoot(project) {
		t.Fatal("IsWorkspaceRoot at a project: want false")
	}

	// Unparseable: still a mount, so a parent's scanner never treats it as
	// plain files.
	broken := t.TempDir()
	if err := os.MkdirAll(filepath.Join(broken, ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfigPath(broken), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsMount(broken) {
		t.Fatal("IsMount with an unparseable config: want true")
	}

	if IsMount(t.TempDir()) {
		t.Fatal("IsMount on a plain folder: want false")
	}
}

// TestWorkspaceRescanCorrectsStaleEntry: the manifest is an index, so a
// hand-edited or stale one is corrected by the next scan — the folder wins.
func TestWorkspaceRescanCorrectsStaleEntry(t *testing.T) {
	root := t.TempDir()
	mkProject(t, root, "real", "m-11111111")
	// A manifest that names a project that does not exist, misreports the id
	// of one that does, and omits the real one entirely.
	if err := SaveWorkspace(root, Workspace{Projects: []WorkspaceProject{
		{Path: "ghost", ID: "m-99999999"},
		{Path: "real", ID: "m-wrong"},
	}}); err != nil {
		t.Fatal(err)
	}
	mkProject(t, root, "added-behind-its-back", "m-22222222")

	if err := RefreshWorkspace(root); err != nil {
		t.Fatalf("RefreshWorkspace: %v", err)
	}
	w, ok, err := LoadWorkspace(root)
	if err != nil || !ok {
		t.Fatalf("LoadWorkspace: %v (ok=%v)", err, ok)
	}
	got := map[string]string{}
	for _, p := range w.Projects {
		got[p.Path] = p.ID
	}
	want := map[string]string{"added-behind-its-back": "m-22222222", "real": "m-11111111"}
	if len(got) != len(want) {
		t.Fatalf("projects = %+v, want %+v", got, want)
	}
	for path, id := range want {
		if got[path] != id {
			t.Fatalf("entry %q = %q, want %q (the folder wins)", path, got[path], id)
		}
	}

	// A symlink to a project counts. os.ReadDir reports the LINK's type, so a
	// naive IsDir() check drops it — and the user's answer to "what on this
	// machine is BearDrive" would quietly omit a folder they can see.
	linked := t.TempDir()
	if _, err := SaveProject(linked, Project{ID: "m-44444444", Volume: "linked"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linked, filepath.Join(root, "by-symlink")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := RefreshWorkspace(root); err != nil {
		t.Fatal(err)
	}
	w, _, err = LoadWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range w.Projects {
		if p.Path == "by-symlink" && p.ID == "m-44444444" {
			found = true
		}
	}
	if !found {
		t.Fatalf("manifest = %+v, want the symlinked project indexed", w.Projects)
	}

	// Refreshing a folder that is not a root creates nothing: a manifest is
	// only ever written where one was deliberately designated.
	plain := t.TempDir()
	mkProject(t, plain, "wiki", "m-33333333")
	if err := RefreshWorkspace(plain); err != nil {
		t.Fatalf("RefreshWorkspace on a plain folder: %v", err)
	}
	if _, err := os.Stat(workspaceConfigPath(plain)); !os.IsNotExist(err) {
		t.Fatal("RefreshWorkspace wrote a manifest into a folder that is not a root")
	}
}

// TestWorkspaceRefusesNesting: a root is never a mount, roots do not nest,
// and a root never sits inside a project (its manifest would sync to the
// team).
func TestWorkspaceRefusesNesting(t *testing.T) {
	root := t.TempDir()
	if err := InitWorkspace(root); err != nil {
		t.Fatal(err)
	}

	inner := filepath.Join(root, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InitWorkspace(inner); err == nil || !strings.Contains(err.Error(), "nest") {
		t.Fatalf("root inside a root: %v, want a refusal naming nesting", err)
	}

	mount := mkProject(t, t.TempDir(), "wiki", "m-44444444")
	if err := InitWorkspace(mount); err == nil {
		t.Fatal("root at a project folder: want a refusal")
	}
	sub := filepath.Join(mount, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InitWorkspace(sub); err == nil {
		t.Fatal("root inside a project: want a refusal")
	}

	// Re-designating an existing root is a refresh, not an error.
	if err := InitWorkspace(root); err != nil {
		t.Fatalf("re-init of an existing root: %v", err)
	}
}

// TestWorkspaceManifestShape pins the on-disk JSON: the kind and the relative
// path index, and nothing that would let a reader resolve state from it.
func TestWorkspaceManifestShape(t *testing.T) {
	root := t.TempDir()
	mkProject(t, root, "team", "m-55555555")
	if err := InitWorkspace(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(workspaceConfigPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["kind"] != WorkspaceKind {
		t.Fatalf("kind = %v", raw["kind"])
	}
	for _, forbidden := range []string{"volume", "remote", "include", "post_sync", "id"} {
		if _, ok := raw[forbidden]; ok {
			t.Fatalf("manifest carries %q: a root holds an index, never project state", forbidden)
		}
	}
}

// TestWorkspaceRootIsNeverTheBdriveHome: a root's manifest lives in
// <folder>/.bdrive — and at $HOME that directory IS the beardrive home,
// holding this device's token, its identity and every project's journals.
// Designating $HOME as a root would write an index in there and make
// IsWorkspaceRoot($HOME) depend on a file inside $BDRIVE_HOME.
func TestWorkspaceRootIsNeverTheBdriveHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BDRIVE_HOME", filepath.Join(home, ProjectDir))

	err := InitWorkspace(home)
	if err == nil {
		t.Fatal("InitWorkspace at $HOME: want a refusal")
	}
	if !strings.Contains(err.Error(), "beardrive home") {
		t.Fatalf("error %q does not say why", err)
	}
	if _, serr := os.Stat(filepath.Join(home, ProjectDir, WorkspaceFile)); !os.IsNotExist(serr) {
		t.Fatal("a manifest was written into the beardrive home")
	}

	// A folder beside it is fine.
	ok := filepath.Join(home, "Projects")
	if err := os.MkdirAll(ok, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InitWorkspace(ok); err != nil {
		t.Fatalf("InitWorkspace in an ordinary folder: %v", err)
	}
}

// TestDesignateWorkspaceIsScanFree
//
// Designation runs inside the desktop connect flow, where a blocking call is a
// UI wedged forever with no way back and no undo. So it must read nothing but
// the manifest it is about to write — no directory listing, no per-child
// configs. A wedged child proves it: a scan would block here.
func TestDesignateWorkspaceIsScanFree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BDRIVE_HOME", filepath.Join(home, ProjectDir))
	root := filepath.Join(home, "Projects")
	if err := os.MkdirAll(filepath.Join(root, "team"), 0o755); err != nil {
		t.Fatal(err)
	}
	wedged := filepath.Join(root, "wedged")
	if err := os.MkdirAll(filepath.Join(wedged, ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(wedged, ProjectDir, "config.json"), 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	type result struct {
		created bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		c, err := DesignateWorkspace(root, WorkspaceProject{Path: "team", ID: "m-scanfree"})
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || !r.created {
			t.Fatalf("DesignateWorkspace = %v, %v", r.created, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DesignateWorkspace read a wedged child: the desktop connect would hang at connecting")
	}

	w, ok, err := LoadWorkspace(root)
	if err != nil || !ok || len(w.Projects) != 1 || w.Projects[0].Path != "team" {
		t.Fatalf("manifest = %+v (ok=%v, %v), want just the project it was given", w.Projects, ok, err)
	}
	if created, err := DesignateWorkspace(root, WorkspaceProject{Path: "other", ID: "m-second"}); created || err != nil {
		t.Fatalf("second DesignateWorkspace = %v, %v; want false, nil", created, err)
	}

	// Un-designation is synchronous, so a failed connect leaves nothing behind
	// and nothing can land after it.
	if err := UndesignateWorkspace(root); err != nil {
		t.Fatal(err)
	}
	if IsWorkspaceRoot(root) {
		t.Fatal("UndesignateWorkspace left the folder a root")
	}
	if err := UndesignateWorkspace(root); err != nil {
		t.Fatalf("UndesignateWorkspace must be idempotent: %v", err)
	}
}

// TestWorkspaceRootRefusalCoversTheWholeHome: the refusal is containment, not
// equality — a custom $BDRIVE_HOME may sit deeper than <folder>/.bdrive — and
// it holds for a home that does not exist yet, where resolving symlinks fails
// and an equality check would pass.
func TestWorkspaceRootRefusalCoversTheWholeHome(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", base)
	// A home NESTED inside what would be the manifest's directory, and one
	// that does not exist on disk — EvalSymlinks fails on a missing directory,
	// so a resolved comparison would quietly pass.
	t.Setenv("BDRIVE_HOME", filepath.Join(state, ProjectDir, "store"))

	for _, fn := range []struct {
		name string
		call func() error
	}{
		{"InitWorkspace", func() error { return InitWorkspace(state) }},
		{"DesignateWorkspace", func() error {
			_, err := DesignateWorkspace(state, WorkspaceProject{Path: "x", ID: "m-x"})
			return err
		}},
	} {
		err := fn.call()
		if err == nil {
			t.Fatalf("%s: a folder whose %s contains the beardrive home must be refused", fn.name, ProjectDir)
		}
		if !strings.Contains(err.Error(), "beardrive home") {
			t.Fatalf("%s: error %q does not say why", fn.name, err)
		}
		if _, serr := os.Stat(workspaceConfigPath(state)); !os.IsNotExist(serr) {
			t.Fatalf("%s wrote a manifest anyway", fn.name)
		}
	}

	// An ordinary folder is fine, even with that same non-existent home.
	ok := filepath.Join(base, "Projects")
	if err := os.MkdirAll(ok, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InitWorkspace(ok); err != nil {
		t.Fatalf("ordinary folder refused: %v", err)
	}

	// The same paths spelled in different case are the same directory on this
	// project's primary filesystems, and a refusal must not be dodged by
	// typing one of them.
	if !underPath("/Users/x/.bdrive", "/USERS/X/.BDRIVE/store") {
		t.Fatal("underPath is case-sensitive: a differently-cased home slips past the refusal")
	}
	if underPath("/Users/x/.bdrive", "/Users/y/.bdrive") {
		t.Fatal("underPath matched two genuinely different directories")
	}
}

// TestWorkspaceRootRefusalSeesThroughSymlinks: the folder and the home can
// each be spelled through a symlink — Home() returns $BDRIVE_HOME verbatim,
// whatever the launcher set, and the folder comes from a UI or a shell. Two
// aliases of one directory must not answer differently, in EITHER direction:
// resolving only the folder side left the hole open from the other.
func TestWorkspaceRootRefusalSeesThroughSymlinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("HOME", base)

	// (1) home spelled through the alias, folder spelled really.
	t.Setenv("BDRIVE_HOME", filepath.Join(alias, ProjectDir))
	if err := checkRootHere(real); err == nil {
		t.Fatal("an aliased home spelling slipped past the refusal")
	}
	if _, err := DesignateWorkspace(real, WorkspaceProject{Path: "x", ID: "m-alias01"}); err == nil {
		t.Fatal("DesignateWorkspace wrote a manifest into the beardrive home (aliased home)")
	}

	// (2) the other way round: home spelled really, folder through the alias.
	t.Setenv("BDRIVE_HOME", filepath.Join(real, ProjectDir))
	if err := checkRootHere(alias); err == nil {
		t.Fatal("an aliased folder spelling slipped past the refusal")
	}

	// And nothing about symlinks alone makes a folder unusable: an ordinary
	// folder reached through a symlinked parent is fine, which is the common
	// macOS shape (/tmp, a symlinked home).
	ok := filepath.Join(alias, "Projects")
	if err := os.MkdirAll(ok, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BDRIVE_HOME", filepath.Join(base, ProjectDir))
	if err := checkRootHere(ok); err != nil {
		t.Fatalf("an ordinary folder under a symlinked parent was refused: %v", err)
	}
}

// TestRefreshDoesNotResurrectADeletedManifest
//
// Deleting the manifest is how a user un-roots a folder. A refresh already in
// flight must not undo that: it checked IsWorkspaceRoot before scanning, and
// the scan can take a long time — long enough for the user to delete the file
// and for the write to put it back, silently.
//
// The FIFO makes the race deterministic instead of hoping for it: the scan
// blocks reading it until this test opens the other end, so the delete lands
// mid-scan for certain.
func TestRefreshDoesNotResurrectADeletedManifest(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BDRIVE_HOME", filepath.Join(t.TempDir(), ProjectDir))
	mkProject(t, root, "team", "m-refresh1")
	if err := InitWorkspace(root); err != nil {
		t.Fatal(err)
	}

	wedged := filepath.Join(root, "slow")
	if err := os.MkdirAll(filepath.Join(wedged, ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(wedged, ProjectDir, "config.json")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- RefreshWorkspace(root) }()

	// Wait for the scan to actually block on the FIFO rather than assuming it
	// has: opening the write end NON-blocking fails with ENXIO until a reader
	// is there, which is the signal. A plain sleep would be a guess, and
	// losing that guess makes the blocking open below hang until the package
	// timeout — a 90-minute stall instead of a failure.
	var f *os.File
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		f, err = os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the scan never opened the FIFO: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The scan is now blocked reading it. Un-root the folder, then let it go.
	if err := os.Remove(workspaceConfigPath(root)); err != nil {
		t.Fatal(err)
	}
	f.WriteString("{}")
	f.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RefreshWorkspace: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RefreshWorkspace never finished")
	}

	if IsWorkspaceRoot(root) {
		t.Fatal("a refresh in flight rewrote a manifest the user had deleted")
	}
}

// TestDesignateWorkspaceObeysTheRootRules: DesignateWorkspace was written as
// InitWorkspace minus the scan, and lost the placement rules with it. A folder
// that is already a project — a clone carrying .bdrive/config.json, picked as
// the connect root — became a mount AND a root, after which `bdrive init`
// there refuses forever with no CLI route back.
func TestDesignateWorkspaceObeysTheRootRules(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BDRIVE_HOME", filepath.Join(t.TempDir(), ProjectDir))
	entry := WorkspaceProject{Path: "team", ID: "m-rules001"}

	// Already a project.
	clone := mkProject(t, t.TempDir(), "cloned-repo", "m-clone001")
	if created, err := DesignateWorkspace(clone, entry); err == nil || created {
		t.Fatalf("DesignateWorkspace at a project folder = %v, %v; want a refusal", created, err)
	}
	if IsWorkspaceRoot(clone) {
		t.Fatal("a project folder became a workspace root")
	}

	// The ancestor rules — roots do not nest, a root is never inside a project
	// — are deliberately NOT applied here: answering them costs one read per
	// level of directories nobody named, and one wedged ancestor hangs the
	// desktop connect forever. InitWorkspace, which can afford to block, still
	// refuses both. The trade-off is stated in DESIGN.md.
	outer := t.TempDir()
	if err := InitWorkspace(outer); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(outer, "sub")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if created, err := DesignateWorkspace(inner, entry); err != nil || !created {
		t.Fatalf("DesignateWorkspace inside a root = %v, %v; the connect flow does not walk ancestors", created, err)
	}
	// The directory must EXIST for this to test the guard: InitWorkspace scans
	// before it saves, so a missing folder fails with ENOENT and the assertion
	// passes even with the nesting rule deleted.
	sub2 := filepath.Join(outer, "sub2")
	if err := os.MkdirAll(sub2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InitWorkspace(sub2); err == nil || !strings.Contains(err.Error(), "nest") {
		t.Fatalf("InitWorkspace inside a root = %v; want a refusal naming nesting", err)
	}

	// Inside a project: same split. What DesignateWorkspace keeps is the
	// folder-local rule, which is the one that strands a project.
	sub := filepath.Join(clone, "docs")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InitWorkspace(sub); err == nil || !strings.Contains(err.Error(), "inside the project") {
		t.Fatalf("InitWorkspace inside a project = %v; want a refusal naming the project", err)
	}

	// A plain folder still works.
	ok := t.TempDir()
	if created, err := DesignateWorkspace(ok, entry); err != nil || !created {
		t.Fatalf("DesignateWorkspace in a plain folder = %v, %v", created, err)
	}
}

// TestUndesignateKeepsADirectoryItDidNotCreate: un-designation removes the
// manifest it wrote, never a .bdrive the user already had — "only if now
// empty" is also true of somebody else's empty directory, and os.Remove
// unlinks a symlink whatever it points at.
func TestUndesignateKeepsADirectoryItDidNotCreate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BDRIVE_HOME", filepath.Join(t.TempDir(), ProjectDir))

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := DesignateWorkspace(root, WorkspaceProject{Path: "team", ID: "m-undes001"}); err != nil {
		t.Fatal(err)
	}
	if err := UndesignateWorkspace(root); err != nil {
		t.Fatal(err)
	}
	if IsWorkspaceRoot(root) {
		t.Fatal("the manifest survived un-designation")
	}
	if _, err := os.Stat(filepath.Join(root, ProjectDir)); err != nil {
		t.Fatalf("un-designation removed a %s directory it did not create: %v", ProjectDir, err)
	}
}

// TestDesignateWorkspaceNeverReadsAnAncestor
//
// The rules that need ancestors (roots do not nest; a root is never inside a
// project) cost one read per level, of directories nobody named. On the
// desktop connect that is a UI stuck at "connecting" forever — no cancel, no
// undo, 409 on every retry — so DesignateWorkspace applies only the rules it
// can answer from the folder itself.
//
// This is the fifth time an unbounded read reached a critical path in this
// feature. The test exists so it is the last.
func TestDesignateWorkspaceNeverReadsAnAncestor(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BDRIVE_HOME", filepath.Join(t.TempDir(), ProjectDir))

	// An ancestor whose manifest and whose project config both block forever.
	if err := os.MkdirAll(filepath.Join(base, ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{WorkspaceFile, "config.json"} {
		if err := syscall.Mkfifo(filepath.Join(base, ProjectDir, name), 0o644); err != nil {
			t.Skipf("cannot create a FIFO here: %v", err)
		}
	}
	root := filepath.Join(base, "deep", "Projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := DesignateWorkspace(root, WorkspaceProject{Path: "team", ID: "m-ancestor"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DesignateWorkspace: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DesignateWorkspace read an ancestor: the desktop connect would hang at connecting")
	}
	if !IsWorkspaceRoot(root) {
		t.Fatal("designation did not happen")
	}

	// InitWorkspace keeps the ancestor rules. Asserted on a directory that
	// EXISTS and on the refusal's wording: the folder here sits under both a
	// root (just designated) and, higher up, the FIFO'd .bdrive that IsMount
	// reads as a project — so a test that only checked "some error" would pass
	// on the wrong rule, which is exactly how the previous version of this
	// assertion passed with the nesting guard deleted.
	inside := filepath.Join(root, "inside")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InitWorkspace(inside); err == nil || !strings.Contains(err.Error(), "nest") {
		t.Fatalf("InitWorkspace inside a root = %v; want a refusal naming nesting", err)
	}
}

// TestDesignateWorkspaceNeverReadsTheManifestPath
//
// Eight rounds of review attacked scans of CHILDREN and walks over ANCESTORS
// and never looked at the one file this function opens first: its own
// manifest path. IsWorkspaceRoot is a ReadFile, not a Stat, so a FIFO — or a
// device node, or a file on a stalled network mount — at
// <root>/.bdrive/workspace.json wedged the desktop connect forever, with
// onboarding.running never clearing and every retry answered 409 for the life
// of the sidecar. Nothing earlier in the flow touches <root>/.bdrive, so this
// is the first thing to reach it.
//
// Existence is all this decision needs, and a stat cannot block.
func TestDesignateWorkspaceNeverReadsTheManifestPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BDRIVE_HOME", filepath.Join(t.TempDir(), ProjectDir))
	if err := os.MkdirAll(filepath.Join(root, ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(workspaceConfigPath(root), 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	type result struct {
		created bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		c, err := DesignateWorkspace(root, WorkspaceProject{Path: "team", ID: "m-fifo0001"})
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		// Something already occupies the manifest path, so this is a no-op —
		// never an overwrite of a file we cannot even read.
		if r.created {
			t.Fatal("DesignateWorkspace overwrote an unknown file at the manifest path")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DesignateWorkspace opened the manifest path: the desktop connect would hang at connecting forever")
	}

	// HasManifest is the non-blocking answer; IsWorkspaceRoot is the one that
	// reads, and is only for callers that can afford to.
	hasDone := make(chan bool, 1)
	go func() { hasDone <- HasManifest(root) }()
	select {
	case got := <-hasDone:
		if !got {
			t.Fatal("HasManifest missed a file at the manifest path")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HasManifest blocked: it must be a stat")
	}
}

// TestManifestReadIsBounded: the manifest indexes a folder's immediate
// children, so a huge file at that path is not one — and reading it is time a
// caller may not have (a 100 MB file cost seconds per call, and every command
// that asks "is this a root?" pays it).
func TestManifestReadIsBounded(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, maxManifestBytes*4)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(workspaceConfigPath(root), big, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := readManifest(workspaceConfigPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != maxManifestBytes {
		t.Fatalf("read %d bytes, want it capped at %d", len(data), maxManifestBytes)
	}
	// And an oversized body is simply not a manifest.
	if IsWorkspaceRoot(root) {
		t.Fatal("a multi-megabyte file read as a workspace manifest")
	}
}

// TestSaveWorkspaceDoesNotResurrectADeletedRoot: a refresh in flight when the
// user deletes the whole root must not rebuild the directory around its
// manifest. MkdirAll recreates the entire chain, which brought a removed
// folder back with a .bdrive inside it.
func TestSaveWorkspaceDoesNotResurrectADeletedRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "Projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveWorkspace(root, Workspace{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := SaveWorkspace(root, Workspace{}); err == nil {
		t.Fatal("SaveWorkspace recreated a root the user deleted")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("the deleted root came back")
	}
}
