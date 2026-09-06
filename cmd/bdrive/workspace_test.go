package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

// wsHome isolates $BDRIVE_HOME and $HOME so nothing here touches the real
// device identity, registry, or agent hook config.
func wsHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BDRIVE_HOME", filepath.Join(home, ".bdrive"))
	return home
}

// TestInitRefusesWorkspaceRoot: the root indexes projects and never syncs, so
// `bdrive init` at one must refuse — and refuse BEFORE any network call or
// file write, since the whole point of the refusal is that nothing at a root
// is ever mounted.
func TestInitRefusesWorkspaceRoot(t *testing.T) {
	wsHome(t)
	root := t.TempDir()
	if err := config.InitWorkspace(root); err != nil {
		t.Fatal(err)
	}
	// A folder the user keeps beside their projects: it must still be here,
	// unmounted, when init has refused.
	other := filepath.Join(root, "not-beardrive")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}

	// Bounded, because "before any network call" is half the assertion: with
	// the guard gone, init reaches ensureLogin and blocks on a browser login
	// that never comes. A hang is the failure, so it must not be an infinite
	// one.
	done := make(chan error, 1)
	go func() {
		_, err := seccliRun(t, initCmd(), []string{root, "--yes"})
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("bdrive init at a workspace root did not refuse: it reached the login flow")
	}
	if err == nil {
		t.Fatal("bdrive init at a workspace root: want a refusal")
	}
	if !strings.Contains(err.Error(), "workspace root") {
		t.Fatalf("error %q does not say what the folder is", err)
	}
	if config.IsMount(root) {
		t.Fatal("the refused init still wrote a project config at the root")
	}
	if _, err := os.Stat(filepath.Join(root, ".bdriveignore")); !os.IsNotExist(err) {
		t.Fatal("the refused init seeded .bdriveignore at the root")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("the refused init disturbed a folder beside the projects: %v", err)
	}
	// The root is still a root: a refusal changes nothing.
	if !config.IsWorkspaceRoot(root) {
		t.Fatal("the refused init damaged the manifest")
	}
}

