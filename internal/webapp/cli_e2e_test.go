package webapp

// CLI onboarding e2e: builds the real bdrive binary and drives the exact
// commands an agent (or the INSTALL_FOR_AGENTS.md runbook) runs against an
// in-process hub — device-code login, init, re-init, hooks, stop. This is
// the deterministic half of onboarding testing; the conversational half
// (does the agent ask before mounting?) lives in the onboarding-e2e skill.
//
// The key regression this guards: `bdrive init` must register agent sync
// hooks itself — a separate `bdrive hooks install` gets blocked by agent
// permission classifiers, which is how teams end up without hooks.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

// cliEnv is a signed-in CLI against a throwaway hub: the real binary, an
// isolated HOME/BDRIVE_HOME, and a browser session for hub-side assertions.
type cliEnv struct {
	run     func(dir string, args ...string) (string, error)
	hub     *httptest.Server
	browser *http.Client
	home    string // the isolated HOME; hooks live under here now
	bin     string // the real binary, for tests that need a bdrive to run
}

func newCLIEnv(t *testing.T) cliEnv {
	t.Helper()
	return newCLIEnvOn(t, nil)
}

// newCLIEnvOn is newCLIEnv against an existing hub, so a test can put two
// DEVICES (separate BDRIVE_HOMEs, separate device identities) on one project.
// nil starts a throwaway hub, which is the single-device default.
func newCLIEnvOn(t *testing.T, hub *httptest.Server) cliEnv {
	t.Helper()
	return newCLIEnvBin(t, hub, "")
}

// newCLIEnvBin is newCLIEnvOn with an explicit binary — the delta-sync E2E
// rows drive a binary built from the pre-change commit against the same hub
// as a current one. Empty means build the current tree.
func newCLIEnvBin(t *testing.T, hub *httptest.Server, bin string) cliEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and execs the bdrive binary; skipped with -short")
	}
	if bin == "" {
		bin = filepath.Join(t.TempDir(), "bdrive")
		build := exec.Command("go", "build", "-o", bin, "github.com/runbear-io/beardrive/cmd/bdrive")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
	}

	if hub == nil {
		hub = startTestHub(t)
	}

	// Isolate the CLI completely: fresh BDRIVE_HOME and a fresh HOME, so
	// agent-platform detection can't see or touch the real ~/.codex etc.
	home := t.TempDir()
	env := append(envWithout("HOME", "BDRIVE_HOME"),
		"HOME="+home, "BDRIVE_HOME="+filepath.Join(home, ".bdrive"))
	run := func(dir string, args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// Sign in via the real device-code flow, approved over HTTP as the
	// runbook's "any signed-in browser" (a cookie session from /auth/login).
	login := exec.Command(bin, "login", "--device", hub.URL)
	login.Env = env
	logFile := filepath.Join(t.TempDir(), "login.log")
	f, err := os.Create(logFile)
	if err != nil {
		t.Fatal(err)
	}
	login.Stdout, login.Stderr = f, f
	if err := login.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { login.Process.Kill() })
	approve := waitForApprovalLink(t, logFile)
	browser := signedInBrowser(t, hub.URL)
	if _, err := browser.PostForm(approve, nil); err != nil {
		t.Fatal(err)
	}
	if err := login.Wait(); err != nil {
		out, _ := os.ReadFile(logFile)
		t.Fatalf("login --device: %v\n%s", err, out)
	}
	return cliEnv{run: run, hub: hub, browser: browser, home: home, bin: bin}
}

