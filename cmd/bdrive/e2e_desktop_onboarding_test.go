package main

// E2E harness for the onboarding wizard (frontend/e2e/desktop-onboarding.spec.ts).
// Same env gate as the other desktop harnesses: only with BDRIVE_E2E_DESKTOP=1.
// It serves the sidecar on :8996 over a wiped BDRIVE_HOME that is SIGNED IN
// with ZERO mounts — the state a user is in right after approving the
// sign-in — plus a fake hub for create-or-join and a scratch project folder
// at a fixed path the spec types into the root field (Playwright cannot drive
// the native chooser; the typed path is the seam).

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

const e2eOnboardAddr = "127.0.0.1:8996"

// e2eOnboardBase is a LITERAL /tmp path, not os.TempDir(): on macOS TempDir is
// a per-user /var/folders/... directory, and the spec has to TYPE the root into
// the form — so both sides must agree on a fixed string.
const e2eOnboardBase = "/tmp/bdrive-e2e-onboard"

// e2eOnboardRoot is the scratch "Claude Code project" the spec connects.
// Wiped and rebuilt on every start.
func e2eOnboardRoot() string { return filepath.Join(e2eOnboardBase, "acme-app") }

func TestE2EDesktopOnboarding(t *testing.T) {
	if os.Getenv("BDRIVE_E2E_DESKTOP") == "" {
		t.Skip("frontend e2e desktop harness; set BDRIVE_E2E_DESKTOP=1 to run")
	}
	ln, err := net.Listen("tcp", e2eOnboardAddr)
	if err != nil {
		t.Fatalf("cannot bind %s: %v", e2eOnboardAddr, err)
	}
	defer ln.Close()

	base := e2eOnboardBase
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	t.Setenv("BDRIVE_HOME", home)
	t.Setenv("HOME", filepath.Join(base, "user")) // agent hooks stay in the scratch

	// The project folder the spec connects: recognizable as a Claude project.
	root := e2eOnboardRoot()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# acme-app\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake hub: create-or-join by name. "shared" already exists, so the spec
	// can exercise the join banner as well as the founder path.
	projects := map[string]string{"shared": "77777777-6666-4555-8444-333333333333"}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/projects":
			rows := []string{}
			for name, id := range projects {
				rows = append(rows, `{"id":"`+id+`","name":"`+name+`"}`)
			}
			io.WriteString(w, `{"projects":[`+strings.Join(rows, ",")+`]}`)
		case r.Method == "POST" && r.URL.Path == "/api/projects":
			var req struct{ Name, Template string }
			json.NewDecoder(r.Body).Decode(&req)
			id, joined := projects[req.Name]
			if !joined {
				id = "12121212-3434-4565-8787-909090909090"
				projects[req.Name] = id
			}
			io.WriteString(w, `{"project":{"id":"`+id+`","name":"`+req.Name+`","template":"`+req.Template+`"},"created":`+
				map[bool]string{true: "false", false: "true"}[joined]+`}`)
		case r.Method == "GET" && r.URL.Path == "/api/orgs":
			io.WriteString(w, `{"orgs":[]}`) // no orgs here: the invite card must say so
		default:
			http.NotFound(w, r) // sync degrades to offline; the wizard still completes
		}
	}))
	defer hub.Close()

	if err := config.SaveSettings(config.Settings{
		Server: hub.URL, Token: "tok-e2e-onboard", Email: "priya@acme.dev", Name: "Priya",
	}); err != nil {
		t.Fatal(err)
	}

	errc := make(chan error, 1)
	go func() { errc <- http.Serve(ln, desktopHandler()) }()
	select {
	case err := <-errc:
		t.Fatalf("serve: %v", err)
	case <-time.After(3 * time.Hour):
	}
}
