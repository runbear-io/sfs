package main

// E2E desktop harness for the frontend's Playwright suite (frontend/e2e,
// desktop.spec.ts). Not part of the normal test suite: it only runs with
// BDRIVE_E2E_DESKTOP=1, where it serves the desktop sidecar handler on
// :8994 over a wiped, deterministically seeded BDRIVE_HOME — one mount
// ("wiki-local") whose volume store holds a few files with history — plus an
// in-process fake hub for the proxied surfaces (heat, shares, restore,
// permissions). Playwright starts it via its webServer config and tears it
// down after the run.

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

const (
	e2eDesktopAddr = "127.0.0.1:8994"
	// e2eDesktopHub is the mount's project id on its (fake) hub — the id the
	// desktop keys everything by, shared with desktop.spec.ts.
	e2eDesktopHub = "11111111-2222-4333-8444-555555555555"
)

func TestE2EDesktop(t *testing.T) {
	if os.Getenv("BDRIVE_E2E_DESKTOP") == "" {
		t.Skip("frontend e2e desktop harness; set BDRIVE_E2E_DESKTOP=1 to run")
	}
	// Bind before wiping: a busy port must not cost a live harness its state.
	ln, err := net.Listen("tcp", e2eDesktopAddr)
	if err != nil {
		t.Fatalf("cannot bind %s (is a desktop harness already running?): %v", e2eDesktopAddr, err)
	}
	defer ln.Close()

	home := filepath.Join(os.TempDir(), "bdrive-e2e-desktop")
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BDRIVE_HOME", home)

	// The fake hub behind the proxied surfaces. Deterministic answers only.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := "/api/p/" + e2eDesktopHub
		switch {
		case r.URL.Path == p+"/heat":
			io.WriteString(w, `{"entries":{"index.md":{"human":5,"agent":2,"readers":2,"last_read":"2026-08-19T12:00:00Z"}},"since":"2026-08-01"}`)
		case r.Method == "GET" && r.URL.Path == p+"/shares":
			io.WriteString(w, `{"shares":[]}`)
		case r.Method == "POST" && r.URL.Path == p+"/shares":
			io.WriteString(w, `{"url":"https://hub.example/s/e2e-share","token":"e2e-share"}`)
		case r.Method == "POST" && r.URL.Path == p+"/restore":
			io.WriteString(w, `{"ok":true}`)
		case r.Method == "GET" && r.URL.Path == p+"/permissions":
			io.WriteString(w, `{"default":"write","me":"admin","creator":"e2e@example.com","grants":[{"email":"reader@example.com","level":"read"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()

	// One mount: registry row + folder config + signed-in settings.
	const mountID = "m-e2e00001"
	folder := filepath.Join(home, "wiki-local")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	remoteURL := hub.URL + "/p/" + e2eDesktopHub
	if _, err := config.SaveProject(folder, config.Project{ID: mountID, Remote: remoteURL}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveMounts(map[string]config.MountInfo{mountID: {Path: folder, Remote: remoteURL}}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveSettings(config.Settings{Server: hub.URL, Token: "tok-e2e", Email: "e2e@example.com", Name: "E2E"}); err != nil {
		t.Fatal(err)
	}

	// The volume store: index.md in two versions (so history has a restore
	// target) plus a linked page, journaled under one device with a signed-in
	// account for provenance.
	volDir, err := config.VolumeDir(mountID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(volDir)
	if err != nil {
		t.Fatal(err)
	}
	blob := func(s string) string {
		t.Helper()
		sha, _, err := st.PutBlobBytes([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	indexV1 := blob("# Wiki-local\n\nfirst draft\n")
	indexV2 := blob("# Wiki-local\n\nStart at [the plan](notes/plan.md).\n")
	plan := blob("# Plan\n\nShip the desktop app.\n")
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	op := func(seq, lamport int64, at time.Time, path, sha string, size int64) journal.Op {
		return journal.Op{
			Seq: seq, Lamport: lamport, Time: at, Device: "dev-e2e", Kind: journal.KindPut,
			Path: path, Blob: sha, Size: size, User: "e2e@example.com", UserName: "E2E",
		}
	}
	err = st.AppendOps("dev-e2e", []journal.Op{
		op(1, 1, base, "index.md", indexV1, 25),
		op(2, 2, base.Add(30*time.Minute), "notes/plan.md", plan, 30),
		op(3, 3, base.Add(time.Hour), "index.md", indexV2, 48),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Serve until Playwright kills the process (or the generous cap).
	errc := make(chan error, 1)
	go func() { errc <- http.Serve(ln, desktopHandler()) }()
	select {
	case err := <-errc:
		t.Fatalf("serve: %v", err)
	case <-time.After(3 * time.Hour):
	}
}
