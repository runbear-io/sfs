package main

// In-app onboarding for BearDrive Desktop (storyboard frames 5-9): inspect a
// candidate folder, then create-and-connect a SHARED FOLDER INSIDE it.
//
// The opinionated shape: the mount is <root>/<name> (default "team"), never
// the root itself — a user's whole Claude Code project does not sync, one
// folder inside it does, and their agent reads it in every session there.
// The hub's create-or-join-by-name semantics make the same screen serve the
// second teammate: same name, same org, they join what already exists.
//
// Everything here runs the SAME path `bdrive init` runs (createProject,
// config.SaveProject, the .bdriveignore seed, installAgentHooks, startSync) —
// this file adds the folder-creation step, the validation the CLI gets from
// the user typing a path deliberately, and progress a UI can poll.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/templates"
)

// sharedNameRe is what a shared-folder name may look like: ONE ordinary path
// element. No separators (a name is never a path), no leading dot (the
// reserved dotfiles this repo cares about — .bdrive, .claude, .git — are all
// dotted, and nobody shares one deliberately), and short enough to be a
// folder on every filesystem.
var sharedNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]{0,63}$`)

// maxInspectEntries bounds the tree preview: enough to recognize the folder,
// never a directory listing of somebody's home.
const maxInspectEntries = 8

// fsProbeTimeout bounds every filesystem call this file makes on a folder the
// user names. It exists because of macOS privacy protection (TCC): ~/Desktop,
// ~/Documents and ~/Downloads are gated, and the process asking here is the
// SIDECAR — a background process with no UI. When TCC wants to ask the user,
// it has nobody to ask, so the syscall does not fail: it blocks forever. A
// user whose projects live in ~/Documents typed the path and watched the
// connect screen do nothing, with no prompt and no error (found 2026-08-21).
//
// So: never call the filesystem unbounded, and turn the timeout into the one
// message that actually helps.
const fsProbeTimeout = 1500 * time.Millisecond

// blockedErr is what the user sees when macOS is sitting on the answer.
func blockedErr(dir string) error {
	return fmt.Errorf("macOS is blocking access to %s. Open System Settings → Privacy & Security → "+
		"Files and Folders, give BearDrive access to that folder, then try again — or pick a folder "+
		"outside Desktop, Documents and Downloads", filepath.Base(dir))
}

// probe runs one filesystem call with a deadline. A call that has not returned
// by then is a privacy prompt nobody can answer, not a slow disk.
func probe[T any](dir string, fn func() (T, error)) (T, error) {
	type res struct {
		v   T
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := fn()
		ch <- res{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-time.After(fsProbeTimeout):
		var zero T
		return zero, blockedErr(dir)
	}
}

// validateShared resolves and checks a (root, name) pair, returning the
// absolute mount path <root>/<name>. Every refusal here is a security row in
// .claude/onboarding-goal.md: the answer must never be a path outside root.
func validateShared(root, name string) (string, error) {
	if !sharedNameRe.MatchString(name) {
		return "", fmt.Errorf("the shared folder's name must be a plain folder name — letters, digits, spaces, . _ - (no slashes)")
	}
	if name == "." || name == ".." || strings.EqualFold(name, config.ProjectDir) {
		return "", fmt.Errorf("%q cannot be a shared folder name", name)
	}
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("pick your project folder first")
	}
	root = filepath.Clean(root)
	fi, err := probe(root, func() (os.FileInfo, error) { return os.Stat(root) })
	if err != nil {
		if strings.HasPrefix(err.Error(), "macOS is blocking") {
			return "", err
		}
		return "", fmt.Errorf("%s is not a folder on this Mac", root)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a folder on this Mac", root)
	}
	target := filepath.Join(root, name)
	// Belt and braces: after Clean+Join the target must still be exactly one
	// element below root. A name that escaped the regex could not reach here,
	// and this says so out loud rather than trusting the regex alone.
	if filepath.Dir(target) != root || filepath.Base(target) != name {
		return "", fmt.Errorf("the shared folder must sit directly inside %s", root)
	}
	// The beardrive home holds this device's token and every project's local
	// data; syncing it (either direction of containment) pushes credentials to
	// the hub and to every teammate. Same rule `bdrive init` applies.
	if home, herr := config.Home(); herr == nil {
		if store.UnderRoot(home, target) || store.UnderRoot(target, home) {
			return "", fmt.Errorf("that folder and the beardrive home %s contain one another; it holds this device's credentials", home)
		}
	}
	return target, nil
}

// mountConflict reports, in the user's words, why this target cannot become a
// new mount — an existing mount above it (its content already syncs), below
// it (that folder is its own project), or exactly it. Empty means free.
func mountConflict(target string) string {
	mounts, err := config.LoadMounts()
	if err != nil {
		return ""
	}
	for _, mi := range mounts {
		if mi.Path == "" || !config.IsMount(mi.Path) {
			continue // stale registry row: the folder moved or is gone
		}
		switch {
		case mi.Path == target:
			return fmt.Sprintf("%s already syncs on this Mac.", target)
		case store.UnderRoot(mi.Path, target):
			return fmt.Sprintf("%s is already inside the synced folder %s.", target, mi.Path)
		case store.UnderRoot(target, mi.Path):
			return fmt.Sprintf("%s already syncs on this Mac, and it is inside the folder you picked.", mi.Path)
		}
	}
	return ""
}

// protectedRoots are the locations macOS gates behind a privacy prompt. A
// mount inside one is legal and works — but the SYNC DAEMON is a detached
// process (daemon.Start uses Setsid so syncing outlives the app), so macOS
// stops seeing its writes as the app's and asks about the helper binary
// instead. Until the app ships with a Developer ID that a grant can stick
// to, that means a prompt for every file that arrives. Say so before the
// folder is chosen, rather than letting the user discover it a day later.
func protectedRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, "Desktop"),
		filepath.Join(home, "Documents"),
		filepath.Join(home, "Downloads"),
		filepath.Join(home, "Library", "Mobile Documents"), // iCloud Drive
	}
}

// protectedWarning names the gated location a path sits in, or "".
func protectedWarning(target string) string {
	for _, p := range protectedRoots() {
		if target == p || store.UnderRoot(p, target) {
			return fmt.Sprintf("macOS protects %s. Syncing works, but the background sync runs as a "+
				"helper process, so macOS will ask permission each time a file arrives — unless you give "+
				"that helper Full Disk Access. A folder outside Desktop, Documents and Downloads avoids "+
				"it entirely.", filepath.Base(p))
		}
	}
	return ""
}

// helperPath is the binary a user has to add under Full Disk Access — the one
// the daemon runs, which is this executable. Reported alongside the warning
// so nobody has to work out where inside the app bundle it lives.
func helperPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		return resolved
	}
	return exe
}

// handleDesktopInspect answers frame 5's live preview: what this folder is,
// what the shared folder would be, and whether the team already has one by
// that name (the join state).
func handleDesktopInspect(w http.ResponseWriter, r *http.Request) {
	root := filepath.Clean(r.URL.Query().Get("path"))
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = defaultSharedName
	}
	out := map[string]any{"path": root, "name": name, "root_name": filepath.Base(root)}
	target, err := validateShared(root, name)
	if err != nil {
		out["error"] = err.Error()
		writeDesktopJSON(w, out)
		return
	}
	out["target"] = target

	// What kind of folder is this? The markers are exactly what the storyboard
	// promises to recognize — nothing is read, only stat'd.
	var markers []string
	for _, m := range []string{".claude", "CLAUDE.md", ".git"} {
		p := filepath.Join(root, m)
		if _, err := probe(root, func() (os.FileInfo, error) { return os.Stat(p) }); err == nil {
			markers = append(markers, m)
		}
	}
	out["markers"] = markers
	out["is_claude_project"] = len(markers) > 0

	// The tree preview: top-level names only, bounded. Dotfiles are skipped as
	// noise EXCEPT the markers above — the storyboard's preview shows
	// `.claude/` deliberately, because seeing it is what tells the user this
	// is the project their agent works in.
	marked := map[string]bool{}
	for _, m := range markers {
		marked[m] = true
	}
	entries := []string{}
	des, derr := probe(root, func() ([]os.DirEntry, error) { return os.ReadDir(root) })
	if derr != nil {
		out["error"] = derr.Error()
		writeDesktopJSON(w, out)
		return
	}
	for _, de := range des {
		if de.Name() == name || (strings.HasPrefix(de.Name(), ".") && !marked[de.Name()]) {
			continue
		}
		if len(entries) == maxInspectEntries {
			out["entries_truncated"] = true
			break
		}
		label := de.Name()
		if de.IsDir() {
			label += "/"
		}
		entries = append(entries, label)
	}
	out["entries"] = entries

	if c := mountConflict(target); c != "" {
		out["conflict"] = c
	}
	if warn := protectedWarning(target); warn != "" {
		out["warning"] = warn
		out["helper"] = helperPath()
	}
	if fi, err := os.Stat(target); err == nil {
		out["target_exists"] = true
		if !fi.IsDir() {
			out["conflict"] = fmt.Sprintf("%s already exists and is a file, not a folder.", target)
		}
	}

	// Join lookup: does the signed-in account's org already have a project by
	// this name? The hub is the authority — asking it is what makes the
	// founder and joiner paths one screen.
	settings, _ := config.LoadSettings()
	if settings.Token != "" && settings.Server != "" {
		if projects, err := listProjects(settings.Server, settings.Token); err == nil {
			for _, p := range projects {
				if strings.EqualFold(p.Name, name) {
					out["join"] = map[string]any{"project": p.ID, "name": p.Name}
					break
				}
			}
		}
	}
	writeDesktopJSON(w, out)
}

// The opinion, in two constants: one folder called "team" inside the project
// the user already works in, started from the LLM wiki template (owner
// decision 2026-08-21) — sources/, wiki/, index.md, log.md, the structure an
// agent can actually keep. The hub seeds it too; seedLocally is the repair
// for a hub that answered without storing it (see init.go).
const (
	defaultSharedName  = "team"
	sharedTemplateName = "wiki"
)

// initProgress is the state frame 8 polls. One onboarding at a time — the UI
// is a wizard, and two concurrent inits would race the same registry.
type initProgress struct {
	mu      sync.Mutex
	running bool
	Phase   string // creating | connecting | syncing | done | error
	Detail  string // one human line for the log strip
	Err     string
	Project string
	Name    string
	Root    string
	Mount   string
	Joined  bool
	Files   int
}

var onboarding initProgress

func (p *initProgress) set(phase, detail string) {
	p.mu.Lock()
	p.Phase, p.Detail = phase, detail
	p.mu.Unlock()
}

func (p *initProgress) fail(err error) {
	p.mu.Lock()
	p.Phase, p.Err = "error", err.Error()
	p.running = false
	p.mu.Unlock()
}

func handleDesktopInitStatus(w http.ResponseWriter, _ *http.Request) {
	onboarding.mu.Lock()
	defer onboarding.mu.Unlock()
	if onboarding.Phase == "" {
		writeDesktopJSON(w, map[string]any{"phase": "idle"})
		return
	}
	// A plain map, not the struct: initProgress carries a mutex, and handing
	// it to the encoder would copy the lock (go vet refuses it).
	writeDesktopJSON(w, map[string]any{
		"phase": onboarding.Phase, "detail": onboarding.Detail, "error": onboarding.Err,
		"project": onboarding.Project, "name": onboarding.Name, "root": onboarding.Root,
		"mount": onboarding.Mount, "joined": onboarding.Joined, "files": onboarding.Files,
	})
}

// handleDesktopInit runs the whole connect step in the background and reports
// through /init/status. It is the storyboard's one commit button.
func handleDesktopInit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Root  string `json:"root"`
		Name  string `json:"name"`
		Hooks *bool  `json:"hooks"` // default true: the Claude integration toggle
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = defaultSharedName
	}
	target, err := validateShared(filepath.Clean(req.Root), req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if c := mountConflict(target); c != "" {
		http.Error(w, c, http.StatusConflict)
		return
	}
	settings, _ := config.LoadSettings()
	if settings.Token == "" || settings.Server == "" {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	hooks := req.Hooks == nil || *req.Hooks

	onboarding.mu.Lock()
	if onboarding.running {
		onboarding.mu.Unlock()
		http.Error(w, "a folder is already being connected", http.StatusConflict)
		return
	}
	// Field-by-field, never a struct assignment: initProgress carries the
	// mutex this line holds, and copying a locked mutex into it makes the
	// Unlock below panic (the first run of TestDesktopInitFounder found it).
	onboarding.running = true
	onboarding.Phase, onboarding.Detail = "creating", "creating "+req.Name+"/"
	onboarding.Name, onboarding.Root, onboarding.Mount = req.Name, filepath.Clean(req.Root), target
	onboarding.Err, onboarding.Project, onboarding.Joined, onboarding.Files = "", "", false, 0
	onboarding.mu.Unlock()

	go runDesktopInit(target, req.Name, settings, hooks)
	writeDesktopJSON(w, map[string]any{"ok": true, "mount": target})
}

// runDesktopInit is `bdrive init` for a folder the app creates: make the
// folder, create-or-join the hub project by name, write the project config
// and ignore seed, register agent hooks, then enroll + first cycle + daemon
// through startSync. On failure it removes what it made (and only what it
// made) so a retry starts clean.
func runDesktopInit(target, name string, settings config.Settings, hooks bool) {
	createdFolder := false
	// Whether THIS run turned the parent into a workspace root, so undo can
	// hand it back: a connect that failed must not leave the user's folder
	// converted, and nothing else can un-root it (there is no CLI for that).
	createdRoot := false
	// Bounded like inspect: a protected folder blocks the syscall rather than
	// refusing it, and a connect step that hangs forever is worse than one
	// that says what to do.
	if _, err := probe(target, func() (os.FileInfo, error) { return os.Stat(target) }); err != nil {
		if strings.HasPrefix(err.Error(), "macOS is blocking") {
			onboarding.fail(err)
			return
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			onboarding.fail(err)
			return
		}
		createdFolder = true
	}
	undo := func(err error) {
		// Only ever unwinds this run's own work: the registry row and the
		// .bdrive config, plus the folder itself when we created it. A folder
		// the user already had keeps its contents.
		if proj, ok, perr := config.LoadProject(target); perr == nil && ok {
			if mounts, merr := config.LoadMounts(); merr == nil {
				delete(mounts, proj.ID)
				config.SaveMounts(mounts)
			}
			os.RemoveAll(filepath.Join(target, config.ProjectDir))
		}
		if createdFolder {
			os.RemoveAll(target)
		}
		if createdRoot {
			// This run designated it; hand the folder back exactly as it was.
			// Safe to do unconditionally here because designation is
			// synchronous — there is no in-flight write to lose a race with.
			_ = config.UndesignateWorkspace(filepath.Dir(target))
		}
		// A root that already existed keeps its manifest untouched: designation
		// is a no-op there, so this run never added an entry and there is
		// nothing to remove. Rescanning to tidy would put an unbounded read on
		// the failure path, where hanging means the user never learns the
		// connect failed at all.
		onboarding.fail(err)
	}

	onboarding.set("connecting", "connecting to "+settings.Server)
	p, created, err := createProject(settings.Server, settings.Token, name, sharedTemplateName)
	if err != nil {
		undo(err)
		return
	}
	remoteURL := settings.Server + "/p/" + p.ID
	if err := checkNotAlreadyMounted(remoteURL, target, p.Name); err != nil {
		undo(err)
		return
	}
	onboarding.mu.Lock()
	onboarding.Project, onboarding.Joined = p.ID, !created
	onboarding.mu.Unlock()

	proj, err := config.SaveProject(target, config.Project{Volume: p.Name, Remote: remoteURL})
	if err != nil {
		undo(err)
		return
	}
	// Designate the folder the user picked as their workspace root, holding
	// the project just created. Scan-free (config.DesignateWorkspace): one
	// stat and one atomic write, over a directory this flow has already
	// statted and written inside, so it needs no probe and cannot wedge the
	// connect. Everything else under the root is discovered later by the
	// daemon's refresh, which can afford to block.
	//
	// Not probed for a second reason: probe abandons a slow call without
	// cancelling it, so a "bounded" designation could still land the manifest
	// after undo removed it. Synchronous and fast is the only shape where
	// createdRoot is true if and only if a manifest exists.
	//
	// Named rootCreated, never `created`: that name is already taken by
	// whether the HUB created the project, which decides whether the template
	// is seeded. Shadowing it seeds a teammate's folder on the joiner path.
	root := filepath.Dir(target)
	rootCreated, werr := config.DesignateWorkspace(root, config.WorkspaceProject{
		Path: filepath.Base(target), ID: proj.ID,
	})
	if werr != nil {
		// Never fatal — the connect worked and the manifest holds no state —
		// but not swallowed either: without it this folder is not a root, so
		// nothing later refuses `bdrive init` above it. Goes to the sidecar's
		// stderr, which is the app log.
		fmt.Fprintf(os.Stderr, "workspace root not designated at %s: %v\n", root, werr)
	}
	createdRoot = rootCreated
	ignorePath := filepath.Join(target, ".bdriveignore")
	if _, err := os.Stat(ignorePath); os.IsNotExist(err) {
		if err := os.WriteFile(ignorePath, []byte(starterIgnore), 0o644); err != nil {
			undo(err)
			return
		}
	}
	if created {
		// Seed from the client's OWN registry whatever the hub answered:
		// seedLocally skips paths that already exist, so it is both the seed
		// and the repair when the hub ignored the template field.
		if tpl, terr := templates.Get(sharedTemplateName); terr == nil {
			if _, werr := tpl.WriteTo(target); werr != nil {
				undo(fmt.Errorf("seed the %s template: %w", tpl.Name, werr))
				return
			}
		}
	}
	if hooks {
		installAgentHooks(target)
	}
	installAutostart(false)

	onboarding.set("syncing", "syncing "+name+"/")
	warn := ""
	if err := startSync(context.Background(), target, proj, false, 3*time.Second, 10*time.Second); err != nil {
		// Everything up to the background daemon leaves a WORKING mount, so
		// only that failure is survivable — unwinding here would delete the
		// folder we just seeded and orphan the hub project. The next bdrive
		// command in the folder starts the daemon (self-heal on next touch).
		if !errors.Is(err, errDaemonStart) {
			undo(err)
			return
		}
		warn = "synced, but background syncing did not start — reopen this folder to retry"
	}
	files := 0
	if des, derr := os.ReadDir(target); derr == nil {
		for _, de := range des {
			if !strings.HasPrefix(de.Name(), ".") {
				files++
			}
		}
	}
	onboarding.mu.Lock()
	onboarding.Phase, onboarding.Detail, onboarding.Files = "done", "synced", files
	onboarding.Err = warn
	onboarding.running = false
	onboarding.mu.Unlock()
}

// handleDesktopChooseFolder shows the real macOS folder chooser and returns
// what the user picked. osascript rather than a shell-side dialog: the picker
// is product behavior, and the shell stays dumb (.claude/desktop-goal.md).
// Playwright cannot drive a native dialog, so the connect form also accepts a
// typed path — that is the e2e seam, and this endpoint is smoke-tested.
func handleDesktopChooseFolder(w http.ResponseWriter, _ *http.Request) {
	out, err := exec.Command("osascript", "-e",
		`try
  POSIX path of (choose folder with prompt "Choose your project folder")
on error number -128
  return ""
end try`).Output()
	if err != nil {
		http.Error(w, "could not open the folder chooser", http.StatusInternalServerError)
		return
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		writeDesktopJSON(w, map[string]any{"canceled": true})
		return
	}
	writeDesktopJSON(w, map[string]any{"path": filepath.Clean(path)})
}
