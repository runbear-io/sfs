package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
)

// A push the hub refuses must fail the command.
//
// BEA-183: a device whose id was never bound to its account (a token minted by
// a CLI older than the binding gate) uploads its blobs fine and gets a 403 on
// the journal alone. So `bdrive sync` printed "uploading 11 files (758.6 KB)",
// then "local changes: 0", and exited 0 — for a full day, while nothing
// reached the hub and no teammate saw the work. The summary line and the hub's
// own reason were both already printed; what made the failure invisible is
// that a script, an agent, or a person skimming reads exit 0 as success.
//
// Which refusal it is deliberately does not matter here: project permissions
// and device registration produce the same 403 on the same door, and the CLI
// must not have to tell them apart to report a failure. So the hub is fronted
// by a proxy that 403s the journal PUT and nothing else — the exact shape of
// the report, without pinning the test to one of the two causes.
func TestSyncFailsWhenTheHubRefusesThePush(t *testing.T) {
	hub, browser := sec8Hub(t) // also leaves a signed-in settings.json

	target, err := url.Parse(hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Off until the project exists and the folder is enrolled: refusing before
	// then would test setup rather than sync.
	var refuse atomic.Bool
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() && r.Method == http.MethodPut &&
			strings.HasSuffix(r.URL.Path, "/store/object") &&
			strings.HasPrefix(r.URL.Query().Get("key"), "journal/") {
			http.Error(w, "this device is not registered to your account on this hub",
				http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	// The device's token only travels to the server it was issued for
	// (remote.deviceToken -> sameOrigin), so the saved server has to be the
	// front door the project's remote also names.
	s, err := config.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	s.Server = front.URL
	if err := config.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	var made struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	sec8JSON(t, browser, "POST", hub.URL+"/api/projects", map[string]string{"name": "refused"}, &made, "")
	if made.Project.ID == "" {
		t.Fatal("setup: no project created")
	}

	folder := t.TempDir()
	if _, err := config.SaveProject(folder, config.Project{
		Volume: "refused",
		Remote: front.URL + "/p/" + made.Project.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}

	refuse.Store(true)
	if err := os.WriteFile(filepath.Join(folder, "note.md"), []byte("a day of work"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := seccliRun(t, syncCmd(), []string{folder})
	if err == nil {
		t.Fatalf("a refused push exited 0 — the failure reads as success:\n%s", out)
	}
	if !strings.Contains(out, "read-only") {
		t.Errorf("the refusal never appeared in the summary:\n%s", out)
	}
	if !strings.Contains(out, "not registered to your account") {
		t.Errorf("the hub's own reason never reached the user:\n%s", out)
	}

	// `bdrive url --sync` makes the same promise in miniature — "push right
	// now, so the link works immediately" — and handing a teammate a link to
	// content the hub never received is the same silent failure. The link
	// itself is still the correct URL, so it stays on stdout for whatever
	// captured it and the warning goes to stderr.
	t.Chdir(folder)
	uc := urlCmd()
	var stderr bytes.Buffer
	uc.SetErr(&stderr)
	link, err := seccliRun(t, uc, []string{"note.md", "--sync"})
	if err != nil {
		t.Fatalf("url --sync: %v\n%s", err, link)
	}
	if !strings.Contains(link, made.Project.ID) {
		t.Errorf("url --sync stopped printing the link:\n%s", link)
	}
	if !strings.Contains(stderr.String(), "will not resolve") {
		t.Errorf("url --sync promised an immediately-resolving link and said nothing about the refusal:\n%s",
			stderr.String())
	}
}
