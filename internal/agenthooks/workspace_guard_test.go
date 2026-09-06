package agenthooks

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
)

// TestHookGuardSkipsWorkspaceRoot
//
// A workspace root sits directly above the user's project folders AND above
// folders that never sync — that separation is the whole point of a root. The
// guard's walk-up stops at the first `.bdrive/config.json` it finds, so if a
// root carried one, every non-BearDrive folder beside the projects would
// answer "this is a BearDrive project" and `bdrive` would be spawned on every
// tool call in it. That is the invariant CLAUDE.md states outright: the guard
// "must never spawn bdrive (or anything else) outside a BearDrive project".
//
// This is why the manifest is `.bdrive/workspace.json` and not a config.json
// carrying a kind (internal/config/workspace.go): telling the two apart in the
// walk-up means inspecting each ancestor's config. That is affordable — `read`
// is a shell builtin — but it would make correctness in the machine's hottest
// guard depend on knowing what a workspace is. A separate name makes the
// walk-up right without workspaces existing as far as it is concerned. Point
// SaveWorkspace at config.json and this test fails.
func TestHookGuardSkipsWorkspaceRoot(t *testing.T) {
	e := secaud4Setup(t)

	root := secaud4Mkdir(t, filepath.Join(e.root, "workspace"))
	if err := config.SaveWorkspace(root, config.Workspace{
		Projects: []config.WorkspaceProject{{Path: "team", ID: "m-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	mount := secaud4Mount(t, secaud4Mkdir(t, filepath.Join(root, "team")))
	e.setMounts(t, `{"m-1":{"path":"`+mount+`"}}`)

	// Control: the guard still fires inside a real project under the root —
	// a walk that climbs too far is as broken as one that stops too early.
	for _, h := range secaud4Hooks() {
		if !e.fires(t, mount, h.cmd) {
			t.Fatalf("control: the %s did not fire inside the project at %s", h.label, mount)
		}
		if !e.fires(t, secaud4Mkdir(t, filepath.Join(mount, "docs", "deep")), h.cmd) {
			t.Fatalf("control: the %s did not fire in a subfolder of the project", h.label)
		}
	}

	// The folders the root exists to hold apart: nothing syncs in them, so
	// nothing runs in them.
	for _, dir := range []string{
		secaud4Mkdir(t, filepath.Join(root, "non-beardrive-folder-1")),
		secaud4Mkdir(t, filepath.Join(root, "non-beardrive-folder-2", "src", "deep")),
	} {
		for _, h := range secaud4Hooks() {
			if e.fires(t, dir, h.cmd) {
				t.Errorf("the %s spawned bdrive in %s — a folder that never syncs; "+
					"the workspace root's manifest read as a mount", h.label, dir)
			}
		}
	}

	// At the root itself the walk finds no mount, and the registry half takes
	// over: a session opened here covers the projects below it, exactly as a
	// session opened above two mounts does today.
	for _, h := range secaud4Hooks() {
		if !e.fires(t, root, h.cmd) {
			t.Errorf("the %s did not fire at the workspace root, which has a mount below it", h.label)
		}
	}
}

// TestHookGuardStaysPureShell: the guard runs on every tool call of every
// session on the machine, so the walk must not spawn anything — the grep it
// gained is bounded to directories that already hold a .bdrive/config.json.
func TestHookGuardStaysPureShell(t *testing.T) {
	for _, cmd := range secaud4Hooks() {
		guard, _, ok := strings.Cut(cmd.cmd, "command -v bdrive")
		if !ok {
			t.Fatalf("%s: guard no longer ends with the bdrive lookup", cmd.label)
		}
		for _, spawn := range []string{"bdrive ", "jq", "python", "awk", "find "} {
			if strings.Contains(guard, spawn) {
				t.Errorf("%s: guard runs %q before deciding the folder is a mount", cmd.label, spawn)
			}
		}
		// One grep, of the registry — the budget CLAUDE.md states and
		// sec_hooks_test.go pins. A workspace root is stepped over by the
		// manifest's file name, not by reading anything.
		if n := strings.Count(guard, "grep"); n > 1 {
			t.Errorf("%s: guard greps %d times, want at most 1", cmd.label, n)
		}
	}
}
