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
	var hubSawAuth, shareSawAuth, shareBody string
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
	if cfg.Mode != "hub" || !cfg.Desktop || cfg.Auth.Enabled || !cfg.Reads.Enabled || cfg.Upload.Enabled {
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

	// Writes refuse: restore, project rename, store push, upload.
	for _, w := range []struct{ method, path, body string }{
		{"POST", "/api/p/" + hubID + "/restore", `{"path":"readme.md","sha":"` + sha1 + `"}`},
		{"PATCH", "/api/projects/" + hubID, `{"name":"x"}`},
		{"PUT", "/api/p/" + hubID + "/store/object?key=journal/evil.jsonl", "{}"},
		{"POST", "/api/p/" + hubID + "/upload/init", `{}`},
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
