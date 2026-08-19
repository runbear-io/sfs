package webapp

// Round 5 — the deferred items, attacked instead of restated.
//
// Four CISOs deferred three rows with "no reproducer" or "needs a design
// decision". This file either produces the reproducer or turns the deferral
// into a guarded invariant, so the row stops being a standing claim:
//
//	Part 1  perms.go:projectPerm's `s.Dir == nil || s.Auth == nil -> PermAdmin`
//	        escape, attacked on the REAL config path (bdrive serve -c ...)
//	        across every configuration shape a hub can boot in.
//	Part 2  "nothing expires": every way an account's access is supposed to
//	        end, checked against a credential minted before it ended.
//	Part 3  NUL bytes through every attacker-supplied string that becomes a
//	        stored record, on a real Postgres — reachable, and silent?
//	Part 4  the completeness sweep's most dangerous entries: the browser
//	        upload door's quota accounting, and RenderMarkdown, which has no
//	        security test at all after four rounds.
//
// Helper prefix: secdef.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/remote"
)

// ---------------------------------------------------------------------------
// Part 1 — is the PermAdmin escape reachable from any real configuration?
// ---------------------------------------------------------------------------

// secdefServe starts `bdrive serve` with the given config file contents and
// returns its base URL, or ("", output) when the process refused to boot.
// Refusing to boot is a legitimate — indeed the preferred — answer for a
// config that would leave the hub half-built; the invariant only has to hold
// for a hub that actually serves.
func secdefServe(t *testing.T, cfg map[string]any, extraArgs ...string) (base string, output func() string) {
	t.Helper()
	bin := seccfgBinary(t)
	state := t.TempDir()
	home := filepath.Join(state, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(state, "config.json")
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	port := seccfgFreePort(t)
	args := append([]string{"serve", "-c", cfgPath, "--addr", fmt.Sprintf("127.0.0.1:%d", port)}, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(envWithout("HOME", "BDRIVE_HOME"),
		"HOME="+home, "BDRIVE_HOME="+filepath.Join(home, ".bdrive"))
	var out seccfgBuf
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	base = fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/config")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return base, out.String
			}
		}
		if cmd.ProcessState != nil || strings.Contains(out.String(), "Error:") {
			return "", out.String // refused to boot: a valid outcome
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", out.String
}

// secdefProbe issues one anonymous request and reports (status, body).
func secdefProbe(t *testing.T, method, target string, body []byte) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// secdefSession signs a fresh account up on a live hub and returns a client
// holding its session.
func secdefSession(t *testing.T, base, email, name string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	resp, err := c.PostForm(base+"/auth/signup", url.Values{
		"email": {email}, "name": {name}, "password": {"password1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	u, _ := url.Parse(base)
	if len(jar.Cookies(u)) == 0 {
		t.Fatalf("fixture wrong: no session for %s (%d)", email, resp.StatusCode)
	}
	return c
}

// secdefRealProjectID signs an account up on a hub that allows it, creates a
// project, and returns its id — so the anonymous probes that follow are not
// answered 404 by proj()'s unknown-id branch before requirePerm is reached.
// When no account can be created, it returns a well-formed id that does not
// exist, which is the best a stranger could do anyway.
func secdefRealProjectID(t *testing.T, base string, allowsSignup bool) string {
	t.Helper()
	const absent = "00000000-0000-0000-0000-000000000000"
	if !allowsSignup {
		return absent
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	resp, err := c.PostForm(base+"/auth/signup", url.Values{
		"email": {"owner@x.io"}, "name": {"Owner"}, "password": {"password1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	u, _ := url.Parse(base)
	if len(jar.Cookies(u)) == 0 {
		t.Fatalf("fixture wrong: signup on a signup-allowing hub left no session (%d)", resp.StatusCode)
	}
	resp, err = c.Post(base+"/api/projects", "application/json",
		strings.NewReader(`{"name":"private"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Project Project `json:"project"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Project.ID == "" {
		t.Fatalf("fixture wrong: could not create a project to probe against (%d)", resp.StatusCode)
	}
	return out.Project.ID
}

// TestSec_Config_NoServedConfigurationReachesTheAdminEscape turns round 1's
// standing claim ("unreachable today: cmd/bdrive/web.go sets srv.Dir and
// srv.Auth in the same block, unconditionally") into an asserted invariant,
// exercised on the only path that builds a real hub.
//
// The escape, if reached, makes projectPerm return PermAdmin for EVERY caller,
// including one with no credential at all — s.Auth == nil is also what turns
// authGate off, so an anonymous request would sail through the gate and then
// be handed admin on every project. That composition is the probe: on any
// configuration that serves, an anonymous caller must reach nothing
// per-project. A configuration that refuses to boot passes trivially — the
// invariant is about hubs that serve.
func TestSec_Config_NoServedConfigurationReachesTheAdminEscape(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and execs the bdrive binary; skipped with -short")
	}
	storage := func() string {
		d := t.TempDir()
		return "file://" + d
	}
	domainGated := map[string]any{"allow_signup": true, "allowed_domains": []string{"x.io"}}
	cases := []struct {
		name string
		cfg  map[string]any
		args []string
		// allowsSignup: an account (and therefore a real project) can be
		// bootstrapped over HTTP on this configuration.
		allowsSignup bool
	}{
		{"hub-minimal", map[string]any{"remote": storage(), "upload": true}, nil, false},
		{"hub-no-upload", map[string]any{"remote": storage()}, nil, false},
		{"hub-empty-auth-block", map[string]any{
			"remote": storage(), "upload": true,
			"auth": map[string]any{},
		}, nil, false},
		{"hub-sqlite-metadata", map[string]any{
			"remote": storage(), "upload": true,
			"database": map[string]any{"driver": "sqlite", "dsn": filepath.Join(t.TempDir(), "m.db")},
			"auth":     domainGated,
		}, nil, true},
		{"hub-reads-off", map[string]any{
			"remote": storage(), "upload": true,
			"reads": map[string]any{"enabled": false},
			"auth":  domainGated,
		}, nil, true},
		{"hub-approval-gated-signup", map[string]any{
			"remote": storage(), "upload": true,
			"auth": map[string]any{"allow_signup": true, "require_approval": true},
		}, nil, false},
		{"hub-open-signup-domain-gated", map[string]any{
			"remote": storage(), "upload": true,
			"auth": domainGated,
		}, nil, true},
		// Single-volume mode is the ONE configuration where Dir and Auth are
		// legitimately nil — and therefore the one that must host no projects.
		{"single-volume-dir", map[string]any{"dir": ".", "upload": true}, nil, false},
		{"single-volume-dir-no-upload", map[string]any{"dir": "."}, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, out := secdefServe(t, tc.cfg, tc.args...)
			if base == "" {
				t.Logf("configuration refused to boot (a safe outcome):\n%s", out())
				return
			}
			// A project id that really exists where one can be made, so the
			// probe is not short-circuited by proj()'s 404 for an unknown id
			// before requirePerm is ever consulted.
			id := secdefRealProjectID(t, base, tc.allowsSignup)

			// Anonymous, no credential of any kind. Every one of these is a
			// per-project surface; on a hub with Auth set, authGate refuses
			// them, and on a hub without Auth there must be no projects to
			// reach. The escape is the only way any of them succeeds.
			probes := []struct {
				method, path string
				body         []byte
			}{
				{"GET", "/api/projects", nil},
				{"POST", "/api/projects", []byte(`{"name":"pwned"}`)},
				{"GET", "/api/projects/" + id, nil},
				{"GET", "/api/p/" + id + "/tree", nil},
				{"GET", "/api/p/" + id + "/file?path=secret.md", nil},
				{"GET", "/api/p/" + id + "/history", nil},
				{"GET", "/api/p/" + id + "/permissions", nil},
				{"PUT", "/api/p/" + id + "/permissions", []byte(`{"default":"write"}`)},
				{"PUT", "/api/p/" + id + "/permissions/attacker@evil.test", []byte(`{"level":"admin"}`)},
				{"DELETE", "/api/projects/" + id, nil},
				{"PATCH", "/api/projects/" + id, []byte(`{"name":"pwned"}`)},
				{"GET", "/api/p/" + id + "/store/list?prefix=", nil},
				{"PUT", "/api/p/" + id + "/store/object?key=blobs/x", []byte("x")},
				{"POST", "/api/p/" + id + "/shares", []byte(`{"path":"a.md"}`)},
			}
			for _, p := range probes {
				code, body := secdefProbe(t, p.method, base+p.path, p.body)
				if code == 200 {
					t.Errorf("%s %s answered 200 to an anonymous caller on config %q — "+
						"a served configuration reached projectPerm's `Dir == nil || Auth == nil -> PermAdmin` escape:\n%s",
						p.method, p.path, tc.name, body[:min(len(body), 400)])
				}
			}
			// /api/orgs is not per-project (a single-volume viewer answers it
			// with an empty list, which is not the escape), but it must never
			// name an org to a stranger.
			code, body := secdefProbe(t, "GET", base+"/api/orgs", nil)
			if code == 200 && strings.Contains(body, `"id"`) {
				t.Errorf("GET /api/orgs named an organization to an anonymous caller on config %q: %s",
					tc.name, body[:min(len(body), 400)])
			}

			// The escape has two arms and the anonymous probe only reaches the
			// `Auth == nil` one (that arm also switches authGate off). The
			// `Dir == nil` arm keeps the gate on, so it takes a real session
			// belonging to nobody: an account in a different org, with no grant
			// on this project. With orgs resolved, it is a plain outsider; with
			// s.Dir nil, it would be handed admin.
			if tc.allowsSignup {
				stranger := secdefSession(t, base, "stranger@x.io", "Stranger")
				for _, p := range []struct{ method, path string }{
					{"GET", "/api/p/" + id + "/tree"},
					{"GET", "/api/p/" + id + "/history"},
					{"GET", "/api/p/" + id + "/permissions"},
					{"GET", "/api/p/" + id + "/store/list?prefix="},
					{"GET", "/api/projects/" + id},
				} {
					req, _ := http.NewRequest(p.method, base+p.path, nil)
					resp, err := stranger.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					b, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if resp.StatusCode == 200 {
						t.Errorf("%s %s answered 200 to a signed-in account in another org on "+
							"config %q — a served configuration reached the `Dir == nil` arm of "+
							"projectPerm's PermAdmin escape:\n%s",
							p.method, p.path, tc.name, string(b[:min(len(b), 400)]))
					}
				}
			}
		})
	}
}

// TestSec_Config_OrgMigrationLeavesNoProjectWorldWritable covers the other
// half of the deferral: what is reachable DURING MigrateOrgs. The sweep runs
// at startup, before the listener exists, so nothing is reachable while it
// runs — but it can fail partway (Create succeeds, one SetOrg fails), and a
// project left org-less by a half-finished sweep is the state projectPerm has
// to fail closed on. Assert the fail-closed direction directly, on the
// registry, so a future change to that branch cannot quietly open it.
func TestSec_Config_OrgMigrationLeavesNoProjectWorldWritable(t *testing.T) {
	srv, _, _ := newHub(t, true, nil)
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Auth = auth
	orgs, err := OpenOrgDB(filepath.Join(t.TempDir(), "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Dir = LocalDirectory{OrgDB: orgs}
	h := srv.Handler()
	stranger := signupAndSession(t, h, "stranger@x.io", "Stranger", "password1")

	// newHub seeded a project with no org — exactly the pre-org shape
	// MigrateOrgs exists to sweep up, and exactly the state a half-finished
	// sweep leaves behind.
	var orphan Project
	for _, p := range srv.Projects.List() {
		if p.Org == "" {
			orphan = p
		}
	}
	if orphan.ID == "" {
		t.Fatal("fixture wrong: newHub should seed an org-less project")
	}
	req := httptest.NewRequest("GET", "/api/p/"+orphan.ID+"/tree", nil)
	req.AddCookie(stranger)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Errorf("an org-less project (the state a half-finished MigrateOrgs leaves) "+
			"is readable by an arbitrary signed-in account: %d %s", rec.Code, rec.Body)
	}

	// And the completed sweep must leave every project owned by an org, so no
	// project is left in that state once the hub is serving.
	if err := MigrateOrgs(srv.Projects, orgs, auth.Accounts()); err != nil {
		t.Fatalf("MigrateOrgs: %v", err)
	}
	for _, p := range srv.Projects.List() {
		if p.Org == "" {
			t.Errorf("project %s still has no org after MigrateOrgs — projectPerm's "+
				"org-less branch is live on a hub that has already booted", p.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Part 2 — "nothing expires": every other way access is supposed to end.
// ---------------------------------------------------------------------------

// secdefTokenFor mints a device token for an account through the real
// provider, the same way `bdrive login` does.
func secdefTokenFor(t *testing.T, auth *BuiltinAuth, email string) string {
	t.Helper()
	auth.mu.Lock()
	u := auth.findByEmail(email)
	auth.mu.Unlock()
	if u == nil {
		t.Fatalf("no account for %s", email)
	}
	tok, err := auth.issueToken(u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// secdefBearer issues a request carrying a device token.
func secdefBearer(t *testing.T, h http.Handler, method, target, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader = bytes.NewReader(nil)
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSec_Token_EveryEndOfAccessEndsTheToken walks the list the scoreboard
// records as never attacked: a device token minted before the account lost
// access must stop working at every point access is supposed to end. Round 2
// closed the password-reset path; these are the others.
func TestSec_Token_EveryEndOfAccessEndsTheToken(t *testing.T) {
	h, srv, cookies, p := permHub(t)
	_ = cookies
	auth := srv.Auth.(*BuiltinAuth)
	orgs := srv.Dir.(LocalDirectory).OrgDB

	// bob is a plain member with write; his device syncs with a token.
	tok := secdefTokenFor(t, auth, "bob@x.io")
	if rec := secdefBearer(t, h, "GET", "/api/p/"+p.ID+"/tree", tok, nil); rec.Code != 200 {
		t.Fatalf("fixture wrong: bob's device token cannot read his project: %d %s", rec.Code, rec.Body)
	}

	t.Run("permission revoked to none", func(t *testing.T) {
		if err := srv.Projects.SetPerm(p.ID, "bob@x.io", PermNone); err != nil {
			t.Fatal(err)
		}
		defer srv.Projects.ClearPerm(p.ID, "bob@x.io")
		if rec := secdefBearer(t, h, "GET", "/api/p/"+p.ID+"/tree", tok, nil); rec.Code != http.StatusForbidden {
			t.Errorf("a device token still reads the project after its account was set to %q: %d %s",
				PermNone, rec.Code, rec.Body)
		}
	})

	t.Run("removed from the org", func(t *testing.T) {
		if err := orgs.RemoveMember(p.Org, "bob@x.io"); err != nil {
			t.Fatal(err)
		}
		defer orgs.AddMember(p.Org, "bob@x.io", RoleMember)
		if rec := secdefBearer(t, h, "GET", "/api/p/"+p.ID+"/tree", tok, nil); rec.Code != http.StatusForbidden {
			t.Errorf("a device token still reads the project after its account was removed from the org: %d %s",
				rec.Code, rec.Body)
		}
	})

	t.Run("account deleted", func(t *testing.T) {
		auth.mu.Lock()
		u := auth.findByEmail("carol@x.io")
		auth.mu.Unlock()
		ctok := secdefTokenFor(t, auth, "carol@x.io")
		if rec := secdefBearer(t, h, "GET", "/api/p/"+p.ID+"/tree", ctok, nil); rec.Code != 200 {
			t.Fatalf("fixture wrong: carol's token cannot read: %d %s", rec.Code, rec.Body)
		}
		if err := auth.Deny(u.ID); err != nil { // the hub's only account-removal path
			t.Fatal(err)
		}
		if rec := secdefBearer(t, h, "GET", "/api/p/"+p.ID+"/tree", ctok, nil); rec.Code == 200 {
			t.Errorf("a device token minted for a DELETED account still reads the project: %d %s",
				rec.Code, rec.Body)
		}
		// The account is gone; its credentials must be gone from the store
		// too, or the next hub process reloads rows for a user that no longer
		// exists and the only thing standing between them and a live session
		// is that no account carries that id any more.
		auth.mu.Lock()
		orphan := 0
		for _, tk := range auth.tokens {
			if tk.User == u.ID {
				orphan++
			}
		}
		auth.mu.Unlock()
		if orphan > 0 {
			t.Errorf("deleting an account left %d of its credentials in the token table: "+
				"account removal does not revoke tokens (BuiltinAuth.Deny never calls revokeTokensFor)", orphan)
		}
	})
}

// secdefFlakyAccounts fails DeleteToken, standing in for a database that is
// briefly unreachable, out of disk, or refusing the write for any reason.
type secdefFlakyAccounts struct {
	AccountRepo
	failDelete bool
}

func (r *secdefFlakyAccounts) DeleteToken(hash string) error {
	if r.failDelete {
		return fmt.Errorf("simulated store failure")
	}
	return r.AccountRepo.DeleteToken(hash)
}

// TestSec_Token_RevocationMustNotSurviveOnlyInMemory is the exact shape round 2
// found for invites (TestSec_DB_RevokedInviteMustNotSurviveAFailedWrite),
// applied to the credential itself. revokeToken and revokeTokensFor drop the
// row from the in-memory map and then DISCARD the store's error, so a logout
// or a password reset reports success while the token stays on disk — and the
// next hub process loads it back, live.
func TestSec_Token_RevocationMustNotSurviveOnlyInMemory(t *testing.T) {
	dir := t.TempDir()
	open := func(t *testing.T, fail bool) (*BuiltinAuth, MetaStore, *secdefFlakyAccounts) {
		t.Helper()
		st, err := OpenFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		repo := &secdefFlakyAccounts{AccountRepo: st.Accounts(), failDelete: fail}
		a, err := NewBuiltinAuth(repo, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		return a, st, repo
	}

	auth, st, repo := open(t, false)
	u, err := auth.createAccount("victim@x.io", "Victim", "password1", true)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.issueToken(u.ID, "stolen-laptop")
	if err != nil {
		t.Fatal(err)
	}

	// The store now refuses the delete; the hub still reports revocation.
	repo.failDelete = true
	auth.revokeTokensFor(u.ID) // what a password reset does
	if _, ok := auth.userForToken(tok); ok {
		t.Fatal("fixture wrong: the token is still live in memory after revocation")
	}
	st.Close()

	// Restart: a fresh process, the same files.
	auth2, st2, _ := open(t, false)
	defer st2.Close()
	if _, ok := auth2.userForToken(tok); ok {
		t.Error("a token revoked by a password reset came back alive after a hub restart: " +
			"revokeTokensFor drops the row from memory and discards the store's error, so a " +
			"revocation the hub reported as done was never durable")
	}
}

// TestSec_Token_LogoutRevocationIsDurableAcrossARestart is the same question
// for the ordinary path, with no injected failure: a token the user logged out
// must not be usable by the next hub process.
func TestSec_Token_LogoutRevocationIsDurableAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewBuiltinAuth(st.Accounts(), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := auth.createAccount("victim@x.io", "Victim", "password1", true)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.issueToken(u.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	auth.revokeToken(tok)
	st.Close()

	st2, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	auth2, err := NewBuiltinAuth(st2.Accounts(), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := auth2.userForToken(tok); ok {
		t.Error("a token revoked by logout is live again after a hub restart")
	}
}

// ---------------------------------------------------------------------------
// Part 3 — NUL through every attacker-supplied string that becomes a record.
// ---------------------------------------------------------------------------

// secdefStoreHub builds a full hub whose metadata lives in the given
// MetaStore, so the same API can be driven against file, sqlite and Postgres.
func secdefStoreHub(t *testing.T, st MetaStore, root string) (http.Handler, *Server) {
	t.Helper()
	be, err := remote.Open(context.Background(), "file://"+root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	projects, err := NewProjectDB(st.Projects())
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewBuiltinAuth(st.Accounts(), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := NewOrgDB(st.Orgs())
	if err != nil {
		t.Fatal(err)
	}
	shares, err := NewShareDB(st.Shares())
	if err != nil {
		t.Fatal(err)
	}
	devices, err := NewDeviceRegistry(st.Devices())
	if err != nil {
		t.Fatal(err)
	}
	reads, err := NewReadLedger(st.Reads(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reads.Close() })
	srv := &Server{
		Root: be, Projects: projects, Device: webDevice,
		Upload: UploadConfig{Enabled: true},
		Auth:   auth, Dir: LocalDirectory{OrgDB: orgs},
		Shares: shares, Devices: devices, Reads: reads,
	}
	return srv.Handler(), srv
}

// secdefNULSurface is one attacker-supplied string that becomes a stored
// record: how to send it, whether the response means "accepted", and whether
// the value is present in a registry (live or freshly reloaded).
type secdefNULSurface struct {
	name     string
	do       func() int             // make the request, return the status
	accepted func(code int) bool    // does that status mean "the hub took it"?
	present  func(*secdefRegs) bool // is the hostile value in this set of registries?
	note     string                 // what the guard, if any, is supposed to do
}

// secdefRegs is one view of the metadata store: either the live server's
// registries or a fresh set reopened from the same store (the next process).
type secdefRegs struct {
	projects *ProjectDB
	orgs     *OrgDB
	auth     *BuiltinAuth
	shares   *ShareDB
	devices  *DeviceRegistry
}

func secdefReopen(t *testing.T, st MetaStore) *secdefRegs {
	t.Helper()
	projects, err := NewProjectDB(st.Projects())
	if err != nil {
		t.Fatalf("reopen projects: %v", err)
	}
	orgs, err := NewOrgDB(st.Orgs())
	if err != nil {
		t.Fatalf("reopen orgs: %v", err)
	}
	auth, err := NewBuiltinAuth(st.Accounts(), true, nil)
	if err != nil {
		t.Fatalf("reopen accounts: %v", err)
	}
	shares, err := NewShareDB(st.Shares())
	if err != nil {
		t.Fatalf("reopen shares: %v", err)
	}
	devices, err := NewDeviceRegistry(st.Devices())
	if err != nil {
		t.Fatalf("reopen devices: %v", err)
	}
	return &secdefRegs{projects, orgs, auth, shares, devices}
}

// TestSec_DB_NULThroughEveryStoredRecordIsRefusedNotLost sweeps every string an
// attacker supplies that becomes a row in the metadata store, on every backend
// including a real Postgres (BDRIVE_TEST_POSTGRES). Postgres cannot hold a NUL
// in a text column at all, so each surface lands in one of three states:
//
//	refused   the hub answers "no" and nothing changes anywhere      — fine
//	stored    the hub answers "yes" and the value survives a reload  — fine
//	LOST      the hub answers "yes" and the record is not there      — the bug
//
// The third is what row 14 has been carrying unexamined for two rounds: the
// hub tells the caller the change landed while the row never reached storage,
// so the live registry and disk disagree until a restart silently undoes it.
// The fourth state — refused, but applied in memory anyway — is the same
// divergence in the other direction and is asserted too.
func TestSec_DB_NULThroughEveryStoredRecordIsRefusedNotLost(t *testing.T) {
	const nul = "ev\x00il"
	const nulEmail = "ev\x00il@x.io"
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			secdefDropDeviceRows(t)
			st := be.open(t)
			defer st.Close()
			h, srv := secdefStoreHub(t, st, t.TempDir())

			alice := signupAndSession(t, h, "alice@x.io", "Alice", "password1")
			rec := doAs(t, h, "POST", "/api/projects", map[string]string{"name": "wiki"}, alice)
			if rec.Code != 200 {
				t.Fatalf("create project: %d %s", rec.Code, rec.Body)
			}
			var created struct {
				Project Project `json:"project"`
			}
			json.Unmarshal(rec.Body.Bytes(), &created)
			p := created.Project
			// bob is a second org member: an ordinary teammate, the realistic
			// attacker for every surface below.
			bob := signupAndSession(t, h, "bob@x.io", "Bob", "password1")
			if err := srv.Dir.(LocalDirectory).OrgDB.AddMember(p.Org, "bob@x.io", RoleMember); err != nil {
				t.Fatal(err)
			}
			live := &secdefRegs{
				projects: srv.Projects,
				orgs:     srv.Dir.(LocalDirectory).OrgDB,
				auth:     srv.Auth.(*BuiltinAuth),
				shares:   srv.Shares,
				devices:  srv.Devices,
			}

			// A file whose PATH carries a NUL, uploaded by bob. cleanUploadPath
			// does not reject control characters, so this lands in the journal
			// (object storage, which has no opinion about NUL) — and becomes a
			// metadata row only when someone shares it.
			nulPath := "ev\x00il.md"
			upload := doAs(t, h, "PUT",
				"/api/p/"+p.ID+"/upload/content?path="+url.QueryEscape(nulPath),
				[]byte("hello"), bob)
			nulFileLanded := upload.Code == 200

			json2xx := func(code int) bool { return code >= 200 && code < 300 }
			// The signup form is server-rendered HTML: success is the 303 to
			// the app, a refusal is the form re-rendered with an error at 200.
			redirected := func(code int) bool { return code == http.StatusSeeOther }

			surfaces := []secdefNULSurface{
				{
					name: "project name (POST /api/projects)",
					do: func() int {
						return doAs(t, h, "POST", "/api/projects", map[string]string{"name": nul}, alice).Code
					},
					accepted: json2xx,
					present: func(r *secdefRegs) bool {
						for _, q := range r.projects.List() {
							if q.Name == nul {
								return true
							}
						}
						return false
					},
				},
				{
					name: "project description (PATCH /api/projects/{id})",
					do: func() int {
						return doAs(t, h, "PATCH", "/api/projects/"+p.ID,
							map[string]string{"description": nul}, alice).Code
					},
					accepted: json2xx,
					present: func(r *secdefRegs) bool {
						q, _ := r.projects.Get(p.ID)
						return q.Description == nul
					},
				},
				{
					name: "org name (PATCH /api/orgs/{org})",
					do: func() int {
						return doAs(t, h, "PATCH", "/api/orgs/"+p.Org, map[string]string{"name": nul}, alice).Code
					},
					accepted: json2xx,
					present: func(r *secdefRegs) bool {
						o, _ := r.orgs.Get(p.Org)
						return o.Name == nul
					},
				},
				{
					name: "account name (POST /auth/signup)",
					do: func() int {
						form := url.Values{"email": {"nulname@x.io"}, "name": {nul}, "password": {"password1"}}
						req := httptest.NewRequest("POST", "/auth/signup", strings.NewReader(form.Encode()))
						req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
						r := httptest.NewRecorder()
						h.ServeHTTP(r, req)
						return r.Code
					},
					accepted: redirected,
					present: func(r *secdefRegs) bool {
						for _, u := range r.auth.Accounts() {
							if u.Name == nul {
								return true
							}
						}
						return false
					},
				},
				{
					name: "project grant email (PUT /api/p/{id}/permissions/{email})",
					do: func() int {
						return doAs(t, h, "PUT", "/api/p/"+p.ID+"/permissions/ev%00il@x.io",
							map[string]string{"level": PermRead}, alice).Code
					},
					accepted: json2xx,
					present: func(r *secdefRegs) bool {
						q, _ := r.projects.Get(p.ID)
						_, ok := q.Perms[nulEmail]
						return ok
					},
				},
				{
					name: "share path (POST /api/p/{id}/shares on a NUL-named file)",
					do: func() int {
						if !nulFileLanded {
							return 0 // the file never landed; nothing to share
						}
						return doAs(t, h, "POST", "/api/p/"+p.ID+"/shares",
							map[string]string{"path": nulPath}, bob).Code
					},
					accepted: json2xx,
					present: func(r *secdefRegs) bool {
						for _, s := range r.shares.List(p.ID) {
							if s.Path == nulPath {
								return true
							}
						}
						return false
					},
					note: "cleanUploadPath does not reject control characters",
				},
				{
					name: "device name (X-Bdrive-Device-Name on PUT /store/object)",
					do: func() int {
						// A device syncs under an identity bound at sign-in.
						secRegisterDevice(t, h, p.ID, bob, "d-secdefnul01", "bob-box", "linux")
						req := httptest.NewRequest("PUT",
							"/api/p/"+p.ID+"/store/object?key=journal/d-secdefnul01.jsonl",
							bytes.NewReader([]byte("{}\n")))
						req.AddCookie(bob)
						req.Header.Set("X-Bdrive-Device", "d-secdefnul01")
						req.Header.Set("X-Bdrive-Device-Name", nul)
						r := httptest.NewRecorder()
						h.ServeHTTP(r, req)
						return r.Code
					},
					accepted: json2xx,
					present: func(r *secdefRegs) bool {
						d, ok := r.devices.Get("d-secdefnul01")
						return ok && strings.Contains(d.Name, "\x00")
					},
					note: "observeDevice strips control characters (round 3), so the " +
						"NUL must never be present — but the device row itself must persist",
				},
			}

			type outcome struct {
				s        secdefNULSurface
				code     int
				inMemory bool
			}
			var got []outcome
			for _, s := range surfaces {
				code := s.do()
				got = append(got, outcome{s, code, s.present(live)})
			}

			// The next hub process, reading the same store.
			st2 := be.open(t)
			defer st2.Close()
			fresh := secdefReopen(t, st2)

			for _, o := range got {
				if o.code == 0 {
					t.Logf("%-62s SKIPPED (precondition not met)", o.s.name)
					continue
				}
				onDisk := o.s.present(fresh)
				ok := o.s.accepted(o.code)
				t.Logf("%-62s status=%d accepted=%v inMemory=%v onDisk=%v",
					o.s.name, o.code, ok, o.inMemory, onDisk)
				if ok && o.inMemory && !onDisk {
					t.Errorf("%s: the hub answered %d (accepted) and the value is in the live "+
						"registry, but it is NOT in the store after a reload — a NUL made the "+
						"write fail silently, so the running hub and its database disagree until "+
						"a restart undoes the change without telling anyone. %s",
						o.s.name, o.code, o.s.note)
				}
				if !ok && o.inMemory {
					t.Errorf("%s: the hub answered %d (refused) but the value IS in the live "+
						"registry — a refused write was applied in memory anyway. %s",
						o.s.name, o.code, o.s.note)
				}
			}

			// The device row is the one surface with a guard (round 3's
			// printableOnly). The guard's job is to keep the row storable, not
			// to drop it: a device that syncs must still be registered, or
			// journal ownership has nothing to resolve against.
			if _, ok := live.devices.Get("d-secdefnul01"); !ok {
				t.Errorf("a device that successfully synced with a NUL in its name header was not "+
					"registered at all on %s — the row that journal ownership (ownJournal -> "+
					"LookupIn) resolves against is missing, so the next caller to claim that "+
					"journal key wins it", be.name)
			} else if _, ok := fresh.devices.Get("d-secdefnul01"); !ok {
				t.Errorf("a device that synced with a NUL in its name header is in the live " +
					"registry but not in the store after a reload — the ownership row does not " +
					"survive a restart")
			}
		})
	}
}

// secdefDropDeviceRows clears the table db_conformance_test.go's postgres
// reset does not know about (device_rows arrived in round 4), so a run does
// not inherit another test's rows.
func secdefDropDeviceRows(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("BDRIVE_TEST_POSTGRES")
	if dsn == "" {
		return
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return
	}
	defer db.Close()
	db.Exec(`DROP TABLE IF EXISTS device_rows`)
}

// ---------------------------------------------------------------------------
// Part 4 — the completeness sweep's most dangerous untested entries.
// ---------------------------------------------------------------------------

// secdefQuota records every CheckWrite / RecordUsage the hub makes.
type secdefQuota struct {
	// Embedded so the read-side hooks (CheckRead/RecordEgress) come for
	// free: this fake exercises the write path, and a widened interface
	// should not need a no-op added here every time.
	UnlimitedQuota

	checked []int64
	usage   []int64
}

func (q *secdefQuota) CheckWrite(_ string, n int64) error {
	q.checked = append(q.checked, n)
	return nil
}
func (q *secdefQuota) CheckSeat(string, int) error { return nil }
func (q *secdefQuota) RecordUsage(_ string, n int64) {
	q.usage = append(q.usage, n)
}

// secdefUnsizedBody is a reader whose type httptest.NewRequest cannot measure,
// so the request arrives with ContentLength == -1 — exactly what any chunked
// HTTP/1.1 upload looks like to the handler.
type secdefUnsizedBody struct{ r io.Reader }

func (b *secdefUnsizedBody) Read(p []byte) (int, error) { return b.r.Read(p) }

// TestSec_Upload_UnsizedBrowserUploadIsBookedAtItsRealSize is round 1's
// finding on the OTHER write door. handleStorePut was fixed to spool the body
// and charge the bytes it actually stored, with the comment "Content-Length is
// -1 on any chunked request, which made every unsized put free".
// handleUploadContent — the browser's relayed-upload door — still takes
// max(r.ContentLength, 0) for BOTH CheckWrite and RecordUsage, while the
// RemoteSource.Upload underneath it ignores the declared size entirely and
// spools. So a chunked browser upload of any size is checked against, and
// billed at, zero bytes.
func TestSec_Upload_UnsizedBrowserUploadIsBookedAtItsRealSize(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	q := &secdefQuota{}
	srv.Quota = q
	h := srv.Handler()

	content := bytes.Repeat([]byte("A"), 4096)
	req := httptest.NewRequest("PUT", "/api/p/"+p.ID+"/upload/content?path=big.bin",
		&secdefUnsizedBody{r: bytes.NewReader(content)})
	if req.ContentLength != -1 {
		t.Fatalf("fixture wrong: ContentLength is %d, want -1 (an unsized/chunked body)", req.ContentLength)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("unsized upload: %d %s", rec.Code, rec.Body)
	}

	// The bytes really landed: the file is in the project.
	tree := httptest.NewRecorder()
	h.ServeHTTP(tree, httptest.NewRequest("GET", "/api/p/"+p.ID+"/tree", nil))
	if !strings.Contains(tree.Body.String(), "big.bin") {
		t.Fatalf("fixture wrong: the upload did not land:\n%s", tree.Body)
	}

	want := int64(len(content))
	if len(q.checked) != 1 || q.checked[0] != want {
		t.Errorf("CheckWrite saw %v bytes for a %d-byte upload — an unsized (chunked) browser upload "+
			"is admitted against a quota of zero bytes, so no plan limit applies to it", q.checked, want)
	}
	if len(q.usage) != 1 || q.usage[0] != want {
		t.Errorf("RecordUsage booked %v bytes for a %d-byte upload — the bytes are stored but never "+
			"billed, so an org's usage never grows however much it uploads this way", q.usage, want)
	}
}

// TestSec_Render_MarkdownCannotShipActiveContent: RenderMarkdown is named by no
// TestSec test after four rounds, and its output is injected into the hub's
// own origin with dangerouslySetInnerHTML — the one document that carries the
// session cookie. Every teammate with write on any project controls the input.
func TestSec_Render_MarkdownCannotShipActiveContent(t *testing.T) {
	payloads := []struct{ name, src string }{
		{"raw script block", "# hi\n\n<script>alert(1)</script>\n"},
		{"raw img onerror", "text <img src=x onerror=alert(1)> more\n"},
		{"raw svg onload", "<svg onload=alert(1)></svg>\n"},
		{"iframe", "<iframe src=\"https://evil.test\"></iframe>\n"},
		{"javascript link", "[click](javascript:alert(1))\n"},
		{"javascript link uppercase", "[click](JaVaScRiPt:alert(1))\n"},
		{"javascript link entity", "[click](java&#115;cript:alert(1))\n"},
		{"javascript link tab", "[click](java\tscript:alert(1))\n"},
		{"image with javascript src", "![x](javascript:alert(1))\n"},
		{"html comment conditional", "<!--[if IE]><script>alert(1)</script><![endif]-->\n"},
		{"autolink javascript", "<javascript:alert(1)>\n"},
		{"style block", "<style>body{background:url('javascript:alert(1)')}</style>\n"},
		{"onclick attribute on markdown emphasis", "*a* <b onclick=alert(1)>b</b>\n"},
		{"wikilink with quote break", "[[a\" onerror=\"alert(1)]]\n"},
		{"wikilink label injection", "[[target|<img src=x onerror=alert(1)>]]\n"},
		{"frontmatter value injection", "---\ntitle: <img src=x onerror=alert(1)>\n---\n\nbody\n"},
		{"frontmatter key injection", "---\n\"<img src=x onerror=alert(1)>\": v\n---\n\nbody\n"},
		{"frontmatter nested injection", "---\na:\n  b: \"</code><script>alert(1)</script>\"\n---\n\nbody\n"},
		{"gfm table cell html", "| a |\n|---|\n| <img src=x onerror=alert(1)> |\n"},
		{"base tag", "<base href=\"https://evil.test/\">\n"},
		{"form action", "<form action=\"https://evil.test\"><input name=p></form>\n"},
	}
	for _, p := range payloads {
		t.Run(p.name, func(t *testing.T) {
			out, err := RenderMarkdown([]byte(p.src))
			if err != nil {
				return // refusing to render is a safe answer
			}
			for _, why := range secdefActiveContent(out) {
				t.Errorf("RenderMarkdown emitted active content (%s) for input %q:\n%s",
					why, p.src, out)
			}
			// The viewer's entry point is a second door onto the same room:
			// it must not be a way around the escaping above. The frontmatter
			// half is the deliberate difference — those values leave as
			// LITERAL text (asserted below) and the client renders them as
			// text nodes — so only the HTML is judged here.
			_, out, err = RenderMarkdownPairs([]byte(p.src))
			if err != nil {
				return
			}
			for _, why := range secdefActiveContent(out) {
				t.Errorf("RenderMarkdownPairs emitted active content (%s) for input %q:\n%s",
					why, p.src, out)
			}
		})
	}

	// The new field carries the raw scalar: escaping moved to the client,
	// which puts it in a React text node. A value that arrived pre-escaped
	// here would show up as &lt;img …&gt; on screen instead.
	for _, c := range []struct{ name, src, key, want string }{
		{"value", "---\ntitle: <img src=x onerror=alert(1)>\n---\nbody\n",
			"title", "<img src=x onerror=alert(1)>"},
		{"key", "---\n\"<img src=x onerror=alert(1)>\": v\n---\nbody\n",
			"<img src=x onerror=alert(1)>", "v"},
		{"nested", "---\na:\n  b: \"</code><script>alert(1)</script>\"\n---\nbody\n",
			"a", "b: \"</code><script>alert(1)</script>\""},
	} {
		t.Run("pairs "+c.name, func(t *testing.T) {
			pairs, _, err := RenderMarkdownPairs([]byte(c.src))
			if err != nil || len(pairs) != 1 {
				t.Fatalf("pairs = %+v, err = %v", pairs, err)
			}
			if pairs[0].Key != c.key || pairs[0].Value != c.want {
				t.Errorf("pair = %+v, want key %q value %q — the frontmatter field is "+
					"contracted to be literal text; pre-escaping it here would double-escape "+
					"on the client", pairs[0], c.key, c.want)
			}
		})
	}
}

var (
	// A real element start tag in the emitted HTML (escaped text reads as
	// &lt;script, which is inert and must not fire).
	secdefTagRe = regexp.MustCompile(`(?is)<\s*(script|iframe|svg|style|base|form|object|embed|link|meta|frame|frameset|applet)\b`)
	// Any tag span, so attributes are only judged inside one.
	secdefAnyTagRe = regexp.MustCompile(`(?s)<[a-zA-Z/][^>]*>`)
	// An event-handler attribute inside a tag.
	secdefEventRe = regexp.MustCompile(`(?i)[\s"'/]on[a-z]+\s*=`)
	// A URL attribute whose value is an active scheme.
	secdefSchemeRe = regexp.MustCompile(`(?i)(href|src|action|formaction|xlink:href)\s*=\s*["']?\s*(javascript|vbscript|data)\s*:`)
)

// secdefActiveContent reports every way the given HTML would execute in the
// hub's own origin. It judges real markup only: escaped text (&lt;img …) is
// what a safe renderer is supposed to produce and must not be flagged.
func secdefActiveContent(html string) []string {
	var why []string
	if m := secdefTagRe.FindString(html); m != "" {
		why = append(why, "element "+strings.TrimSpace(m))
	}
	for _, tag := range secdefAnyTagRe.FindAllString(html, -1) {
		if m := secdefEventRe.FindString(tag); m != "" {
			why = append(why, "event handler attribute in "+tag)
		}
		if m := secdefSchemeRe.FindString(tag); m != "" {
			why = append(why, "active URL scheme "+m+" in "+tag)
		}
	}
	return why
}
