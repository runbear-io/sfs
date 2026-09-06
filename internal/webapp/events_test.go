package webapp

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// handleEvents blocks until the connection goes away, so the one thing it must
// never do is outlive it. A ResponseRecorder never disconnects, which is what
// makes this worth pinning: a future test poking the route with the ordinary
// do() helper would otherwise hang the whole package until its timeout.
func TestEventStreamReturnsWhenTheClientGoesAway(t *testing.T) {
	s := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleEvents(nil, httptest.NewRecorder(), r)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleEvents did not return after the request context was cancelled")
	}
	// And it let go of its slot on the way out.
	if n := s.events().total; n != 0 {
		t.Fatalf("subscriber count = %d after the stream ended, want 0", n)
	}
}

// The other half of the loop: the sync client's Watcher capability against a
// real hub. This is what replaces the daemon's 10s remote gate, so it is worth
// exercising over a real connection rather than trusting the two halves apart.
func TestRemoteWatchReceivesAPush(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	be, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	w, ok := be.(remote.Watcher)
	if !ok {
		t.Fatal("the https backend must implement remote.Watcher")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := w.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		req, _ := http.NewRequest("PUT", ts.URL+"/api/p/"+p.ID+"/upload/content?path=note.md",
			strings.NewReader("hello"))
		wr, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		wr.Body.Close()
		select {
		case _, open := <-ch:
			if !open {
				t.Fatal("watch channel closed instead of signalling")
			}
			return
		case <-deadline:
			t.Fatal("no wakeup within 5s")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Cancelling the context must end the stream and close the channel, so the
// daemon's stopWatch actually stops it rather than leaking a goroutine per
// backend it drops.
func TestRemoteWatchClosesOnCancel(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	be, err := remote.Open(context.Background(), ts.URL+"/p/"+p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := be.(remote.Watcher).Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, open := <-ch:
		if open {
			t.Fatal("expected a closed channel, got a signal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel still open 5s after cancel")
	}
}

func TestEventHubFanOut(t *testing.T) {
	h := &eventHub{subs: map[string]map[*subscriber]struct{}{}}
	a, ok := h.subscribe("p1")
	if !ok {
		t.Fatal("subscribe a")
	}
	b, ok := h.subscribe("p1")
	if !ok {
		t.Fatal("subscribe b")
	}
	other, ok := h.subscribe("p2")
	if !ok {
		t.Fatal("subscribe other")
	}

	h.publish("p1", changeEvent{Type: "change", Paths: []string{"a.md"}})

	for name, sub := range map[string]*subscriber{"a": a, "b": b} {
		select {
		case frame := <-sub.ch:
			if !strings.Contains(string(frame), "a.md") {
				t.Errorf("%s: frame = %s", name, frame)
			}
		default:
			t.Errorf("%s: no frame", name)
		}
	}
	// A project's changes are that project's business: p2 subscribed to a
	// different id and must not learn a path from p1.
	select {
	case frame := <-other.ch:
		t.Fatalf("p2 subscriber got a p1 frame: %s", frame)
	default:
	}

	h.unsubscribe("p1", a)
	h.publish("p1", changeEvent{Type: "change", Paths: []string{"b.md"}})
	select {
	case <-a.ch:
		t.Fatal("unsubscribed listener still received")
	default:
	}
	if len(h.subs["p1"]) != 1 || h.total != 2 {
		t.Fatalf("after unsubscribe: subs=%d total=%d", len(h.subs["p1"]), h.total)
	}
}

// A listener that stops reading must not be able to slow a writer down, and
// must not silently miss changes either: it is told to resync instead.
func TestEventHubSlowSubscriberIsToldToResync(t *testing.T) {
	h := &eventHub{subs: map[string]map[*subscriber]struct{}{}}
	sub, _ := h.subscribe("p1")

	for i := 0; i < subBuffer+10; i++ {
		h.publish("p1", changeEvent{Type: "change", Paths: []string{"a.md"}})
	}
	if !sub.lost.Load() {
		t.Fatal("overflow did not mark the subscriber lost")
	}
	if got := len(sub.ch); got != subBuffer {
		t.Fatalf("queued %d frames, want the buffer's %d", got, subBuffer)
	}
}

func TestEventHubBounds(t *testing.T) {
	h := &eventHub{subs: map[string]map[*subscriber]struct{}{}}
	for i := 0; i < maxSubsPerProject; i++ {
		if _, ok := h.subscribe("p1"); !ok {
			t.Fatalf("refused subscriber %d, under the cap", i)
		}
	}
	if _, ok := h.subscribe("p1"); ok {
		t.Fatal("accepted a subscriber past maxSubsPerProject")
	}
	// The cap is per project, not global, until the global one is reached.
	if _, ok := h.subscribe("p2"); !ok {
		t.Fatal("a different project should still be able to subscribe")
	}
}

func TestPublishChangeTruncatesLongPathLists(t *testing.T) {
	s := &Server{}
	sub, _ := s.events().subscribe("")

	paths := make([]string, eventPathLimit+5)
	for i := range paths {
		paths[i] = "f.md"
	}
	s.publishChange(httptest.NewRequest("POST", "/", nil), "browser", paths, len(paths), 0)

	var ev changeEvent
	select {
	case frame := <-sub.ch:
		if err := json.Unmarshal(frame, &ev); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("no frame")
	}
	if len(ev.Paths) != eventPathLimit || !ev.More {
		t.Fatalf("paths=%d more=%v, want %d and more=true", len(ev.Paths), ev.More, eventPathLimit)
	}
}

// publishChange must stay silent for a write that changed nothing, so an
// idle push does not wake every client in the project.
func TestPublishChangeIgnoresEmptyWrites(t *testing.T) {
	s := &Server{}
	sub, _ := s.events().subscribe("")

	s.publishChange(httptest.NewRequest("POST", "/", nil), "sync", nil, 0, 0)
	select {
	case frame := <-sub.ch:
		t.Fatalf("published for a no-op write: %s", frame)
	default:
	}
}

func TestNewOpPaths(t *testing.T) {
	ops := []journal.Op{
		{Seq: 1, Path: "old.md"},
		{Seq: 2, Path: "a.md"},
		{Seq: 3, Path: "b.md"},
		{Seq: 4, Path: "a.md"}, // same file twice in one push is one path
	}
	got := newOpPaths(ops, 1)
	want := []string{"a.md", "b.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// End to end over real HTTP: a browser write reaches a listener's stream.
// httptest.NewServer, not a ResponseRecorder — the handler only returns when
// the connection does, so this needs a real one.
func TestEventStreamDeliversAWrite(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/p/" + p.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("events: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	frames := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data: ") {
				frames <- strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	// The subscriber registers when the handler runs, which races this write.
	// Retry rather than sleep: the write is idempotent at the same path.
	deadline := time.After(5 * time.Second)
	for {
		req, _ := http.NewRequest("PUT", ts.URL+"/api/p/"+p.ID+"/upload/content?path=note.md",
			strings.NewReader("hello"))
		wr, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		wr.Body.Close()
		if wr.StatusCode != 200 {
			t.Fatalf("upload: %d", wr.StatusCode)
		}
		select {
		case frame := <-frames:
			var ev changeEvent
			if err := json.Unmarshal([]byte(frame), &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Type != "change" || ev.Source != "browser" || len(ev.Paths) != 1 || ev.Paths[0] != "note.md" {
				t.Fatalf("event = %+v", ev)
			}
			return
		case <-deadline:
			t.Fatal("no change event within 5s")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