// TestSyncStartNeverScansTheWorkspaceRoot
//
// startSync used to re-index the root here. It must not: the scan reads the
// root's directory and one config per child, neither bounded, so a single
// wedged sibling — a FIFO here, a TCC-gated or dead network path in life —
// hung `bdrive init` forever, and hung the desktop connect at "syncing" on
// every connect after the first. `daemon.Run` does the re-indexing, off the
// startup path and in its own goroutine.
//
// Deleting the call is the fix, so this asserts the absence: startSync gets on
// with its own work beside a sibling that can never be read.
func TestSyncStartNeverScansTheWorkspaceRoot(t *testing.T) {
	wsHome(t)
	root := t.TempDir()
	if err := config.InitWorkspace(root); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(root, "team")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	proj, err := config.SaveProject(mount, config.Project{Volume: "team", Remote: "file://" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	// A sibling whose config blocks any reader forever.
	decoy := filepath.Join(root, "wedged")
	if err := os.MkdirAll(filepath.Join(decoy, config.ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(decoy, config.ProjectDir, "config.json"), 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	done := make(chan struct{})
	go func() {
		// The daemon cannot fork from a test binary, so this returns an error;
		// returning AT ALL is the assertion.
		_ = startSync(t.Context(), mount, proj, false, 0, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("startSync hung on a wedged folder beside the project: it must not scan the workspace root")
	}
}

// TestLegacyProjectUnchanged: a project folder with no root above it is
// exactly today's layout. Nothing writes a manifest into its parent, and
// every path that learned about workspaces still answers as it did.
func TestLegacyProjectUnchanged(t *testing.T) {
	wsHome(t)
	parent := t.TempDir()
	mount := filepath.Join(parent, "wiki")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	proj, err := config.SaveProject(mount, config.Project{Volume: "wiki", Remote: "file://" + t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	_ = startSync(t.Context(), mount, proj, false, 0, 0)

	for _, p := range []string{
		filepath.Join(parent, config.ProjectDir, "workspace.json"),
		filepath.Join(parent, config.ProjectDir, "config.json"),
		filepath.Join(parent, config.ProjectDir),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s exists: syncing a project wrote state into its parent", p)
		}
	}
	if config.IsWorkspaceRoot(parent) {
		t.Fatal("the parent of a project became a workspace root on its own")
	}
	if !config.IsMount(mount) {
		t.Fatal("the project stopped reading as a mount")
	}
	got, ok, err := config.LoadProject(mount)
	if err != nil || !ok || got.ID != proj.ID || got.Volume != "wiki" {
		t.Fatalf("LoadProject = %+v (ok=%v, %v), want the unchanged project", got, ok, err)
	}
}

// TestCommandsAtARootDoNotAdviseInit: before workspace roots existed, every
// command's "not a beardrive project" error told the user to run `bdrive
// init` there. At a root that advice is now a dead end — init refuses on
// purpose — so the message has to say what the folder actually is.
func TestCommandsAtARootDoNotAdviseInit(t *testing.T) {
	wsHome(t)
	root := t.TempDir()
	if err := config.InitWorkspace(root); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		cmd  interface {
			SetArgs([]string)
			Execute() error
		}
		args []string
	}{
		{"log", logCmd(), []string{root}},
		{"grep", grepCmd(), []string{"anything", root}},
	} {
		_, err := seccliRun(t, tc.cmd, tc.args)
		if err == nil {
			t.Errorf("%s at a workspace root: want an error", tc.name)
			continue
		}
		if strings.Contains(err.Error(), "bdrive init") {
			t.Errorf("%s at a workspace root sends the user to `bdrive init`, which refuses there:\n  %v", tc.name, err)
		}
		if !strings.Contains(err.Error(), "workspace root") {
			t.Errorf("%s at a workspace root does not say what the folder is:\n  %v", tc.name, err)
		}
	}

	// A plain folder keeps the original advice, which is still right there.
	plain := t.TempDir()
	if _, err := seccliRun(t, logCmd(), []string{plain}); err == nil ||
		!strings.Contains(err.Error(), "bdrive init") {
		t.Fatalf("log in a plain folder should still advise init: %v", err)
	}
}

// TestInitRefusesAFolderContainingARoot: mounting a folder that CONTAINS a
// workspace root is the damaging direction. The nested project is excluded by
// the syncer's nested-mount handling, but the folders beside it — the ones the
// root exists to keep out of BearDrive — are ordinary content to the parent
// mount, and the next cycle pushes them to everyone.
func TestInitRefusesAFolderContainingARoot(t *testing.T) {
	wsHome(t)
	outer := t.TempDir()
	root := filepath.Join(outer, "Projects")
	mount := filepath.Join(root, "team")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SaveProject(mount, config.Project{ID: "m-contain01", Volume: "team"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(mount); err != nil {
		t.Fatal(err)
	}
	if err := config.InitWorkspace(root); err != nil {
		t.Fatal(err)
	}
	// The folder the user deliberately kept out of BearDrive.
	private := filepath.Join(root, "private-stuff")
	if err := os.MkdirAll(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "secret.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := seccliRun(t, initCmd(), []string{outer, "--yes"})
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("bdrive init above a workspace root did not refuse: it reached the login flow")
	}
	if err == nil {
		t.Fatal("bdrive init above a workspace root: want a refusal")
	}
	if !strings.Contains(err.Error(), "workspace root") || !strings.Contains(err.Error(), root) {
		t.Fatalf("error %q does not name the root it would have swept up", err)
	}
	if config.IsMount(outer) {
		t.Fatal("the refused init mounted the folder anyway")
	}

	// The root itself is still fine to work in, and so is the project.
	if !config.IsWorkspaceRoot(root) {
		t.Fatal("the refusal damaged the root")
	}
}

// TestShareFamilyAtARootDoesNotAdviseInit: `share`, `restore`, `forget` and
// `url` resolve their project by walking UP (findProject), a second
// not-a-project message that the first pass at this missed. At a root — or in
// a folder beside the projects — that walk reaches the top, and the advice
// must not be `bdrive init`, which refuses there.
func TestShareFamilyAtARootDoesNotAdviseInit(t *testing.T) {
	wsHome(t)
	root := t.TempDir()
	if err := config.InitWorkspace(root); err != nil {
		t.Fatal(err)
	}
	beside := filepath.Join(root, "not-beardrive")
	if err := os.MkdirAll(beside, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{root, beside} {
		_, _, err := findProject(dir)
		if err == nil {
			t.Fatalf("findProject(%s): want an error", dir)
		}
		if strings.Contains(err.Error(), "bdrive init") {
			t.Errorf("findProject(%s) sends the user to `bdrive init`, which refuses at a root:\n  %v", dir, err)
		}
		if !strings.Contains(err.Error(), "workspace root") {
			t.Errorf("findProject(%s) does not say what the folder is:\n  %v", dir, err)
		}
	}

	// Outside any root the original message is still right.
	if _, _, err := findProject(t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "bdrive init") {
		t.Fatalf("findProject in a plain folder should still advise init: %v", err)
	}
}

// TestInitInAProjectUnderARootStillWorks
//
// The shipping desktop layout is <root>/<project>, so `bdrive init` inside a
// project under a root is the single most-travelled path there is: it is how
// syncing resumes after `bdrive stop`, and how a moved folder self-heals.
//
// A guard meant to refuse a folder CONTAINING a root refused this instead —
// mountsUnder(folder) includes folder itself, so a project's own root looked
// like a root "underneath" it. `bdrive stop` says "run bdrive init to resume"
// and init then refused: a project stranded with no CLI route back.
func TestInitInAProjectUnderARootStillWorks(t *testing.T) {
	wsHome(t)
	root := t.TempDir()
	mount := filepath.Join(root, "team")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	proj, err := config.SaveProject(mount, config.Project{
		ID: "m-under001", Volume: "team", Remote: "file://" + t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(mount); err != nil {
		t.Fatal(err)
	}
	if err := config.InitWorkspace(root); err != nil {
		t.Fatal(err)
	}

	// The guard must see nothing to refuse here.
	if r, found := workspaceRootUnder(mount); found {
		t.Fatalf("workspaceRootUnder(%s) = %s: a project's OWN root is not a root beneath it", mount, r)
	}
	// Nor for a subfolder of the project.
	sub := filepath.Join(mount, "docs")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if r, found := workspaceRootUnder(sub); found {
		t.Fatalf("workspaceRootUnder(%s) = %s: nothing is beneath a subfolder", sub, r)
	}

	// And the real command resumes rather than refusing. It reaches startSync,
	// whose daemon cannot fork from a test binary — any error must therefore
	// be about the daemon, never about a workspace root.
	done := make(chan error, 1)
	go func() {
		_, err := seccliRun(t, initCmd(), []string{mount, "--yes"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil && strings.Contains(err.Error(), "workspace root") {
			t.Fatalf("bdrive init in a project under a root was refused:\n  %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("bdrive init in a project under a root hung")
	}
	if p, ok, err := config.LoadProject(mount); err != nil || !ok || p.ID != proj.ID {
		t.Fatalf("the project lost its identity: %+v (ok=%v, %v)", p, ok, err)
	}
}

// TestInitDoesNotSeeAnUnenrolledRoot pins the known gap: the registry cannot
// see a root whose projects this device never enrolled — a tree copied from
// another machine — so init above it is not refused.
func TestInitDoesNotSeeAnUnenrolledRoot(t *testing.T) {
	wsHome(t)
	outer := t.TempDir()
	root := filepath.Join(outer, "Projects")
	mount := filepath.Join(root, "team")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	// Written, never enrolled: nothing is in this device's registry.
	if _, err := config.SaveProject(mount, config.Project{ID: "m-copied01", Volume: "team"}); err != nil {
		t.Fatal(err)
	}
	if err := config.InitWorkspace(root); err != nil {
		t.Fatal(err)
	}
	mounts, err := config.LoadMounts()
	if err != nil || len(mounts) != 0 {
		t.Fatalf("registry = %v (%v), want empty for this test to mean anything", mounts, err)
	}

	// The gap, asserted so it stays a known one rather than a belief: the
	// guard is registry-only, so a root this device never enrolled is
	// invisible and init above it is NOT refused. Closing it means reading
	// folders the user did not name, which hung `bdrive init` when it was
	// tried (TestInitNeverScansForRoots). Stated in DESIGN.md.
	if r, found := workspaceRootUnder(outer); found {
		t.Fatalf("workspaceRootUnder(%s) = %s: the guard grew a scan — check it cannot hang", outer, r)
	}
}

// TestInitNeverScansForRoots
//
// The guard that refuses a folder CONTAINING a workspace root must not read
// folders the user did not name to find one. Scanning the children was tried,
// and it hung `bdrive init` forever on a single FIFO or wedged network child —
// the same hazard that had to be deleted from startSync, moved one function
// over onto the same command. A guard that can hang the command it guards is
// worse than the gap it closes.
func TestInitNeverScansForRoots(t *testing.T) {
	wsHome(t)
	folder := t.TempDir()

	// Children that block any reader forever, under both names that get read.
	for _, name := range []string{config.WorkspaceFile, "config.json"} {
		dir := filepath.Join(folder, "wedged-"+name)
		if err := os.MkdirAll(filepath.Join(dir, config.ProjectDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(dir, config.ProjectDir, name), 0o644); err != nil {
			t.Skipf("cannot create a FIFO here: %v", err)
		}
	}

	done := make(chan struct{})
	go func() {
		workspaceRootUnder(folder)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("workspaceRootUnder read a wedged child: `bdrive init` would hang forever")
	}
}

// TestInitFindsARootAboveADeepProject
//
// A project need not sit directly under its root: <root>/team/wiki is a mount
// whose root is two levels up. Checking only each mount's immediate parent
// missed that, so `bdrive init` above such a root was allowed — and the next
// cycle pushed the folders the root exists to hold apart to the whole team.
func TestInitFindsARootAboveADeepProject(t *testing.T) {
	wsHome(t)
	outer := t.TempDir()
	root := filepath.Join(outer, "Projects")

	// The project sits two levels below the root.
	deep := filepath.Join(root, "team", "wiki")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SaveProject(deep, config.Project{ID: "m-deep0001", Volume: "wiki"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(deep); err != nil {
		t.Fatal(err)
	}
	if err := config.InitWorkspace(root); err != nil {
		t.Fatal(err)
	}
	// The folder the root exists to keep out of BearDrive.
	if err := os.MkdirAll(filepath.Join(root, "private"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Compared resolved: the walk follows the registry's spelling of the mount
	// (which may be symlinked), so the root it reports is the same directory,
	// not necessarily the same string.
	if r, found := workspaceRootUnder(outer); !found || resolvePath(r) != resolvePath(root) {
		t.Fatalf("workspaceRootUnder(%s) = %q,%v; want the root at %s", outer, r, found, root)
	}
	// And the project's own init still works — the walk must stop at the
	// folder being examined, never treat a project's own root as one beneath.
	if r, found := workspaceRootUnder(deep); found {
		t.Fatalf("workspaceRootUnder(%s) = %s: a project's own root is not beneath it", deep, r)
	}
	if r, found := workspaceRootUnder(filepath.Join(root, "team")); found {
		t.Fatalf("workspaceRootUnder(%s) = %s: nothing is beneath an intermediate folder", filepath.Join(root, "team"), r)
	}
}

// TestWorkspaceRootUnderWalksTheResolvedMountPath
//
// The registry stores whatever spelling a mount was enrolled under, which may
// be a symlink with a SHORTER chain than the real path. Walking that spelling
// climbs a different tree than the folder being examined, so the "stop when we
// reach the folder" test never fires — and the walk continues past it into the
// folder's own ancestors, reporting a root ABOVE as one beneath.
//
// The user-visible result is `bdrive init` refusing a perfectly legitimate
// folder, naming a workspace root that the folder is actually INSIDE.
func TestWorkspaceRootUnderWalksTheResolvedMountPath(t *testing.T) {
	wsHome(t)
	base := t.TempDir()

	// A workspace root ABOVE the folder we will ask about.
	if err := config.InitWorkspace(base); err != nil {
		t.Fatal(err)
	}

	folder := filepath.Join(base, "a", "b")
	deep := filepath.Join(folder, "c")
	mount := filepath.Join(deep, "team")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SaveProject(mount, config.Project{ID: "m-symreg01", Volume: "team"}); err != nil {
		t.Fatal(err)
	}

	// Enrol it under a SHORTER spelling: <base>/short -> <base>/a/b/c.
	short := filepath.Join(base, "short")
	if err := os.Symlink(deep, short); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	mounts["m-symreg01"] = config.MountInfo{Path: filepath.Join(short, "team")}
	if err := config.SaveMounts(mounts); err != nil {
		t.Fatal(err)
	}

	// Nothing is beneath `folder`: the only root is above it.
	if r, found := workspaceRootUnder(folder); found {
		t.Fatalf("workspaceRootUnder(%s) = %s — a root ABOVE the folder, reported as beneath it; "+
			"`bdrive init` would refuse a legitimate folder", folder, r)
	}
}
