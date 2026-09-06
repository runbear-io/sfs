package main

// Signed-out sibling of TestE2EDesktop (same env gate): the desktop sidecar
// over a COMPLETELY virgin BDRIVE_HOME — no settings, no device, no mounts —
// on :8995. This is what a fresh install shows before the first sign-in, and
// desktop-signedout.spec.ts pins the truthfulness of that first screen.

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const e2eDesktopSignedOutAddr = "127.0.0.1:8995"

func TestE2EDesktopSignedOut(t *testing.T) {
	if os.Getenv("BDRIVE_E2E_DESKTOP") == "" {
		t.Skip("frontend e2e desktop harness; set BDRIVE_E2E_DESKTOP=1 to run")
	}
	ln, err := net.Listen("tcp", e2eDesktopSignedOutAddr)
	if err != nil {
		t.Fatalf("cannot bind %s (is a signed-out harness already running?): %v", e2eDesktopSignedOutAddr, err)
	}
	defer ln.Close()

	home := filepath.Join(os.TempDir(), "bdrive-e2e-desktop-signedout")
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BDRIVE_HOME", home)

	errc := make(chan error, 1)
	go func() { errc <- http.Serve(ln, desktopHandler()) }()
	select {
	case err := <-errc:
		t.Fatalf("serve: %v", err)
	case <-time.After(3 * time.Hour):
	}
}
