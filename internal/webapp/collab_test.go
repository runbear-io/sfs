package webapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The rule the whole relay rests on: exactly one joiner is told to seed. Two
// clients each building a Yjs document from the same file text produce two
// DIFFERENT documents, and merging them duplicates every character — so this
// is a correctness test, not a tidiness one.
func TestCollabExactlyOneJoinerSeeds(t *testing.T) {
	room := &collabRoom{subs: map[*subscriber]struct{}{}}
	const n = 32
	var wg sync.WaitGroup
	seeds := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, first := room.join(&subscriber{ch: make(chan []byte, 1)})
			seeds[i] = first
		}(i)
	}
	wg.Wait()
	got := 0
	for _, s := range seeds {
		if s {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("%d of %d joiners were told to seed, want exactly 1", got, n)
	}
}

// A joiner after the first gets the log, which is what lets it rebuild the
// same document instead of seeding a second one.
func TestCollabLaterJoinerGetsTheLog(t *testing.T) {
	room := &collabRoom{subs: map[*subscriber]struct{}{}}
	first := &subscriber{ch: make(chan []byte, 8)}
	if _, seed := room.join(first); !seed {
		t.Fatal("the first joiner into an empty room must seed")
	}
	room.post([]byte("update-one"), first)
	room.post([]byte("update-two"), first)

	log, seed := room.join(&subscriber{ch: make(chan []byte, 8)})
	if seed {
		t.Fatal("a joiner into a non-empty room must NOT seed")
	}
	if len(log) != 2 || string(log[0]) != "update-one" || string(log[1]) != "update-two" {
		t.Fatalf("log = %v, want both updates in order", log)
	}
}

// An update reaches the other editors and is not echoed to its sender.
func TestCollabPostFansOutButNotToSender(t *testing.T) {
	room := &collabRoom{subs: map[*subscriber]struct{}{}}
	a := &subscriber{ch: make(chan []byte, 4)}
	b := &subscriber{ch: make(chan []byte, 4)}
	room.join(a)
	room.join(b)

	if !room.post([]byte("hello"), a) {
		t.Fatal("post refused")
	}
	select {
	case f := <-b.ch:
		if !strings.Contains(string(f), "aGVsbG8=") { // base64("hello")
			t.Fatalf("peer frame = %s", f)
		}
	default:
		t.Fatal("the other editor received nothing")
	}
	select {
	case f := <-a.ch:
		t.Fatalf("the sender was echoed its own update: %s", f)
	default:
	}
}

// A CRDT peer that misses an update is silently diverged, which is worse than
// a missed file notification: it is told to rebuild.
func TestCollabSlowEditorIsToldToResync(t *testing.T) {
	room := &collabRoom{subs: map[*subscriber]struct{}{}}
	slow := &subscriber{ch: make(chan []byte, 2)}
	room.join(slow)
	for i := 0; i < 10; i++ {
		room.post([]byte("x"), nil)
	}
	if !slow.lost.Load() {
		t.Fatal("an editor that overflowed was not marked lost")
	}
}

// The log is memory a member grows by typing, so it is bounded — and the
// answer to a full room is "everyone rebuild", not a silent truncation that
// would diverge every peer.
func TestCollabRoomIsBounded(t *testing.T) {
	room := &collabRoom{subs: map[*subscriber]struct{}{}}
	big := make([]byte, 1<<20)
	n := 0
	for room.post(big, nil) {
		n++
		if n > maxRoomBytes/len(big)+2 {
			t.Fatal("room accepted more than its byte cap")
		}
	}
	if room.bytes > maxRoomBytes {
		t.Fatalf("room holds %d bytes, cap is %d", room.bytes, maxRoomBytes)
	}
	room.reset()
	if room.bytes != 0 || len(room.updates) != 0 {
		t.Fatal("reset left the log behind")
	}
}

// An idle room is dropped; a room with someone in it never is.
func TestCollabIdleRoomsAreSwept(t *testing.T) {
	h := &collabHub{rooms: map[string]*collabRoom{}}
	stale := h.room("p\x00old.md")
	stale.touched = time.Now().Add(-2 * roomIdle)
	occupied := h.room("p\x00busy.md")
	occupied.touched = time.Now().Add(-2 * roomIdle)
	occupied.join(&subscriber{ch: make(chan []byte, 1)})

	h.room("p\x00trigger.md") // any join runs the sweep

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.rooms["p\x00old.md"]; ok {
		t.Error("an empty idle room was kept")
	}
	if _, ok := h.rooms["p\x00busy.md"]; !ok {
		t.Error("a room with an editor in it was swept")
	}
}

// The path is a room key and comes from the caller.
func TestCollabRefusesAnUnsafePath(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	for _, bad := range []string{"", "../../etc/passwd", "/abs"} {
		rec := do(t, h, "POST", "/api/p/"+p.ID+"/collab?path="+bad, map[string]any{"update": "AAA="})
		if rec.Code != 400 {
			t.Errorf("path %q: %d, want 400", bad, rec.Code)
		}
	}
}

// The relay never interprets an update, but it must not accept an unbounded
// or malformed one either.
func TestCollabRejectsBadUpdates(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	url := "/api/p/" + p.ID + "/collab?path=a.md"
	for name, body := range map[string]any{
		"not base64": map[string]any{"update": "!!!not-base64!!!"},
		"empty":      map[string]any{"update": ""},
		"oversized":  map[string]any{"update": strings.Repeat("A", (maxUpdateBytes+64)*2)},
	} {
		rec := do(t, h, "POST", url, body)
		if rec.Code != 400 {
			t.Errorf("%s: %d, want 400", name, rec.Code)
		}
	}
}

// The claim must be released if the client that made it leaves without
// typing: otherwise the room stays claimed but empty, and the next joiner
// opens a blank document and snapshots that emptiness over a real file.
func TestCollabSeedClaimIsReleasedByAnEditorWhoNeverTyped(t *testing.T) {
	room := &collabRoom{subs: map[*subscriber]struct{}{}}
	a := &subscriber{ch: make(chan []byte, 1)}
	if _, first := room.join(a); !first {
		t.Fatal("first joiner should seed")
	}
	room.leave(a) // opened the file, typed nothing, closed it

	b := &subscriber{ch: make(chan []byte, 1)}
	if _, first := room.join(b); !first {
		t.Fatal("after an empty room is abandoned the next joiner must seed")
	}
}

// But a room that HAS content stays claimed, so a joiner rebuilds from the
// log rather than seeding a second document over it.
func TestCollabSeedClaimSurvivesWhenTheLogHasContent(t *testing.T) {
	room := &collabRoom{subs: map[*subscriber]struct{}{}}
	a := &subscriber{ch: make(chan []byte, 4)}
	room.join(a)
	room.post([]byte("typed something"), a)
	room.leave(a)

	b := &subscriber{ch: make(chan []byte, 4)}
	log, first := room.join(b)
	if first {
		t.Fatal("a room with a log must not be re-seeded")
	}
	if len(log) != 1 {
		t.Fatalf("log = %v, want the one update", log)
	}
}

func TestCollabStreamSetsEventStreamHeaders(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/p/" + p.ID + "/collab?path=a.md")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("collab stream: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
}
