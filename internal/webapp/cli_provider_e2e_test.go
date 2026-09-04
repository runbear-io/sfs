package webapp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// The real binary, over real HTTP, against a hub whose AuthProvider is NOT
// BuiltinAuth — the shape every managed deployment has, and the one no test in
// this repo drove.
//
// That gap is why the outage shipped and survived a release. `ownJournal`
// refuses a journal write for every provider, but the binder that satisfies it
// was wired behind `if a, ok := s.Auth.(*BuiltinAuth); ok`, so on a managed hub
// nothing was ever bound and every push was refused forever. Every existing
// test used BuiltinAuth, where the wiring happened to work, and every in-process
// test of the gate asserted the refusal — which was still correct. Only a hub
// with a different provider, driven the whole way from `bdrive login` to a
// journal object in the store, tells the two apart.
//
// The two sub-tests differ in ONE thing: whether the provider calls the binder
// at mint. Everything else — the CLI, the flags, the files — is identical.

// cliprovAuth is a managed provider: it authenticates against its own notion of
// a session, mints its own CLI tokens, and the hub knows nothing about either.
// Modelled on the cloud's, including the mint point being the only place a
// binding could be made.
type cliprovAuth struct {
	callBinder bool
	user       User
	token      string

	mu   sync.Mutex
	bind DeviceBinder
}

func (a *cliprovAuth) CLILoginPath() string { return "/auth/cli" }
func (a *cliprovAuth) Accounts() []User     { return []User{a.user} }

func (a *cliprovAuth) Authenticate(r *http.Request) (User, bool) {
	if r.Header.Get("Authorization") != "Bearer "+a.token {
		return User{}, false
	}
	return a.user, true
}

func (a *cliprovAuth) UseDeviceBinder(b DeviceBinder) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bind = b
}

// finishLogin is the mint point. The contract is: bind before the token goes
// out, and refuse the login if the bind is refused.
func (a *cliprovAuth) finishLogin(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	bind := a.bind
	a.mu.Unlock()
	if a.callBinder && bind != nil {
		if err := bind(a.user.Email, r); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"token": a.token, "user": a.user})
}

// Register mounts the CLI endpoints. The device-code flow approves itself on
// the first poll, so the whole run is headless — no browser, no cookie.
func (a *cliprovAuth) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/device/start", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"code": "cliprov", "verify_url": "/auth/device/cliprov", "interval": 1,
		})
	})
	mux.HandleFunc("POST /api/auth/device/poll", a.finishLogin)
	mux.HandleFunc("POST /api/auth/exchange", a.finishLogin)
}

// startManagedHub is startTestHub with a managed provider, and with the syncing
// account as a plain org MEMBER of somebody else's project. That last part is
// load-bearing: ownJournal has an admin recovery arm, so a user who created the
// project pushes fine with no binding at all and the bug hides. The field report
// was a member on a project owned by someone else, which is what this is.
func startManagedHub(t *testing.T, callBinder bool) (*httptest.Server, string, string) {
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
	const member = "member@x.io"
	srv := &Server{Root: be, Projects: db, Device: webDevice, Upload: UploadConfig{Enabled: true}}
	srv.Devices, _ = OpenDeviceRegistry(filepath.Join(state, "devices.json"))
	srv.Auth = &cliprovAuth{
		callBinder: callBinder,
		user:       User{ID: "u-member", Email: member, Name: "Member"},
		token:      "bdt_cliprov_token",
	}
	orgs, err := OpenOrgDB(filepath.Join(state, "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	org, err := orgs.Create("acme", "owner@x.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := orgs.AddMember(org.ID, member, "member"); err != nil {
		t.Fatal(err)
	}
	srv.Dir = LocalDirectory{OrgDB: orgs}
	p, _, err := db.GetOrCreate("team", org.ID)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, p.ID, filepath.Join(state, "storage", p.ID, "journal")
}

// cliprovRun signs the CLI in, connects it to the project, and syncs one file.
// It returns the `bdrive sync` output and the hub's journal directory.
// Returns the sync's combined output, the hub's journal directory, and the
// sync's exit status.
func cliprovRun(t *testing.T, callBinder bool) (string, string, error) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and execs the bdrive binary; skipped with -short")
	}
	bin := filepath.Join(t.TempDir(), "bdrive")
	if out, err := exec.Command("go", "build", "-o", bin,
		"github.com/runbear-io/beardrive/cmd/bdrive").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	hub, projectID, journalDir := startManagedHub(t, callBinder)

	home := t.TempDir()
	env := append(envWithout("HOME", "BDRIVE_HOME"),
		"HOME="+home, "BDRIVE_HOME="+filepath.Join(home, ".bdrive"))
	run := func(dir string, args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// The device flow approves itself, so this is the whole sign-in.
	if out, err := run(home, "login", "--device", hub.URL); err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "notes.md"), []byte("from the field report"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run(work, "stop", work) }) // never leak the daemon
	if out, err := run(work, "init", "--project", projectID, "--yes", "--no-autostart"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	// The daemon init started keeps cycling; `sync` takes the same volume flock,
	// so the two serialize rather than race. Not stopped first on purpose —
	// `bdrive stop` PAUSES the mount, and a paused mount refuses to sync at all.
	// The exit code is returned rather than asserted here: a refused push now
	// fails the command (BEA-183), so the two callers want opposite answers and
	// the exit code is part of what each is pinning.
	out, syncErr := run(work, "sync", work)
	return out, journalDir, syncErr
}

// A managed provider that honours the contract: the device binds at mint and
// the CLI's journal reaches the hub.
func TestCLIManagedProviderDeviceCanPush(t *testing.T) {
	out, journalDir, syncErr := cliprovRun(t, true)

	if syncErr != nil {
		t.Fatalf("sync: %v\n%s", syncErr, out)
	}
	if strings.Contains(out, "read-only") {
		t.Fatalf("a bound device was refused:\n%s", out)
	}
	entries, err := os.ReadDir(journalDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no journal reached the hub (%v): the push did not land\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(journalDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "notes.md") {
		t.Fatalf("the hub's journal does not carry the edit: %s", body)
	}
}

// And the outage itself, so the fix cannot silently regress into it: a provider
// that ignores the binder produces exactly the reported symptom — a `write`
// member whose blobs upload and whose journal is refused, forever.
func TestCLIManagedProviderThatIgnoresTheBinderIsRefused(t *testing.T) {
	out, journalDir, syncErr := cliprovRun(t, false)

	// Non-zero as well as loud. Printing the refusal and exiting 0 is what let
	// a day of work sit unpushed and unnoticed (BEA-183): the summary above it
	// reads `local changes: 0`, so every visible signal said success.
	if syncErr == nil {
		t.Fatalf("a refused push exited 0 — the failure reads as success:\n%s", out)
	}
	if !strings.Contains(out, "read-only") {
		t.Fatalf("expected the documented refusal, got:\n%s", out)
	}
	if !strings.Contains(out, "not registered to your account") {
		t.Fatalf("the refusal did not say why — that sentence is the only thing that "+
			"tells this apart from a genuine read-only grant:\n%s", out)
	}
	if entries, err := os.ReadDir(journalDir); err == nil && len(entries) > 0 {
		t.Fatalf("an unbound device wrote a journal to the hub: %v", entries)
	}
}
