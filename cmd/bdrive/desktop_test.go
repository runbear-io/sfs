package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

// TestDesktopServer drives the whole desktop sidecar in-process: a fake
// BDRIVE_HOME with one mount whose volume store holds two versions of one
// file, a fake hub for the heat proxy, and the real handler over both.
func TestDesktopServer(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	const mountID = "m-1234abcd"
	const hubID = "0b5e8a1f-2c3d-4e5f-8a9b-0c1d2e3f4a5b"

	// Fake hub: answers heat and shares, checks the device token arrives.
	var hubSawAuth, shareSawAuth, shareBody, restoreSawAuth, createSawAuth, uploadPath string
	var grantSawAuth, renameSawAuth string
	var uploadBytes int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/p/"+hubID+"/heat":
			hubSawAuth = r.Header.Get("Authorization")
			io.WriteString(w, `{"entries":{"readme.md":{"human":3}},"since":"2026-08-01"}`)
		case r.Method == "POST" && r.URL.Path == "/api/p/"+hubID+"/shares":
			shareSawAuth = r.Header.Get("Authorization")
			b, _ := io.ReadAll(r.Body)
			shareBody = string(b)
			io.WriteString(w, `{"url":"https://hub.example/s/sh-abc","token":"sh-abc"}`)
		case r.Method == "GET" && r.URL.Path == "/api/p/"+hubID+"/shares":
			io.WriteString(w, `{"shares":[]}`)
		case r.Method == "DELETE" && r.URL.Path == "/api/shares/sh-abc":
			io.WriteString(w, `{"ok":true}`)
		case r.Method == "POST" && r.URL.Path == "/api/p/"+hubID+"/restore":
			restoreSawAuth = r.Header.Get("Authorization")
			io.WriteString(w, `{"ok":true}`)
		case r.Method == "GET" && r.URL.Path == "/api/p/"+hubID+"/permissions":
			io.WriteString(w, `{"default":"write","me":"admin","grants":[{"email":"mino@runbear.io","level":"read"}]}`)
		case r.Method == "PUT" && r.URL.Path == "/api/p/"+hubID+"/permissions/mino@runbear.io":
			grantSawAuth = r.Header.Get("Authorization")
			io.WriteString(w, `{"ok":true}`)
		case r.Method == "PATCH" && r.URL.Path == "/api/projects/"+hubID:
			renameSawAuth = r.Header.Get("Authorization")
			io.WriteString(w, `{"ok":true}`)
		case r.Method == "POST" && r.URL.Path == "/api/projects":
			createSawAuth = r.Header.Get("Authorization")
			io.WriteString(w, `{"id":"33333333-4444-4555-8666-777777777777","name":"fresh","perm":"admin"}`)
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/api/p/") && strings.HasSuffix(r.URL.Path, "/upload/content"):
			n, _ := io.Copy(io.Discard, r.Body)
			uploadPath, uploadBytes = r.URL.Path, n
			io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	// One mount: working folder + registry row + settings.
	folder := t.TempDir()
	if _, err := config.SaveProject(folder, config.Project{ID: mountID, Remote: hub.URL + "/p/" + hubID}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveMounts(map[string]config.MountInfo{mountID: {Path: folder, Remote: hub.URL + "/p/" + hubID}}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveSettings(config.Settings{Server: hub.URL, Token: "tok123"}); err != nil {
		t.Fatal(err)
	}

	// Its volume store: two versions of readme.md under device dev-a.
	volDir, err := config.VolumeDir(mountID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(volDir)
	if err != nil {
		t.Fatal(err)
	}
	sha1, _, err := st.PutBlobBytes([]byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	sha2, _, err := st.PutBlobBytes([]byte("hello desktop"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	err = st.AppendOps("dev-a", []journal.Op{
		{Seq: 1, Lamport: 1, Time: now.Add(-time.Hour), Device: "dev-a", Kind: journal.KindPut,
			Path: "readme.md", Blob: sha1, Size: 11, User: "snow@runbear.io", UserName: "Snow"},
		{Seq: 2, Lamport: 2, Time: now, Device: "dev-a", Kind: journal.KindPut,
			Path: "readme.md", Blob: sha2, Size: 13, User: "snow@runbear.io", UserName: "Snow"},
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(desktopHandler())
	defer ts.Close()

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// Config: desktop hub mode, no auth, heat on.
	code, body := get("/api/config")
	if code != 200 {
		t.Fatalf("config: %d %s", code, body)
	}
	var cfg struct {
		Mode    string `json:"mode"`
		Desktop bool   `json:"desktop"`
		Auth    struct {
			Enabled bool `json:"enabled"`
		} `json:"auth"`
		Reads struct {
			Enabled bool `json:"enabled"`
		} `json:"reads"`
		Upload struct {
			Enabled bool `json:"enabled"`
		} `json:"upload"`
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	// upload.enabled=true is what offers project creation in the UI; the
	// create and its uploads are hub proxies, never local writes.
	if cfg.Mode != "hub" || !cfg.Desktop || cfg.Auth.Enabled || !cfg.Reads.Enabled || !cfg.Upload.Enabled {
		t.Fatalf("config = %s", body)
	}

	// Projects: the mount, as its hub project id, read-only, named by folder.
	code, body = get("/api/projects")
	if code != 200 || !strings.Contains(body, hubID) {
		t.Fatalf("projects: %d %s", code, body)
	}
	if !strings.Contains(body, `"perm":"read"`) {
		t.Fatalf("desktop project not read-only: %s", body)
	}
	if !strings.Contains(body, filepath.Base(folder)) {
		t.Fatalf("project not named after folder: %s", body)
	}

	// Tree: current state with provenance from the journal.
	code, body = get("/api/p/" + hubID + "/tree")
	if code != 200 || !strings.Contains(body, "readme.md") || !strings.Contains(body, "snow@runbear.io") {
		t.Fatalf("tree: %d %s", code, body)
	}

	// File: latest content wins.
	code, body = get("/api/p/" + hubID + "/file?path=readme.md")
	if code != 200 || body != "hello desktop" {
		t.Fatalf("file: %d %q", code, body)
	}

	// History: both versions, oldest blob still addressable.
	code, body = get("/api/p/" + hubID + "/history?path=readme.md")
	if code != 200 || !strings.Contains(body, sha1) || !strings.Contains(body, sha2) {
		t.Fatalf("history: %d %s", code, body)
	}
	code, body = get("/api/p/" + hubID + "/blob?sha=" + sha1)
	if code != 200 || body != "hello world" {
		t.Fatalf("blob: %d %q", code, body)
	}

	// Heat: proxied to the hub with the saved token.
	code, body = get("/api/p/" + hubID + "/heat?days=30")
	if code != 200 || !strings.Contains(body, "readme.md") {
		t.Fatalf("heat: %d %s", code, body)
	}
	if hubSawAuth != "Bearer tok123" {
		t.Fatalf("hub saw auth %q", hubSawAuth)
	}

	// Shares are hub-backed: reads and same-origin writes proxy through with
	// the device token; a cross-origin browser write is refused before the
	// hub is ever contacted (any website can POST to loopback).
	code, body = get("/api/p/" + hubID + "/shares")
	if code != 200 || !strings.Contains(body, `"shares":[]`) {
		t.Fatalf("share list: %d %s", code, body)
	}
	shareReq := func(origin string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+"/api/p/"+hubID+"/shares", strings.NewReader(`{"path":"readme.md"}`))
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := shareReq("https://evil.example"); code != 403 || shareSawAuth != "" {
		t.Fatalf("cross-origin share create = %d (hub saw %q)", code, shareSawAuth)
	}
	code, body = shareReq(ts.URL)
	if code != 200 || !strings.Contains(body, "https://hub.example/s/sh-abc") {
		t.Fatalf("share create: %d %s", code, body)
	}
	if shareSawAuth != "Bearer tok123" || !strings.Contains(shareBody, "readme.md") {
		t.Fatalf("hub saw auth %q body %q", shareSawAuth, shareBody)
	}
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/shares/sh-abc", nil)
	req.Header.Set("Origin", ts.URL)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("share revoke: %v %v", err, resp.Status)
	} else {
		resp.Body.Close()
	}

	// Restore is hub-backed like shares: same-origin writes proxy through
	// with the device token (the hub journals the op and enforces the real
	// permission); a cross-origin write is refused before the hub is reached.
	restoreReq := func(origin string) int {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+"/api/p/"+hubID+"/restore", strings.NewReader(`{"path":"readme.md","sha":"`+sha1+`"}`))
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := restoreReq("https://evil.example"); code != 403 || restoreSawAuth != "" {
		t.Fatalf("cross-origin restore = %d (hub saw %q)", code, restoreSawAuth)
	}
	if code := restoreReq(ts.URL); code != 200 || restoreSawAuth != "Bearer tok123" {
		t.Fatalf("restore proxy = %d (hub saw %q)", code, restoreSawAuth)
	}

	// The People panel reads the hub's grants, not the local registry's
	// empty ones.
	code, body = get("/api/p/" + hubID + "/permissions")
	if code != 200 || !strings.Contains(body, "mino@runbear.io") {
		t.Fatalf("permissions proxy: %d %s", code, body)
	}

	// Project creation goes to the signed-in hub (2026-08-20 owner decision),
	// and template seeding streams uploads to the project's hub — falling
	// back to the signed-in hub for a project that has no local mount yet.
	// The 3 MiB body pins streaming: the old buffered proxy capped at 1 MiB.
	doOrigin := func(method, path string, body io.Reader) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, body)
		req.Header.Set("Origin", ts.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body = doOrigin("POST", "/api/projects", strings.NewReader(`{"name":"fresh"}`))
	if code != 200 || !strings.Contains(body, "fresh") || createSawAuth != "Bearer tok123" {
		t.Fatalf("project create proxy: %d %s (hub saw %q)", code, body, createSawAuth)
	}
	const freshID = "33333333-4444-4555-8666-777777777777"
	big := strings.Repeat("x", 3<<20)
	code, _ = doOrigin("PUT", "/api/p/"+freshID+"/upload/content?path=index.md", strings.NewReader(big))
	if code != 200 || uploadBytes != int64(len(big)) || !strings.Contains(uploadPath, freshID) {
		t.Fatalf("upload proxy: %d, hub got %d bytes at %q", code, uploadBytes, uploadPath)
	}

	// Project metadata and grant edits are hub writes: proxied with the
	// token, admin-gated by the hub itself.
	if code, _ := doOrigin("PATCH", "/api/projects/"+hubID, strings.NewReader(`{"name":"renamed"}`)); code != 200 || renameSawAuth != "Bearer tok123" {
		t.Fatalf("rename proxy = %d (hub saw %q)", code, renameSawAuth)
	}
	if code, _ := doOrigin("PUT", "/api/p/"+hubID+"/permissions/mino@runbear.io", strings.NewReader(`{"level":"write"}`)); code != 200 || grantSawAuth != "Bearer tok123" {
		t.Fatalf("grant proxy = %d (hub saw %q)", code, grantSawAuth)
	}


	// Local writes refuse: the store push (a journal write) never proxies.
	for _, w := range []struct{ method, path, body string }{
		{"PUT", "/api/p/" + hubID + "/store/object?key=journal/evil.jsonl", "{}"},
	} {
		req, _ := http.NewRequest(w.method, ts.URL+w.path, strings.NewReader(w.body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("%s %s succeeded (%d); desktop must be read-only", w.method, w.path, resp.StatusCode)
		}
	}

	// The journal file was never touched by any of the above.
	if devs, _ := st.Devices(); len(devs) != 1 || devs[0] != "dev-a" {
		t.Fatalf("journals changed: %v", devs)
	}

	// SPA fallback serves the frontend shell for deep links.
	code, body = get("/" + hubID + "/history")
	if code != 200 || !strings.Contains(body, "<div id=") {
		t.Fatalf("spa fallback: %d %.100s", code, body)
	}

	// Sync control: status reports the mount, unpaused.
	code, body = get("/api/desktop/status")
	if code != 200 || !strings.Contains(body, hubID) || !strings.Contains(body, `"paused":false`) {
		t.Fatalf("status: %d %s", code, body)
	}

	post := func(path string, withHeader bool) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+path, nil)
		if withHeader {
			req.Header.Set("X-Bdrive-Desktop", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Control POSTs without the CSRF header refuse — any web page can fire a
	// cross-origin POST at loopback.
	if code, _ := post("/api/desktop/p/"+hubID+"/pause", false); code != 403 {
		t.Fatalf("headerless pause = %d", code)
	}

	// Sync-now runs a real cycle: it materializes readme.md into the folder.
	code, body = post("/api/desktop/p/"+hubID+"/sync", true)
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("sync: %d %s", code, body)
	}
	if got, err := os.ReadFile(filepath.Join(folder, "readme.md")); err != nil || string(got) != "hello desktop" {
		t.Fatalf("sync did not materialize: %v %q", err, got)
	}

	// Pause sticks (the marker the agent hooks honor), then shows in status,
	// and sync-now respects it.
	if code, body := post("/api/desktop/p/"+hubID+"/pause", true); code != 200 {
		t.Fatalf("pause: %d %s", code, body)
	}
	if _, body := get("/api/desktop/status"); !strings.Contains(body, `"paused":true`) {
		t.Fatalf("status after pause: %s", body)
	}
	if code, _ := post("/api/desktop/p/"+hubID+"/sync", true); code != 409 {
		t.Fatalf("sync while paused = %d", code)
	}
}

// TestDesktopSessionFlow drives session/login/logout against a fake hub:
// logout revokes the token on the hub before clearing it locally; the login
// handler's no-auth path saves the server without a browser flow.
func TestDesktopSessionFlow(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())

	var revokedAuth string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "DELETE" && r.URL.Path == "/api/auth/token":
			revokedAuth = r.Header.Get("Authorization")
			w.WriteHeader(200)
		case r.URL.Path == "/api/config":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"mode":"hub","auth":{"enabled":false}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	if err := config.SaveSettings(config.Settings{Server: hub.URL, Token: "tokX", Email: "snow@runbear.io", Name: "Snow"}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(desktopHandler())
	defer ts.Close()

	call := func(method, path, body string, withHeader bool) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if withHeader {
			req.Header.Set("X-Bdrive-Desktop", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Session reports the saved sign-in.
	code, body := call("GET", "/api/desktop/session", "", false)
	if code != 200 || !strings.Contains(body, `"signed_in":true`) || !strings.Contains(body, "snow@runbear.io") {
		t.Fatalf("session: %d %s", code, body)
	}

	// The web config carries the same account (the sidebar account menu),
	// sourced from settings.json since the desktop has no Auth provider.
	if _, body := call("GET", "/api/config", "", false); !strings.Contains(body, `"me":{"email":"snow@runbear.io"`) {
		t.Fatalf("config me: %s", body)
	}

	// Logout needs the CSRF header, then revokes on the hub FIRST.
	if code, _ := call("POST", "/api/desktop/logout", "", false); code != 403 {
		t.Fatalf("headerless logout = %d", code)
	}
	code, body = call("POST", "/api/desktop/logout", "", true)
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("logout: %d %s", code, body)
	}
	if revokedAuth != "Bearer tokX" {
		t.Fatalf("hub revocation saw %q", revokedAuth)
	}
	s, _ := config.LoadSettings()
	if s.Token != "" || s.Email != "" || s.Server != hub.URL {
		t.Fatalf("settings after logout = %+v (server must be kept, token cleared)", s)
	}
	if _, body := call("GET", "/api/desktop/session", "", false); !strings.Contains(body, `"signed_in":false`) {
		t.Fatalf("session after logout: %s", body)
	}

	// Login against a hub with auth disabled: remembers the server, no
	// browser flow. (The browser flow itself is browserLogin, exercised by
	// the login command's own tests.)
	code, body = call("POST", "/api/desktop/login", `{"server":"`+hub.URL+`"}`, true)
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("login: %d %s", code, body)
	}
	if s, _ := config.LoadSettings(); s.Server != hub.URL {
		t.Fatalf("login did not remember the server: %+v", s)
	}

	// A garbage server URL is refused before anything is touched.
	if code, _ := call("POST", "/api/desktop/login", `{"server":"ftp://nope"}`, true); code != 400 {
		t.Fatalf("bad server url = %d", code)
	}
}

// TestDesktopLoginBrowserFlow drives the real PKCE loopback flow end to end
// against a fake auth-enabled hub, with the test standing in for the user's
// browser: fetch the sign-in URL, follow the hub's redirect into the loopback
// callback, and let the sidecar exchange the code. This is the flow "Sign
// in…"/switch-account/sign-up all ride — the hub's /auth page (where signup
// lives) is what the opened URL serves; the desktop's job ends at opening it
// and completing the callback.
func TestDesktopLoginBrowserFlow(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())

	var exchanged struct{ code, verifier, device string }
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/config":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"mode":"hub","auth":{"enabled":true,"cli_login":"/auth/cli"}}`)
		case r.URL.Path == "/auth/cli":
			// The hub's sign-in page: a real one authenticates (or signs up)
			// the user first; this one approves instantly.
			q := r.URL.Query()
			http.Redirect(w, r, q.Get("redirect")+"?state="+q.Get("state")+"&code=c0de", http.StatusFound)
		case r.Method == "POST" && r.URL.Path == "/api/auth/exchange":
			var req struct {
				Code     string `json:"code"`
				Verifier string `json:"code_verifier"`
				Device   string `json:"device"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			exchanged.code, exchanged.verifier, exchanged.device = req.Code, req.Verifier, req.Device
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"token":"tok-browser","user":{"email":"new@runbear.io","name":"New"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	// Stand in for the user's browser: fetch the sign-in URL; http.Get
	// follows the hub's redirect into the loopback callback.
	prev := openBrowser
	openBrowser = func(u string) error {
		go func() {
			if resp, err := http.Get(u); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	defer func() { openBrowser = prev }()

	ts := httptest.NewServer(desktopHandler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/desktop/login", strings.NewReader(`{"server":"`+hub.URL+`"}`))
	req.Header.Set("X-Bdrive-Desktop", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "new@runbear.io") {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	if exchanged.code != "c0de" || exchanged.verifier == "" {
		t.Fatalf("exchange saw code=%q verifier=%q — PKCE verifier must travel", exchanged.code, exchanged.verifier)
	}
	// The hub should record which app asked, not just which Mac.
	if !strings.Contains(exchanged.device, "BearDrive Desktop") {
		t.Fatalf("exchange device = %q, want it to name the app", exchanged.device)
	}
	s, err := config.LoadSettings()
	if err != nil || s.Token != "tok-browser" || s.Email != "new@runbear.io" || s.Server != hub.URL {
		t.Fatalf("settings after browser login = %+v (%v)", s, err)
	}
}

// TestDesktopFolderRulesProxy: the Mac app reads folder permissions from the
// hub, not from its own registry.
//
// Its own fixture rather than a step in TestDesktopServer, for a reason worth
// keeping: a hub that answers /scope changes what a sync cycle DOES — a moved
// scope tag makes the syncer drop its peer journals and re-pull — so bolting
// this onto the fixture that also asserts materialization made the two tests
// interfere. They are separate concerns and now have separate hubs.
//
// What this pins is that answering locally would not be an ERROR, it would be
// a plausible lie. desktopProjects.Load builds a Project with an ID and a Name
// and nothing else, so the local handler reports "no rules, nothing denied"
// for a project the hub genuinely restricts: no lock in the tree, no chip in
// the listing, and a scope inviting the device to write where it may not.
func TestDesktopFolderRulesProxy(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	const mountID = "m-9f8e7d6c"
	const hubID = "7c1a3b5d-9e2f-4a6b-8c0d-1e3f5a7b9c2d"

	var ruleSawAuth string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/p/"+hubID+"/folders":
			io.WriteString(w, `{"folders":[{"prefix":"vault/","default":"none","grants":[],"me":"admin"}],"scope":"t1"}`)
		case r.Method == "GET" && r.URL.Path == "/api/p/"+hubID+"/scope":
			io.WriteString(w, `{"scope":"t1","readonly":["notes/"],"deny":["vault/"]}`)
		case r.Method == "PUT" && r.URL.Path == "/api/p/"+hubID+"/folders":
			ruleSawAuth = r.Header.Get("Authorization")
			io.WriteString(w, `{"ok":true,"prefix":"vault/"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	folder := t.TempDir()
	if _, err := config.SaveProject(folder, config.Project{ID: mountID, Remote: hub.URL + "/p/" + hubID}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveMounts(map[string]config.MountInfo{mountID: {Path: folder, Remote: hub.URL + "/p/" + hubID}}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveSettings(config.Settings{Server: hub.URL, Token: "tok123"}); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(desktopHandler())
	defer ts.Close()

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// The rule list, which drives the lock in FileTree and the chip in
	// FolderListing. Locally this is an empty list, and nothing looks wrong.
	if code, body := get("/api/p/" + hubID + "/folders"); code != 200 || !strings.Contains(body, "vault/") {
		t.Fatalf("folders proxy: %d %s", code, body)
	}
	// The device's own scope. Answered locally it says "write anywhere".
	code, body := get("/api/p/" + hubID + "/scope")
	if code != 200 || !strings.Contains(body, "vault/") || !strings.Contains(body, "notes/") {
		t.Fatalf("scope proxy: %d %s", code, body)
	}

	// The edit, which locally could never succeed: perms.go hard-returns
	// PermRead on desktop, against handleProjectFolderSet's admin gate.
	req, _ := http.NewRequest("PUT", ts.URL+"/api/p/"+hubID+"/folders",
		strings.NewReader(`{"prefix":"vault","default":"none"}`))
	req.Header.Set("Origin", ts.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || ruleSawAuth != "Bearer tok123" {
		t.Fatalf("folder rule proxy = %d (hub saw %q)", resp.StatusCode, ruleSawAuth)
	}
}
