package syncer

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
)

// tree lists every file under folder, slash-separated and sorted, skipping the
// mount's own .bdrive directory.
func tree(t *testing.T, folder string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if config.ReservedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(folder, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestWorkspaceRootNeverScanned: a mount under a workspace root syncs itself
// and nothing else. The root holds folders BearDrive never touches — that
// separation is what a root is for — and its manifest is not a project file.
//
// The guard is structural, not a name rule: the root is simply never a sync
// folder, so "workspace.json" is NOT reserved and a file a user happens to
// give that name inside a project syncs like any other.
func TestWorkspaceRootNeverScanned(t *testing.T) {
	be := sharedRemote(t)

	root := t.TempDir()
	mount := filepath.Join(root, "team")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SaveProject(mount, config.Project{ID: "m-team1234", Volume: "team"}); err != nil {
		t.Fatal(err)
	}
	if err := config.InitWorkspace(root); err != nil {
		t.Fatal(err)
	}
	// Everything the user keeps beside the project.
	write(t, root, "non-beardrive-folder-1/secret.txt", "private")
	write(t, root, "notes.md", "mine")

	a := deviceAt(t, "deva", mount, be)
	write(t, mount, "doc.md", "v1")
	// A user file that happens to carry the manifest's name, at the top of a
	// project: nothing about workspaces may make it special.
	write(t, mount, "workspace.json", `{"some":"app config"}`)
	write(t, mount, "sub/nested.txt", "deep")
	cycle(t, a)

	b := newDevice(t, "devb", be)
	cycle(t, b)

	got := tree(t, b.Folder)
	want := []string{"doc.md", "sub/nested.txt", "workspace.json"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("peer received %v, want exactly %v", got, want)
	}
	if body := read(t, b.Folder, "workspace.json"); body != `{"some":"app config"}` {
		t.Fatalf("workspace.json content = %q, want the user's file", body)
	}

	// The root itself is untouched by the cycle: no journal, no volume, no
	// manifest rewrite driven by syncing.
	if config.IsMount(root) {
		t.Fatal("the workspace root became a mount")
	}
	w, ok, err := config.LoadWorkspace(root)
	if err != nil || !ok || len(w.Projects) != 1 || w.Projects[0].Path != "team" {
		t.Fatalf("root manifest = %+v (ok=%v, %v), want the one project", w.Projects, ok, err)
	}
	if _, err := os.Stat(filepath.Join(root, "non-beardrive-folder-1", "secret.txt")); err != nil {
		t.Fatalf("a folder beside the project was disturbed: %v", err)
	}

	// The manifest is kept out by structure, not by a reserved name — adding
	// one would silently stop a real user file from syncing.
	if config.ReservedDir("workspace") || config.ReservedName(config.WorkspaceFile) ||
		config.ReservedPath(config.WorkspaceFile) {
		t.Fatal("workspace.json was made a reserved name: a user's own file would stop syncing")
	}
	// Inside a mount, the manifest's directory is still reserved for the
	// reason it always was.
	if !config.ReservedPath(config.ProjectDir + "/" + config.WorkspaceFile) {
		t.Fatal(".bdrive/workspace.json is not reserved")
	}
}