func TestCLIOnboardingE2E(t *testing.T) {
	e := newCLIEnv(t)
	run, hub, browser := e.run, e.hub, e.browser

	// --- Init in a folder where Claude Code is in use. Hooks must be
	// registered by init itself, before the first sync output.
	work := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(filepath.Join(work, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer run(work, "stop", work) // don't leak the daemon on failure
	out, err := run(work, "init", "--name", "cli-e2e", "--yes")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "claude") || !strings.Contains(out, "hooks registered") {
		t.Fatalf("init did not report registering claude hooks:\n%s", out)
	}
	settings, err := os.ReadFile(filepath.Join(e.home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("init did not write the user's .claude/settings.json: %v", err)
	}
	for _, want := range []string{"bdrive sync", "bdrive read-log"} {
		if !strings.Contains(string(settings), want) {
			t.Fatalf("user settings.json missing %q hook:\n%s", want, settings)
		}
	}
	// Nothing agent-shaped may be created inside the project: it would sync.
	assertNoProjectHookFiles(t, work)

	// init's star ask is for humans at a terminal only. `run` pipes stdout,
	// which is exactly the shape of a CI job or a script parsing the output —
	// asking there is the mistake that got postinstall ads banned from npm.
	// (Matched on the repo URL, not the word "star" — `autostart registered`
	// is a legitimate line that contains it.)
	if strings.Contains(out, "github.com/runbear-io/beardrive") {
		t.Fatalf("init asked for a GitHub star without a TTY:\n%s", out)
	}

	// A reboot kills the daemon, so init registers the login agent that
	// brings it back. It must land in the user's own LaunchAgents dir (this
	// test's isolated HOME) and point at `bdrive resume`, which covers every
	// mount rather than needing one registration per project.
	if runtime.GOOS == "darwin" {
		plist := filepath.Join(e.home, "Library", "LaunchAgents", "ai.beardrive.daemon.plist")
		body, err := os.ReadFile(plist)
		if err != nil {
			t.Fatalf("init did not register the login agent: %v", err)
		}
		if !strings.Contains(string(body), "<string>resume</string>") {
			t.Fatalf("login agent does not run `bdrive resume`:\n%s", body)
		}
		if out, err := run(work, "autostart"); err != nil || !strings.Contains(out, "registered") {
			t.Fatalf("autostart status: %v\n%s", err, out)
		}
	}

	// resume is idempotent against a live daemon — the login agent runs it on
	// a machine where nothing is stopped, and must not start a second one.
	out, err = run(work, "resume")
	if err != nil || !strings.Contains(out, "already running 1") {
		t.Fatalf("resume should have found the running daemon: %v\n%s", err, out)
	}

	// The hooks are the whole agent integration: init must not install a
	// skill file anywhere, and no `skill` subcommand may come back.
	for _, agent := range []string{"claude", "codex", "gemini", "hermes"} {
		p := filepath.Join(e.home, "."+agent, "skills", "beardrive", "SKILL.md")
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("init installed a skill at %s — the hooks are the integration now", p)
		}
	}
	if out, err := run(work, "skill"); err == nil {
		t.Fatalf("`bdrive skill` still exists:\n%s", out)
	}

	// The project must actually exist on the hub, created under the account.
	if projects := hubProjects(t, browser, hub.URL); !strings.Contains(projects, "cli-e2e") {
		t.Fatalf("hub project list missing cli-e2e: %s", projects)
	}

	// --- Re-running init resumes and converges hooks idempotently.
	out, err = run(work, "init", "--yes")
	if err != nil {
		t.Fatalf("re-init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "resuming") || !strings.Contains(out, "hooks already registered") {
		t.Fatalf("re-init should resume with hooks already registered:\n%s", out)
	}

	if out, err = run(work, "hooks"); err != nil || !strings.Contains(out, "hooks registered") {
		t.Fatalf("hooks status: %v\n%s", err, out)
	}

	if out, err = run(work, "stop", work); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	cfg1, err := os.ReadFile(filepath.Join(work, ".bdrive", "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	// --- A second mount on the same machine (the "add another folder" flow)
	// must create a separate project and leave the first mount untouched.
	// Also the opt-out: --no-hooks must leave the platform config alone.
	work2 := filepath.Join(t.TempDir(), "proj2")
	if err := os.MkdirAll(filepath.Join(work2, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer run(work2, "stop", work2)
	out, err = run(work2, "init", "--name", "cli-e2e-nohooks", "--yes", "--no-hooks", "--no-autostart")
	if err != nil {
		t.Fatalf("init --no-hooks: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(work2, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("--no-hooks still wrote .claude/settings.json (stat err: %v)", err)
	}
	if strings.Contains(out, "login:") {
		t.Fatalf("--no-autostart still touched the login agent:\n%s", out)
	}
	if out, err = run(work2, "stop", work2); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	cfg1b, err := os.ReadFile(filepath.Join(work, ".bdrive", "config.json"))
	if err != nil || string(cfg1) != string(cfg1b) {
		t.Fatalf("second init disturbed the first mount's config (err %v):\nbefore: %s\nafter:  %s", err, cfg1, cfg1b)
	}
	projects := hubProjects(t, browser, hub.URL)
	for _, want := range []string{"cli-e2e", "cli-e2e-nohooks"} {
		if !strings.Contains(projects, want) {
			t.Fatalf("hub missing project %q after second init: %s", want, projects)
		}
	}
}

// Two sibling folders under one parent, each synced to a DIFFERENT project —
// the shape a second project lands in on a machine that already syncs one.
// The mounts must stay fully independent: separate ids, separate content, and
// the first one's config untouched by the second's init.
func TestCLISiblingProjectMounts(t *testing.T) {
	e := newCLIEnv(t)
	run, hub, browser := e.run, e.hub, e.browser

	parent := t.TempDir()
	a, b := filepath.Join(parent, "a"), filepath.Join(parent, "b")
	for dir, file := range map[string]string{a: "brand.md", b: "adr.md"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte("# "+file+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if out, err := run(a, "init", "--name", "project-a", "--yes"); err != nil {
		t.Fatalf("init a: %v\n%s", err, out)
	}
	defer run(a, "stop", a)
	cfgA, err := os.ReadFile(filepath.Join(a, ".bdrive", "config.json"))
	if err != nil {
		t.Fatal(err)
	}

	if out, err := run(b, "init", "--name", "project-b", "--yes"); err != nil {
		t.Fatalf("init b: %v\n%s", err, out)
	}
	defer run(b, "stop", b)

	// The second init must not have touched the first mount.
	cfgAafter, err := os.ReadFile(filepath.Join(a, ".bdrive", "config.json"))
	if err != nil || string(cfgA) != string(cfgAafter) {
		t.Fatalf("init in b/ changed a/'s config (err %v):\nbefore: %s\nafter:  %s", err, cfgA, cfgAafter)
	}
	cfgB, err := os.ReadFile(filepath.Join(b, ".bdrive", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfgA) == string(cfgB) {
		t.Fatalf("sibling mounts share a config: %s", cfgA)
	}

	// Each project holds its own file and not the other's.
	idA, idB := projectIDByName(t, browser, hub.URL, "project-a"), projectIDByName(t, browser, hub.URL, "project-b")
	pathsA, pathsB := hubPaths(t, browser, hub.URL, idA), hubPaths(t, browser, hub.URL, idB)
	if !pathsA["brand.md"] || pathsA["adr.md"] {
		t.Fatalf("project-a content wrong: %v", pathsA)
	}
	if !pathsB["adr.md"] || pathsB["brand.md"] {
		t.Fatalf("project-b content wrong: %v", pathsB)
	}
}

// Connecting a second device to an existing project, over a real hub, with the
// files that actually collide in practice: the `.bdriveignore` init seeds and an
// AGENTS.md an agent wrote in the folder before it was ever synced. Neither may
// fork into a `.bdriveignore.bdrive-conflict-<device>-<time>` file, and the
// joiner's copy may not replace the team's. The superseded content stays
// reachable through `bdrive restore --list`.
func TestCLIConnectAdoptsProjectVersions(t *testing.T) {
	hub := startTestHub(t)
	first := newCLIEnvOn(t, hub)

	const teamAgents = "# Team instructions\nask before editing docs/\n"
	owner := filepath.Join(t.TempDir(), "team")
	if err := os.MkdirAll(owner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owner, "AGENTS.md"), []byte(teamAgents), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := first.run(owner, "init", "--name", "join-e2e", "--yes"); err != nil {
		t.Fatalf("init owner: %v\n%s", err, out)
	}
	defer first.run(owner, "stop", owner)

	// The team's ignore rules: the seeded file plus a line of their own, so the
	// joiner's freshly seeded copy really does differ.
	teamIgnore, err := os.ReadFile(filepath.Join(owner, ".bdriveignore"))
	if err != nil {
		t.Fatal(err)
	}
	teamIgnore = append(teamIgnore, []byte("\n# team rules\nscratch/\n")...)
	if err := os.WriteFile(filepath.Join(owner, ".bdriveignore"), teamIgnore, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := first.run(owner, "sync"); err != nil {
		t.Fatalf("owner sync: %v\n%s", err, out)
	}

	// A second device, its own HOME and device identity, joining the same
	// project from a folder that already holds its own AGENTS.md.
	second := newCLIEnvOn(t, hub)
	joiner := filepath.Join(t.TempDir(), "joined")
	if err := os.MkdirAll(joiner, 0o755); err != nil {
		t.Fatal(err)
	}
	const localAgents = "notes my agent wrote in this folder\n"
	if err := os.WriteFile(filepath.Join(joiner, "AGENTS.md"), []byte(localAgents), 0o644); err != nil {
		t.Fatal(err)
	}
	id := projectIDByName(t, first.browser, hub.URL, "join-e2e")
	out, err := second.run(joiner, "init", "--project", id, "--yes")
	if err != nil {
		t.Fatalf("init joiner: %v\n%s", err, out)
	}
	defer second.run(joiner, "stop", joiner)
	if !strings.Contains(out, "adopted:") {
		t.Fatalf("init did not report adopting the project's versions:\n%s", out)
	}

	// Let both devices settle, then look at every folder involved.
	if out, err := second.run(joiner, "sync"); err != nil {
		t.Fatalf("joiner sync: %v\n%s", err, out)
	}
	if out, err := first.run(owner, "sync"); err != nil {
		t.Fatalf("owner sync back: %v\n%s", err, out)
	}
	for name, folder := range map[string]string{"joiner": joiner, "owner": owner} {
		if c := conflictCopiesUnder(t, folder); len(c) != 0 {
			t.Errorf("%s folder has conflict copies after a plain connect: %v", name, c)
		}
		if got := readFile(t, filepath.Join(folder, "AGENTS.md")); got != teamAgents {
			t.Errorf("%s AGENTS.md = %q, want the team's version", name, got)
		}
		if got := readFile(t, filepath.Join(folder, ".bdriveignore")); got != string(teamIgnore) {
			t.Errorf("%s .bdriveignore = %q, want the team's version", name, got)
		}
	}

	// Nothing was dropped: the joiner's own version is a superseded version of
	// that path, which is exactly what `bdrive restore --list` reads.
	versions, err := second.run(joiner, "restore", "AGENTS.md", "--list")
	if err != nil {
		t.Fatalf("restore --list: %v\n%s", err, versions)
	}
	var older string // the one version that is not the current content
	for _, line := range strings.Split(versions, "\n") {
		if m := restoreIDRe.FindStringSubmatch(line); m != nil && !strings.HasPrefix(strings.TrimSpace(line), "*") {
			older = m[1]
		}
	}
	if older == "" {
		t.Fatalf("the joiner's pre-join AGENTS.md is not in history:\n%s", versions)
	}
	if out, err := second.run(joiner, "restore", "AGENTS.md", older); err != nil {
		t.Fatalf("restore %s: %v\n%s", older, err, out)
	}
	if got := readFile(t, filepath.Join(joiner, "AGENTS.md")); got != localAgents {
		t.Errorf("restored AGENTS.md = %q, want the joiner's pre-join content", got)
	}
}

var restoreIDRe = regexp.MustCompile(`\b([0-9a-f]{8})\b`)

func conflictCopiesUnder(t *testing.T, folder string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(folder, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(d.Name(), ".bdrive-conflict-") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// One project mounted from two folders on the SAME device. The remote journal
// key is per-device, so a second mount would restart the sequence and
// overwrite the first mount's ops — its files would vanish from the hub. Init
// must refuse; if it ever accepts again, the first mount's file must survive.
func TestCLISameProjectTwoMounts(t *testing.T) {
	e := newCLIEnv(t)
	run, hub, browser := e.run, e.hub, e.browser

	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "adr.md"), []byte("# ADR\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(first, "init", "--name", "dup", "--yes"); err != nil {
		t.Fatalf("init first: %v\n%s", err, out)
	}
	defer run(first, "stop", first)
	id := projectIDByName(t, browser, hub.URL, "dup")

	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := run(second, "init", "--project", id, "--yes")
	defer run(second, "stop", second)
	if err != nil {
		// The refusal must name the folder already holding the project.
		if !strings.Contains(out, first) {
			t.Fatalf("refusal should point at the existing mount %s:\n%s", first, out)
		}
		if config := filepath.Join(second, ".bdrive", "config.json"); fileExists(config) {
			t.Fatalf("refused init still wrote %s", config)
		}
		return
	}
	if paths := hubPaths(t, browser, hub.URL, id); !paths["adr.md"] {
		t.Fatalf("second mount erased the first mount's file from the hub: %v\n%s", paths, out)
	}
}

// A mount below the session's directory (a repo root with wiki/ mounted) and
// one above it (a session inside the synced folder) must both sync: agent
// hooks run wherever the editor was opened, which is rarely the mount root.
func TestCLISyncResolvesMountFromAnyDirectory(t *testing.T) {
	e := newCLIEnv(t)
	run := e.run

	repo := t.TempDir()
	wiki := filepath.Join(repo, "wiki")
	deep := filepath.Join(wiki, "notes")
	for _, dir := range []string{deep, filepath.Join(repo, ".claude"), filepath.Join(wiki, ".claude")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(deep, "a.md"), []byte("# A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Run init the way an agent does: from the repo root, naming the subfolder.
	if out, err := run(repo, "init", "wiki", "--name", "scoped", "--yes"); err != nil {
		t.Fatalf("init wiki: %v\n%s", err, out)
	}
	defer run(wiki, "stop", wiki)

	// From the repo root: the mount is below, found through the registry.
	out, err := run(repo, "sync")
	if err != nil || !strings.Contains(out, wiki) {
		t.Fatalf("sync at the repo root missed the wiki mount: %v\n%s", err, out)
	}
	// From inside the mount: walk up to its root.
	out, err = run(deep, "sync")
	if err != nil || !strings.Contains(out, wiki) {
		t.Fatalf("sync inside the mount did not resolve its root: %v\n%s", err, out)
	}
	// Hooks are user-level; the repo and the mount stay clean.
	for _, dir := range []string{repo, wiki} {
		assertNoProjectHookFiles(t, dir)
	}

	// Reaching the repo through a symlink must find the same mount: macOS
	// hands sessions /tmp paths for mounts registered under /private/tmp.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if out, err := run(link, "sync"); err != nil || !strings.Contains(out, "project") {
		t.Fatalf("sync through a symlinked repo root missed the mount: %v\n%s", err, out)
	}
}

// Wherever init runs — inside the mount, at a repo root, anywhere — the hooks
// go to the user's config and the project tree is left clean. Platforms read
// hook config only from the directory a session starts in, so one user-level
// registration is what actually covers every session.
func TestCLIInitWritesNoProjectHookFiles(t *testing.T) {
	e := newCLIEnv(t)
	run := e.run

	repo := t.TempDir()
	wiki := filepath.Join(repo, "wiki")
	for _, dir := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, ".claude"), filepath.Join(wiki, ".claude")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Pre-existing project hooks from an older version, plus a foreign hook.
	stale := filepath.Join(repo, ".claude", "settings.json")
	old := `{"hooks":{"UserPromptSubmit":[` +
		`{"hooks":[{"type":"command","command":"sh -c 'bdrive sync .'"}]},` +
		`{"hooks":[{"type":"command","command":"echo mine"}]}]}}`
	if err := os.WriteFile(stale, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := run(wiki, "init", "--name", "in-repo", "--yes")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	defer run(wiki, "stop", wiki)

	if !fileExists(filepath.Join(e.home, ".claude", "settings.json")) {
		t.Fatalf("hooks not registered in the user config:\n%s", out)
	}
	assertNoProjectHookFiles(t, wiki)
	// The stale project config keeps the foreign hook but loses ours.
	data, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "bdrive sync") {
		t.Fatalf("stale project hooks survived: %s", data)
	}
	if !strings.Contains(string(data), "echo mine") {
		t.Fatalf("migration removed a hook that was not ours: %s", data)
	}
	if !strings.Contains(out, "moved out of") {
		t.Fatalf("init did not report the migration:\n%s", out)
	}
}

// assertNoProjectHookFiles fails if BearDrive left agent config in a project.
func assertNoProjectHookFiles(t *testing.T, dir string) {
	t.Helper()
	for _, rel := range []string{
		filepath.Join(".claude", "settings.json"),
		filepath.Join(".codex", "hooks.json"),
		filepath.Join(".gemini", "settings.json"),
	} {
		if fileExists(filepath.Join(dir, rel)) {
			t.Fatalf("%s exists in the project — hooks must be user-level only", filepath.Join(dir, rel))
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// hubPaths is the set of paths a project's history knows about.
func hubPaths(t *testing.T, browser *http.Client, hubURL, projectID string) map[string]bool {
	t.Helper()
	resp, err := browser.Get(hubURL + "/api/p/" + projectID + "/history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Entries []struct{ Path string } `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, e := range body.Entries {
		paths[e.Path] = true
	}
	return paths
}

// projectID looks a project up by name through the hub's own API.
func projectIDByName(t *testing.T, browser *http.Client, hubURL, name string) string {
	t.Helper()
	var body struct {
		Projects []struct{ ID, Name string } `json:"projects"`
	}
	if err := json.Unmarshal([]byte(hubProjects(t, browser, hubURL)), &body); err != nil {
		t.Fatal(err)
	}
	for _, p := range body.Projects {
		if p.Name == name {
			return p.ID
		}
	}
	t.Fatalf("no project named %q on the hub", name)
	return ""
}

// startTestHub serves a minimal hub (auth + orgs + projects + store proxy)
// on an ephemeral port, mirroring TestE2EServe's wiring without the seeds.
func startTestHub(t *testing.T) *httptest.Server {
	t.Helper()
	state := t.TempDir()
	be, err := remote.Open(t.Context(), "file://"+filepath.Join(state, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenProjectDB(filepath.Join(state, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Root: be, Projects: db, Device: webDevice, Upload: UploadConfig{Enabled: true}}
	srv.Devices, _ = OpenDeviceRegistry(filepath.Join(state, "devices.json"))
	srv.Shares, err = OpenShareDB(filepath.Join(state, "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := OpenBuiltinAuth(filepath.Join(state, "auth.json"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.signup(e2eAdmin, "E2E Admin", e2ePassword); err != nil {
		t.Fatal(err)
	}
	srv.Auth = auth
	orgs, err := OpenOrgDB(filepath.Join(state, "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orgs.Create("default", e2eAdmin); err != nil {
		t.Fatal(err)
	}
	srv.Dir = LocalDirectory{OrgDB: orgs}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// signedInBrowser returns an http client holding a hub session cookie for
// the admin account — the "any signed-in browser" of the device flow.
func signedInBrowser(t *testing.T, hubURL string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	resp, err := c.PostForm(hubURL+"/auth/login", url.Values{
		"email": {e2eAdmin}, "password": {e2ePassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(jar.Cookies(mustParse(t, hubURL))) == 0 {
		t.Fatal("browser sign-in left no session cookie")
	}
	return c
}

func hubProjects(t *testing.T, browser *http.Client, hubURL string) string {
	t.Helper()
	resp, err := browser.Get(hubURL + "/api/projects")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var approveRe = regexp.MustCompile(`(https?://\S+/auth/device/[a-f0-9]+)`)

// waitForCode polls the login command's output for the device code it prints.
func waitForApprovalLink(t *testing.T, logFile string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logFile)
		if m := approveRe.FindSubmatch(data); m != nil {
			return string(m[1])
		}
		time.Sleep(100 * time.Millisecond)
	}
	data, _ := os.ReadFile(logFile)
	t.Fatalf("login --device never printed an approval link:\n%s", data)
	return ""
}

func envWithout(names ...string) []string {
	var out []string
Env:
	for _, kv := range os.Environ() {
		for _, n := range names {
			if strings.HasPrefix(kv, n+"=") {
				continue Env
			}
		}
		out = append(out, kv)
	}
	return out
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// `bdrive init --template` is the CLI-first path to a structured project: the
// hub seeds it at creation (the CLI creates through the same endpoint the
// browser does), init's blocking first cycle pulls it, and re-running the
// same command is a no-op rather than a second copy.
func TestCLITemplateSeeding(t *testing.T) {
	e := newCLIEnv(t)
	run, hub, browser := e.run, e.hub, e.browser

	work := filepath.Join(t.TempDir(), "brain")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	defer run(work, "stop", work)
	out, err := run(work, "init", "--name", "seeded", "--template", "docs", "--yes")
	if err != nil {
		t.Fatalf("init --template: %v\n%s", err, out)
	}
	if !strings.Contains(out, "docs template") {
		t.Fatalf("init said nothing about the template:\n%s", out)
	}

	want := []string{"AGENTS.md", filepath.Join("decisions", "0001-record-decisions.md"), filepath.Join("docs", "README.md")}
	for _, rel := range want {
		if !fileExists(filepath.Join(work, rel)) {
			t.Fatalf("%s is not on disk after init --template docs:\n%s", rel, out)
		}
	}
	id := projectIDByName(t, browser, hub.URL, "seeded")
	paths := hubPaths(t, browser, hub.URL, id)
	for _, rel := range []string{"AGENTS.md", "decisions/0001-record-decisions.md", "docs/README.md"} {
		if !paths[rel] {
			t.Fatalf("%s never reached the hub: %v", rel, paths)
		}
	}
	// Every directory of the template has a file in it — an empty directory
	// would never sync, so the structure would silently not exist for a
	// teammate.
	if !paths["docs/README.md"] || !paths["decisions/0001-record-decisions.md"] {
		t.Fatalf("a template directory reached the hub empty: %v", paths)
	}

	// Re-running is safe: the runbook promises it, and agents pass --yes.
	before, err := os.ReadFile(filepath.Join(work, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := run(work, "init", "--template", "docs", "--yes"); err != nil {
		t.Fatalf("re-init --template: %v\n%s", err, out)
	}
	after, err := os.ReadFile(filepath.Join(work, "AGENTS.md"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("re-running init --template rewrote AGENTS.md (err %v)", err)
	}
}

// The two refusals that must cost nothing: a scope that would hide the
// template from the whole team, and a name that does not exist.
func TestCLITemplateRefusals(t *testing.T) {
	e := newCLIEnv(t)
	run := e.run

	scoped := filepath.Join(t.TempDir(), "scoped")
	if err := os.MkdirAll(scoped, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := run(scoped, "init", "--name", "scoped", "--template", "para", "--only", "docs", "--yes")
	if err == nil {
		t.Fatalf("--template with --only should be refused:\n%s", out)
	}
	if !strings.Contains(out, ".bdriveignore") {
		t.Fatalf("the refusal should say why (scope lives in .bdriveignore):\n%s", out)
	}
	if fileExists(filepath.Join(scoped, ".bdrive", "config.json")) {
		t.Fatal("a refused init still initialized the folder")
	}
	if fileExists(filepath.Join(scoped, "AGENTS.md")) {
		t.Fatal("a refused init still seeded files")
	}

	// Joining a project that already exists never restructures it: the
	// refusal has to name what it was actually created from.
	first := filepath.Join(t.TempDir(), "first")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := run(first, "init", "--name", "taken", "--template", "docs", "--yes"); err != nil {
		t.Fatalf("init first: %v\n%s", err, out)
	}
	defer run(first, "stop", first)
	second := filepath.Join(t.TempDir(), "second")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err = run(second, "init", "--name", "taken", "--template", "para", "--yes")
	defer run(second, "stop", second)
	if err == nil {
		t.Fatalf("a template on an existing project should be refused:\n%s", out)
	}
	if !strings.Contains(out, "docs") {
		t.Fatalf("the refusal should name the project's existing template:\n%s", out)
	}
	if fileExists(filepath.Join(second, "projects", "README.md")) {
		t.Fatal("a refused init still wrote the para skeleton")
	}

	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err = run(bad, "init", "--name", "bad", "--template", "karpathy-wiki", "--yes")
	if err == nil {
		t.Fatalf("an unknown template should be refused:\n%s", out)
	}
	for _, name := range []string{"docs", "para"} {
		if !strings.Contains(out, name) {
			t.Fatalf("the refusal should name the valid set (%s missing):\n%s", name, out)
		}
	}
	if fileExists(filepath.Join(bad, ".bdrive", "config.json")) {
		t.Fatal("a refused init still initialized the folder")
	}
}

// The same six rules, on the path every file takes. `bdrive status` names a
// synced file that looked like it held a credential when it last changed —
// and the file synced anyway, which is the posture: warn, never block.
func TestCLISecretsWarnOnSync(t *testing.T) {
	e := newCLIEnv(t)
	run := e.run

	work := t.TempDir()
	// Fabricated, AWS-shaped. Not a credential.
	const plantedKey = "AKIAIOSFODNN7EXAMPLE"
	if err := os.WriteFile(filepath.Join(work, "clean.md"), []byte("# Clean\n\nnothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(work, "init", "--name", "secret-warn", "--yes"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	defer run(work, "stop", work)

	// Nothing planted yet: the block is absent entirely, not empty.
	out, err := run(work, "status", work)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if strings.Contains(out, "secrets:") {
		t.Fatalf("status names credentials with none found:\n%s", out)
	}

	if err := os.WriteFile(filepath.Join(work, "deploy.md"), []byte(
		"# Deploy\n\nexport AWS_ACCESS_KEY_ID="+plantedKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(work, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	out, err = run(work, "status", work)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	for _, want := range []string{"secrets:", "deploy.md:3", "an AWS access key", "when they last changed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, plantedKey) {
		t.Fatalf("status echoed the key back:\n%s", out)
	}

	// It synced anyway — warn, never hold. The hub has the file.
	if out, err := run(work, "share", "deploy.md", "--force"); err != nil || !strings.Contains(out, "/s/") {
		t.Fatalf("the flagged file did not reach the hub: %v\n%s", err, out)
	}

	// Fixing the file is the whole remedy: no command, no flag.
	if err := os.WriteFile(filepath.Join(work, "deploy.md"), []byte(
		"# Deploy\n\nread AWS_ACCESS_KEY_ID from the environment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(work, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	out, err = run(work, "status", work)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if strings.Contains(out, "secrets:") {
		t.Fatalf("the warning outlived the credential:\n%s", out)
	}
}

// `bdrive share` refuses a file that looks like it holds credentials, and
// --force is the way past it. This is the flow BEA-111 exists for: the CLI
// used to print the URL and nothing else.
func TestCLIShareSecretGate(t *testing.T) {
	e := newCLIEnv(t)
	run := e.run

	work := t.TempDir()
	// Fabricated, AWS-shaped. Not a credential.
	const plantedKey = "AKIAIOSFODNN7EXAMPLE"
	if err := os.WriteFile(filepath.Join(work, "deploy.md"), []byte(
		"# Deploy\n\nexport AWS_ACCESS_KEY_ID="+plantedKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "clean.md"), []byte("# Clean\n\nnothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(work, "init", "--name", "share-gate", "--yes"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	defer run(work, "stop", work)
	if out, err := run(work, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}

	// A clean file shares as it always did.
	out, err := run(work, "share", "clean.md")
	if err != nil || !strings.Contains(out, "/s/") {
		t.Fatalf("clean file did not share: %v\n%s", err, out)
	}

	// The planted file is refused, by rule and line, and names the way out.
	out, err = run(work, "share", "deploy.md")
	if err == nil {
		t.Fatalf("share of a file holding a key succeeded:\n%s", out)
	}
	for _, want := range []string{"aws_access_key_id", "line 3", "--force", "at the moment you shared it"} {
		if !strings.Contains(out, want) {
			t.Fatalf("share refusal missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, plantedKey) {
		t.Fatalf("the CLI echoed the secret back:\n%s", out)
	}
	if strings.Contains(out, "/s/") {
		t.Fatalf("a refused share still printed a URL:\n%s", out)
	}

	// --force is the override, and the link it mints works.
	out, err = run(work, "share", "deploy.md", "--force")
	if err != nil || !strings.Contains(out, "/s/") {
		t.Fatalf("share --force: %v\n%s", err, out)
	}
	link := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Deploy") {
		t.Fatalf("forced link does not serve: %d %s", resp.StatusCode, body)
	}
}

// bdrive share <folder> used to say the folder wasn't synced yet, so users went
// looking for a sync fault that wasn't there — and the CLI bolted a "run bdrive
// sync" hint onto it. Folders now get the real answer, hint-free.
func TestCLIShareFolder(t *testing.T) {
	e := newCLIEnv(t)
	run := e.run

	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"notes/readme.md", "notes/zeta.md"} {
		if err := os.WriteFile(filepath.Join(work, filepath.FromSlash(f)), []byte("# "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := run(work, "init", "--name", "share-folder", "--yes"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	defer run(work, "stop", work)
	if out, err := run(work, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}

	out, err := run(work, "share", "notes")
	if err == nil {
		t.Fatalf("share of a folder succeeded:\n%s", out)
	}
	for _, want := range []string{"per-file", "notes/readme.md"} {
		if !strings.Contains(out, want) {
			t.Fatalf("folder error missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"wait a few seconds", "not synced", "400 Bad Request"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("folder error should not contain %q:\n%s", unwanted, out)
		}
	}

	// A path that really is missing keeps the sync-timing diagnosis and its hint.
	out, err = run(work, "share", "missing.md")
	if err == nil {
		t.Fatalf("share of a missing file succeeded:\n%s", out)
	}
	for _, want := range []string{"not synced to this project yet", "wait a few seconds"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing-file error missing %q:\n%s", want, out)
		}
	}
}

// TestCLIStatusReportsUnscannedWork is BEA-106: with the daemon stopped,
// `status` answered from the state cache and the journal — neither of which
// has seen an edit nobody scanned — and reported the folder clean. A wrong
// "you're clean" is worse than no answer, so status now walks the folder
// read-only and reports that drift on its own line.
func TestCLIStatusReportsUnscannedWork(t *testing.T) {
	e := newCLIEnv(t)
	run := e.run

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "index.md"), []byte("# Index\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(work, "init", "--name", "status-drift", "--yes"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	defer run(work, "stop", work)
	if out, err := run(work, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if out, err := run(work, "stop", work); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}

	// Clean and stopped: the line is present and reads zero.
	out, err := run(work, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "local:    0 change(s) not yet scanned") {
		t.Fatalf("clean status missing a zeroed local line:\n%s", out)
	}

	// Now the reported case: edit with no daemon to scan it.
	if err := os.WriteFile(filepath.Join(work, "index.md"), []byte("# Index\n\nappended by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = run(work, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "local:    1 change(s) not yet scanned (0 new, 1 edited, 0 removed)") {
		t.Fatalf("status did not report the unscanned edit:\n%s", out)
	}
	// And it stays distinct from `pending`, which is still legitimately zero.
	if !strings.Contains(out, "pending:  0 local change(s) not yet pushed") {
		t.Fatalf("status conflated drift with pending:\n%s", out)
	}

	// status is a pure read: the edit is still uncommitted afterwards, so the
	// sync that follows is the one that journals it. (`stop` paused this
	// folder, so resuming it is `init` again.)
	if out, err := run(work, "init", "--yes"); err != nil {
		t.Fatalf("init resume: %v\n%s", err, out)
	}
	if out, err := run(work, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	out, err = run(work, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "local:    0 change(s) not yet scanned") {
		t.Fatalf("drift did not clear after a sync:\n%s", out)
	}
}
