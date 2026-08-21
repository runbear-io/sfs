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
)

// onboardHub is a fake hub that answers the two calls the connect step makes:
// list projects (the join lookup) and create-or-join by name. `existing` seeds
// the org's projects, so a test picks the founder or the joiner path by what
// it puts there.
func onboardHub(t *testing.T, existing map[string]string, seen *struct {
	name, template string
	created        bool
}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/projects":
			rows := []string{}
			for name, id := range existing {
				rows = append(rows, `{"id":"`+id+`","name":"`+name+`"}`)
			}
			io.WriteString(w, `{"projects":[`+strings.Join(rows, ",")+`]}`)
		case r.Method == "POST" && r.URL.Path == "/api/projects":
			var req struct{ Name, Template string }
			json.NewDecoder(r.Body).Decode(&req)
			id, joined := existing[req.Name]
			if !joined {
				id = "11111111-2222-4333-8444-55555555aaaa"
				existing[req.Name] = id
			}
			if seen != nil {
				seen.name, seen.template, seen.created = req.Name, req.Template, !joined
			}
			io.WriteString(w, `{"project":{"id":"`+id+`","name":"`+req.Name+`","template":"`+req.Template+`"},"created":`+
				map[bool]string{true: "false", false: "true"}[joined]+`}`)
		default:
			// The first sync cycle talks to /api/p/<id>/store/*; answering 404
			// makes it degrade to offline, which is what an unreachable hub
			// does — the connect step must still finish.
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// onboardEnv points BDRIVE_HOME at a scratch dir, signs the device in to the
// fake hub, and returns the sidecar under test plus a project-root folder.
func onboardEnv(t *testing.T, hub string) (*httptest.Server, string) {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	// Agent hooks write to the USER config; keep them off this developer's Mac.
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveSettings(config.Settings{Server: hub, Token: "tok-onboard", Email: "priya@acme.dev"}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# acme-app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(desktopHandler())
	t.Cleanup(ts.Close)
	return ts, root
}

func postInit(t *testing.T, ts *httptest.Server, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/api/desktop/init", strings.NewReader(body))
	req.Header.Set("X-Bdrive-Desktop", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// waitInit polls the status endpoint until the connect step settles.
func waitInit(t *testing.T, ts *httptest.Server) map[string]any {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL + "/api/desktop/init/status")
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		switch out["phase"] {
		case "done", "error":
			return out
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("connect step never finished")
	return nil
}

// TestDesktopInspect covers frame 5's live preview: what the folder is, what
// the shared folder would be, and the join lookup that makes one screen serve
// both the founder and the teammate.
func TestDesktopInspect(t *testing.T) {
	hub := onboardHub(t, map[string]string{}, nil)
	ts, root := onboardEnv(t, hub.URL)

	get := func(q string) map[string]any {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/desktop/inspect?" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	out := get("path=" + root + "&name=team")
	if out["is_claude_project"] != true {
		t.Fatalf("CLAUDE.md must be recognized: %v", out)
	}
	if out["target"] != filepath.Join(root, "team") {
		t.Fatalf("target = %v, want <root>/team", out["target"])
	}
	if out["error"] != nil || out["conflict"] != nil {
		t.Fatalf("clean folder must have no error/conflict: %v", out)
	}
	// The preview shows the project's own top level: real entries plus the
	// markers (a bare .git stays hidden, .claude/ would show).
	entries, _ := out["entries"].([]any)
	got := map[string]bool{}
	for _, e := range entries {
		got[e.(string)] = true
	}
	if !got["src/"] || !got["CLAUDE.md"] || len(entries) != 2 {
		t.Fatalf("entries = %v, want CLAUDE.md + src/", out["entries"])
	}
	if out["join"] != nil {
		t.Fatalf("no project named team on the hub yet: %v", out["join"])
	}

	// A name the org already uses flips the screen into join mode.
	hubProjects := map[string]string{"team": "99999999-8888-4777-8666-555555555555"}
	hub2 := onboardHub(t, hubProjects, nil)
	if err := config.SaveSettings(config.Settings{Server: hub2.URL, Token: "tok-onboard"}); err != nil {
		t.Fatal(err)
	}
	out = get("path=" + root + "&name=team")
	join, _ := out["join"].(map[string]any)
	if join == nil || join["project"] != hubProjects["team"] {
		t.Fatalf("join lookup = %v, want the existing project", out["join"])
	}

	// Refusals: a name is one plain folder name, never a path.
	for _, bad := range []string{"../escape", "a/b", "", ".bdrive", "."} {
		out := get("path=" + root + "&name=" + bad)
		if bad == "" {
			continue // empty means "use the default"
		}
		if out["error"] == nil {
			t.Fatalf("name %q must be refused: %v", bad, out)
		}
	}
}

// TestDesktopInitFounder is the storyboard's happy path: the folder is
// created inside the project root, seeded from the LLM wiki template, the hub
// project is created by name, and sync starts — with nothing outside
// <root>/<name> written.
func TestDesktopInitFounder(t *testing.T) {
	var seen struct {
		name, template string
		created        bool
	}
	hub := onboardHub(t, map[string]string{}, &seen)
	ts, root := onboardEnv(t, hub.URL)

	code, body := postInit(t, ts, `{"root":"`+root+`","name":"team","hooks":false}`)
	if code != 200 {
		t.Fatalf("init: %d %s", code, body)
	}
	st := waitInit(t, ts)
	if st["phase"] != "done" {
		t.Fatalf("phase = %v (%v)", st["phase"], st["error"])
	}
	if st["joined"] != false {
		t.Fatalf("founder path must report joined=false: %v", st)
	}
	// daemon.Start refuses to re-exec a TEST binary (that was a fork bomb), so
	// this path exercises the survivable-failure branch too: the connect step
	// still finishes, and says so.
	if warn, _ := st["error"].(string); !strings.Contains(warn, "background syncing did not start") {
		t.Fatalf("expected the daemon warning on a test binary, got %q", warn)
	}
	if seen.name != "team" || seen.template != "wiki" {
		t.Fatalf("hub saw name=%q template=%q, want team/wiki", seen.name, seen.template)
	}

	target := filepath.Join(root, "team")
	// Seeded from the wiki template, and the project config + ignore file are
	// inside the SHARED folder, never at the project root.
	for _, want := range []string{"index.md", "log.md", ".bdriveignore", ".bdrive/config.json"} {
		if _, err := os.Stat(filepath.Join(target, want)); err != nil {
			t.Fatalf("missing %s in the shared folder: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".bdrive")); !os.IsNotExist(err) {
		t.Fatal("the PROJECT ROOT must never become a mount")
	}
	if _, err := os.Stat(filepath.Join(root, ".bdriveignore")); !os.IsNotExist(err) {
		t.Fatal("nothing may be written outside <root>/<name>")
	}
	// The mount is registered at the shared folder.
	mounts, err := config.LoadMounts()
	if err != nil || len(mounts) != 1 {
		t.Fatalf("mounts = %v (%v)", mounts, err)
	}
	for _, mi := range mounts {
		if mi.Path != target {
			t.Fatalf("mount path = %s, want %s", mi.Path, target)
		}
	}

	// A second connect of the same folder is refused rather than making a
	// second mount of one project.
	if code, _ := postInit(t, ts, `{"root":"`+root+`","name":"team"}`); code != http.StatusConflict {
		t.Fatalf("re-connecting the same folder = %d, want 409", code)
	}
	// And so is a shared folder nested inside the one that now syncs.
	if code, _ := postInit(t, ts, `{"root":"`+target+`","name":"inner"}`); code != http.StatusConflict {
		t.Fatalf("nested mount = %d, want 409", code)
	}
}

// TestDesktopInitJoiner is frame 7: a teammate types the same name in their
// own project, so the hub hands back the existing project — and the folder is
// NOT seeded (the content arrives by syncing, not by a second template write).
func TestDesktopInitJoiner(t *testing.T) {
	var seen struct {
		name, template string
		created        bool
	}
	hub := onboardHub(t, map[string]string{"team": "99999999-8888-4777-8666-555555555555"}, &seen)
	ts, root := onboardEnv(t, hub.URL)

	if code, body := postInit(t, ts, `{"root":"`+root+`","name":"team","hooks":false}`); code != 200 {
		t.Fatalf("init: %d %s", code, body)
	}
	st := waitInit(t, ts)
	if st["phase"] != "done" {
		t.Fatalf("phase = %v (%v)", st["phase"], st["error"])
	}
	if st["joined"] != true || st["project"] != "99999999-8888-4777-8666-555555555555" {
		t.Fatalf("joiner path = %v, want joined=true on the existing project", st)
	}
	if _, err := os.Stat(filepath.Join(root, "team", "index.md")); !os.IsNotExist(err) {
		t.Fatal("joining must not seed the template over a teammate's content")
	}
}

// TestDesktopInitRefusals pins the security rows: the name may not escape the
// root, the beardrive home may never become a mount, and a failed connect
// leaves nothing behind.
func TestDesktopInitRefusals(t *testing.T) {
	hub := onboardHub(t, map[string]string{}, nil)
	ts, root := onboardEnv(t, hub.URL)

	for _, tc := range []struct{ name, body string }{
		{"escaping name", `{"root":"` + root + `","name":"../escaped"}`},
		{"path as name", `{"root":"` + root + `","name":"a/b"}`},
		{"dotted name", `{"root":"` + root + `","name":".bdrive"}`},
		{"relative root", `{"root":"work","name":"team"}`},
		{"missing root", `{"root":"` + filepath.Join(root, "nope") + `","name":"team"}`},
	} {
		if code, body := postInit(t, ts, tc.body); code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s, want 400", tc.name, code, body)
		}
	}
	// The beardrive home holds the device token: never a mount, either way round.
	home, err := config.Home()
	if err != nil {
		t.Fatal(err)
	}
	if code, body := postInit(t, ts, `{"root":"`+home+`","name":"team"}`); code != http.StatusBadRequest {
		t.Fatalf("home as root = %d %s, want 400", code, body)
	}
	if _, err := os.Stat(filepath.Join(root, "../escaped")); !os.IsNotExist(err) {
		t.Fatal("a refused name must never create a folder")
	}

	// Signed out: no connect at all.
	if err := config.SaveSettings(config.Settings{Server: hub.URL}); err != nil {
		t.Fatal(err)
	}
	if code, _ := postInit(t, ts, `{"root":"`+root+`","name":"team"}`); code != http.StatusUnauthorized {
		t.Fatalf("signed-out connect = %d, want 401", code)
	}
	// Guard: a drive-by page cannot start one.
	req, _ := http.NewRequest("POST", ts.URL+"/api/desktop/init", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("headerless init = %d, want 403", resp.StatusCode)
	}
}

// TestDesktopInspectBlockedFolder pins the macOS privacy behavior found on
// 2026-08-21: a folder the OS gates (Desktop/Documents/Downloads) does not
// refuse the syscall — it BLOCKS, because the sidecar has no UI for the
// prompt. The flow must never hang on that; it must say what to do. A blocked
// filesystem is simulated by a probe that never returns.
func TestDesktopInspectBlockedFolder(t *testing.T) {
	slow := make(chan struct{}) // never closed: the call never returns
	start := time.Now()
	_, err := probe("/Users/someone/Documents", func() (int, error) {
		<-slow
		return 0, nil
	})
	if err == nil {
		t.Fatal("a filesystem call that never returns must not be waited on forever")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("probe waited %s; the whole point is a bound", took)
	}
	// The message is the feature: it has to name the folder and the fix.
	for _, want := range []string{"Documents", "System Settings", "Files and Folders", "BearDrive"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("blocked-folder message %q is missing %q", err, want)
		}
	}
}
