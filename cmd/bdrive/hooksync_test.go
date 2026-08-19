package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/secrets"
	"github.com/runbear-io/beardrive/internal/store"
)

// `bdrive sync --hook` must emit the gated-link formula as Claude Code
// hook JSON, stamp the session note, and stay a silent no-op everywhere
// else — a hook must never fail the turn.
func TestSyncHookMode(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	proj, err := config.SaveProject(folder, config.Project{
		Volume: "wiki",
		Remote: "https://hub.example.com/p/p-12345678", // unreachable: cycle degrades offline, formula still valid
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil { // enroll, as `bdrive init` would
		t.Fatal(err)
	}

	c := syncCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetIn(strings.NewReader(`{"session_id":"sess-42","prompt":"hello"}`))
	c.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c.Execute(); err != nil {
		t.Fatalf("hook mode must never fail: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		`"hookSpecificOutput"`,
		`"hookEventName":"UserPromptSubmit"`,
		"https://hub.example.com/p-12345678", // base URL: remote minus /p
		"[🔗](",                               // the emoji-link convention
		"code blocks",                        // paths in code blocks stay plain
		"PUBLIC",                             // bdrive share stays opt-in
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook output missing %q:\n%s", want, got)
		}
	}

	// The session note was stamped for the daemon's follow-up scans.
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	if note := st.LoadNote(); note != "claude-code session sess-42" {
		t.Errorf("note = %q, want the stamped session", note)
	}
}

func TestSyncHookModeNoOps(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())

	// Not a mount: silent success, no output.
	c := syncCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetIn(strings.NewReader(`{"session_id":"x"}`))
	c.SetArgs([]string{t.TempDir(), "--hook", "claude-code"})
	if err := c.Execute(); err != nil {
		t.Fatalf("non-mount must be a silent no-op: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("non-mount emitted output: %s", out.String())
	}

	// A config.json that arrived with the folder (git clone, copied dir)
	// but was never enrolled on this device via `bdrive init`: silent no-op,
	// and — crucially — no device enrollment as a side effect.
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	proj, err := config.SaveProject(folder, config.Project{
		Volume: "wiki", Remote: "https://hub.example.com/p/p-12345678",
	})
	if err != nil {
		t.Fatal(err)
	}
	c2 := syncCmd()
	out.Reset()
	c2.SetOut(&out)
	c2.SetIn(strings.NewReader(`{"session_id":"x"}`))
	c2.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c2.Execute(); err != nil {
		t.Fatalf("unenrolled mount must be a silent no-op: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("unenrolled mount emitted output: %s", out.String())
	}
	mounts, err := config.LoadMounts()
	if err != nil {
		t.Fatal(err)
	}
	if _, enrolled := mounts[proj.ID]; enrolled {
		t.Fatal("hook auto-enrolled the mount; only `bdrive init` may do that")
	}

	// Enrolled but paused by `bdrive stop`: silent no-op too.
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPaused(vdir, true); err != nil {
		t.Fatal(err)
	}
	c3 := syncCmd()
	out.Reset()
	c3.SetOut(&out)
	c3.SetIn(strings.NewReader(`{"session_id":"x"}`))
	c3.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c3.Execute(); err != nil {
		t.Fatalf("paused mount must be a silent no-op: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("paused mount emitted output: %s", out.String())
	}

	// Garbage stdin on a live mount: still sync, still emit, never fail.
	if err := store.SetPaused(vdir, false); err != nil {
		t.Fatal(err)
	}
	c4 := syncCmd()
	out.Reset()
	c4.SetOut(&out)
	c4.SetIn(strings.NewReader("not json at all"))
	c4.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c4.Execute(); err != nil {
		t.Fatalf("garbage stdin must not fail: %v", err)
	}
	if !strings.Contains(out.String(), `"hookSpecificOutput"`) {
		t.Fatalf("formula not emitted on garbage stdin: %s", out.String())
	}
}

// mountAt creates a project folder under parent and enrolls it on this
// device, as `bdrive init` would.
func mountAt(t *testing.T, parent, name, remote string) config.Project {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	proj, err := config.SaveProject(dir, config.Project{Volume: name, Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(dir); err != nil {
		t.Fatal(err)
	}
	return proj
}

func runHook(t *testing.T, folder string) string {
	t.Helper()
	c := syncCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetIn(strings.NewReader(`{"session_id":"sess-42"}`))
	c.SetArgs([]string{folder, "--hook", "claude-code"})
	if err := c.Execute(); err != nil {
		t.Fatalf("hook mode must never fail: %v", err)
	}
	return out.String()
}

// A session at a root whose subfolders are separate projects must get EVERY
// project's URL, each keyed by the prefix the agent sees — emitting only the
// first mount's base made agents hang one project's paths on another
// project's URL.
func TestSyncHookModeMultipleMounts(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	a := mountAt(t, root, "projA", "https://hub.example.com/p/p-aaaaaaaa")
	b := mountAt(t, root, "projB", "https://hub.example.com/p/p-bbbbbbbb")

	got := runHook(t, root)

	// One JSON object: the hook's stdout contract.
	if n := strings.Count(strings.TrimSpace(got), "\n"); n != 0 {
		t.Fatalf("hook emitted %d objects, want 1:\n%s", n+1, got)
	}
	for _, want := range []string{
		"https://hub.example.com/p-aaaaaaaa",
		"https://hub.example.com/p-bbbbbbbb",
		"`projA/`",
		"`projB/`",
		"matches the path longest", // how to pick between them
		"do not link it",           // a path in neither is not synced
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook output missing %q:\n%s", want, got)
		}
	}

	// stdin is consumed once, so the note must still reach every mount.
	for _, proj := range []config.Project{a, b} {
		vdir, err := config.VolumeDir(proj.ID)
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(vdir)
		if err != nil {
			t.Fatal(err)
		}
		if note := st.LoadNote(); note != "claude-code session sess-42" {
			t.Errorf("%s: note = %q, want the stamped session", proj.Volume, note)
		}
	}
}

// A mount that has no hub URL must not swallow the context for the mounts
// that do — the old "first mount emits" guard could never detect a mount
// that emitted nothing, because hook mode never returns an error.
func TestSyncHookModeSkipsNonHubMount(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	mountAt(t, root, "a-plain", "file://"+t.TempDir()) // sorts first, no hub
	mountAt(t, root, "b-hub", "https://hub.example.com/p/p-bbbbbbbb")

	got := runHook(t, root)
	if !strings.Contains(got, "https://hub.example.com/p-bbbbbbbb") {
		t.Errorf("hub mount lost its link behind a non-hub mount:\n%s", got)
	}
	if !strings.Contains(got, "`b-hub/`") {
		t.Errorf("hub mount missing its prefix:\n%s", got)
	}
	if strings.Contains(got, "a-plain") {
		t.Errorf("non-hub mount has no URL and must not be listed:\n%s", got)
	}
}

// A session started inside a mount writes paths relative to its own
// directory, so that subpath belongs in the base URL — there is no prefix
// for the agent to strip.
func TestSyncHookModeInsideMount(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	mountAt(t, root, "wiki", "https://hub.example.com/p/p-12345678")
	sub := filepath.Join(root, "wiki", "docs", "notes")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	got := runHook(t, sub)
	if !strings.Contains(got, "https://hub.example.com/p-12345678/docs/notes") {
		t.Errorf("base URL missing the session's subpath (and `/` must stay literal):\n%s", got)
	}
}

// Plain `bdrive sync` (the push hook's form, and what users type) refuses
// unenrolled and paused mounts with instructions instead of silently
// enrolling or resuming.
func TestSyncRefusesUnenrolledAndPaused(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	proj, err := config.SaveProject(folder, config.Project{Volume: "wiki"})
	if err != nil {
		t.Fatal(err)
	}

	c := syncCmd()
	c.SetArgs([]string{folder})
	err = c.Execute()
	if err == nil || !strings.Contains(err.Error(), "bdrive init") {
		t.Fatalf("unenrolled sync error = %v, want a `bdrive init` pointer", err)
	}

	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPaused(vdir, true); err != nil {
		t.Fatal(err)
	}
	c2 := syncCmd()
	c2.SetArgs([]string{folder})
	err = c2.Execute()
	if err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("paused sync error = %v, want a paused message", err)
	}
}

// seedInbound pretends an earlier cycle — the daemon's, in the ordinary case
// — materialized these paths on this mount.
func seedInbound(t *testing.T, proj config.Project, paths ...string) {
	t.Helper()
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		deleted := strings.HasPrefix(p, "-")
		if err := st.LogInbound(strings.TrimPrefix(p, "-"), deleted); err != nil {
			t.Fatal(err)
		}
	}
}

// The whole point of the spool: a path materialized by an EARLIER cycle (the
// daemon's) is still reported by the hook, whose own cycle sees nothing. A
// Result field would report nothing here.
func TestSyncHookModeReportsInboundChanges(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	proj := mountAt(t, root, "wiki", "https://hub.example.com/p/p-12345678")

	seedInbound(t, proj, "notes/readme.md", "-old.md")

	got := runHook(t, filepath.Join(root, "wiki"))
	for _, want := range []string{
		"re-read before editing",
		"`notes/readme.md`",
		"`old.md (deleted)`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook output missing %q:\n%s", want, got)
		}
	}

	// The drain cleared: a second run with no peer activity says nothing.
	if again := runHook(t, filepath.Join(root, "wiki")); strings.Contains(again, "re-read before editing") {
		t.Errorf("second run repeated the changed list:\n%s", again)
	}
}

// Each path carries its own mount's prefix — never another mount's.
func TestSyncHookModeInboundMultipleMounts(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	a := mountAt(t, root, "projA", "https://hub.example.com/p/p-aaaaaaaa")
	b := mountAt(t, root, "projB", "https://hub.example.com/p/p-bbbbbbbb")
	seedInbound(t, a, "notes/a.md")
	seedInbound(t, b, "notes/b.md")

	got := runHook(t, root)
	for _, want := range []string{"`projA/notes/a.md`", "`projB/notes/b.md`"} {
		if !strings.Contains(got, want) {
			t.Errorf("hook output missing %q:\n%s", want, got)
		}
	}
}

// A session started inside a mount sees paths relative to its own directory:
// its subpath is stripped, and a sibling folder's file is not its to re-read.
func TestSyncHookModeInboundInsideMount(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	proj := mountAt(t, root, "wiki", "https://hub.example.com/p/p-12345678")
	sub := filepath.Join(root, "wiki", "docs", "notes")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	seedInbound(t, proj, "docs/notes/mine.md", "elsewhere/theirs.md")

	got := runHook(t, sub)
	if !strings.Contains(got, "`mine.md`") {
		t.Errorf("path under the session folder not stripped to what the agent sees:\n%s", got)
	}
	if strings.Contains(got, "theirs.md") {
		t.Errorf("path outside the session folder must not be listed:\n%s", got)
	}
}

// The first cycle on a fresh mount materializes everything; the turn must not
// carry the whole project.
func TestSyncHookModeInboundCap(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	proj := mountAt(t, root, "wiki", "https://hub.example.com/p/p-12345678")
	var paths []string
	for i := 0; i < hookChangedMax+5; i++ {
		paths = append(paths, fmt.Sprintf("f%02d.md", i))
	}
	seedInbound(t, proj, paths...)

	got := runHook(t, filepath.Join(root, "wiki"))
	if !strings.Contains(got, "+5 more") {
		t.Errorf("capped list missing its tail:\n%s", got)
	}
	if strings.Contains(got, "f24.md") {
		t.Errorf("list rendered past the cap:\n%s", got)
	}
}

// An unreadable spool leaves the turn intact: exit 0, valid JSON, links still
// emitted.
func TestSyncHookModeInboundSpoolUnreadable(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	proj := mountAt(t, root, "wiki", "https://hub.example.com/p/p-12345678")
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the spool should be: every read of it fails.
	if err := os.MkdirAll(filepath.Join(vdir, "inbound.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := runHook(t, filepath.Join(root, "wiki"))
	if !strings.Contains(got, `"hookSpecificOutput"`) || !strings.Contains(got, "https://hub.example.com/p-12345678") {
		t.Errorf("unreadable spool broke the turn's context:\n%s", got)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("hook emitted invalid JSON: %v\n%s", err, got)
	}
}

func seedSecrets(t *testing.T, proj config.Project, found map[string][]secrets.Finding) {
	t.Helper()
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSecrets(proj.ID, found); err != nil {
		t.Fatal(err)
	}
}

// The credential warning reaches the agent, whose own cycle found nothing:
// the daemon scanned that write seconds ago, so — exactly like the inbound
// spool — the record is what carries it. Advisory: the sentence says the file
// has ALREADY synced, because it has.
func TestSyncHookModeReportsSecrets(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	proj := mountAt(t, root, "wiki", "https://hub.example.com/p/p-12345678")
	seedSecrets(t, proj, map[string][]secrets.Finding{
		"deploy.md": {{Rule: "aws_access_key_id", Line: 12}},
	})

	got := runHook(t, filepath.Join(root, "wiki"))
	for _, want := range []string{
		"looked like they contain credentials when they last changed",
		"`deploy.md` (an AWS access key, line 12)",
		"tell the user",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook output missing %q:\n%s", want, got)
		}
	}
	// Unlike the inbound spool, findings are state and are NOT drained: the
	// file still holds the key on the next turn, so the next turn still says so.
	if again := runHook(t, filepath.Join(root, "wiki")); !strings.Contains(again, "`deploy.md`") {
		t.Errorf("the warning vanished after one turn:\n%s", again)
	}
}

// Every path carries its own mount's prefix — a credential in one project must
// never be reported as a path in another.
func TestSyncHookModeSecretsMultipleMounts(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	a := mountAt(t, root, "projA", "https://hub.example.com/p/p-aaaaaaaa")
	b := mountAt(t, root, "projB", "https://hub.example.com/p/p-bbbbbbbb")
	seedSecrets(t, a, map[string][]secrets.Finding{"a.md": {{Rule: "private_key", Line: 1}}})
	seedSecrets(t, b, map[string][]secrets.Finding{"b.md": {{Rule: "slack_token", Line: 2}}})

	got := runHook(t, root)
	for _, want := range []string{"`projA/a.md` (a private key, line 1)", "`projB/b.md` (a Slack token, line 2)"} {
		if !strings.Contains(got, want) {
			t.Errorf("hook output missing %q:\n%s", want, got)
		}
	}
}

// The empty case is the common one, paid on every turn of every session: the
// context must be byte-identical to what it was before this check existed.
func TestSyncHookModeNoSecretsSaysNothing(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	mountAt(t, root, "wiki", "https://hub.example.com/p/p-12345678")

	got := runHook(t, filepath.Join(root, "wiki"))
	for _, unwanted := range []string{"credential", "secret"} {
		if strings.Contains(strings.ToLower(got), unwanted) {
			t.Errorf("a clean mount mentions %q on every turn:\n%s", unwanted, got)
		}
	}
}
