package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

// TestDesktopViewerReadReachesTheHub: opening a file in the Mac app counts.
//
// The sidecar answers the viewer routes from local state and keeps no
// ReadLedger, so before this a person reading here was recorded nowhere while
// the same file opened in the web app counted — the dashboard's
// reads-x-staleness quadrant under-reported exactly the reader the app is for.
//
// The assertion that matters is the KIND. Routing these through the report
// route's default would have filed a person's browsing as device traffic,
// which is not a smaller error than the gap, just a better-hidden one.
func TestDesktopViewerReadReachesTheHub(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	const mountID = "m-read0001"
	const hubID = "4d2c6e8a-1b3f-4c5d-8e7a-9f0b1c2d3e4f"

	type report struct {
		Kind  string `json:"kind"`
		Reads []struct {
			Path string `json:"path"`
		} `json:"reads"`
	}
	got := make(chan report, 4)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" && r.URL.Path == "/api/p/"+hubID+"/reads" {
			var rep report
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &rep)
			got <- rep
			io.WriteString(w, `{"accepted":1}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer hub.Close()

	folder := t.TempDir()
	remoteURL := hub.URL + "/p/" + hubID
	if _, err := config.SaveProject(folder, config.Project{ID: mountID, Remote: remoteURL}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveMounts(map[string]config.MountInfo{mountID: {Path: folder, Remote: remoteURL}}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveSettings(config.Settings{Server: hub.URL, Token: "tok-read"}); err != nil {
		t.Fatal(err)
	}
	volDir, err := config.VolumeDir(mountID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(volDir)
	if err != nil {
		t.Fatal(err)
	}
	sha, _, err := st.PutBlobBytes([]byte("# Notes\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = st.AppendOps("dev-r", []journal.Op{{
		Seq: 1, Lamport: 1, Time: time.Now().UTC(), Device: "dev-r",
		Kind: journal.KindPut, Path: "notes.md", Blob: sha, Size: 8,
	}})
	if err != nil {
		t.Fatal(err)
	}

	drainReads() // whatever an earlier test left behind is not this test's

	ts := httptest.NewServer(desktopHandler())
	defer ts.Close()

	// A person opens the file. The render route is a viewer read on a hub and
	// has to be one here too.
	resp, err := http.Get(ts.URL + "/api/p/" + hubID + "/render?path=notes.md")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("render: %d", resp.StatusCode)
	}

	flushReads(context.Background())

	select {
	case rep := <-got:
		if rep.Kind != "human" {
			t.Errorf("reported kind %q, want %q: a person browsing the app is not a device", rep.Kind, "human")
		}
		if len(rep.Reads) != 1 || rep.Reads[0].Path != "notes.md" {
			t.Errorf("reported %+v, want the one path that was opened", rep.Reads)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the hub was told nothing: a file read in the Mac app is still counted nowhere")
	}
}
