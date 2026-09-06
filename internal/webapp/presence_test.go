package webapp

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPresenceMarkAndExpire(t *testing.T) {
	h := &presenceHub{at: map[string]map[string]presenceEntry{}}
	t0 := time.Now()

	people, changed := h.mark("p1", "a@x.io", "Alice", "index.md", t0)
	if !changed || len(people) != 1 || people[0].Name != "Alice" {
		t.Fatalf("first beat: people=%+v changed=%v", people, changed)
	}
	// A repeat beat on the same path is not news — otherwise every client
	// would wake every other client every heartbeat, forever.
	if _, changed = h.mark("p1", "a@x.io", "Alice", "index.md", t0.Add(time.Second)); changed {
		t.Fatal("an unchanged beat reported a change")
	}
	// Moving to another file is.
	if _, changed = h.mark("p1", "a@x.io", "Alice", "guide.md", t0.Add(2*time.Second)); !changed {
		t.Fatal("moving to another path reported no change")
	}

	h.mark("p1", "b@x.io", "Bob", "guide.md", t0.Add(3*time.Second))
	// Bob goes quiet; Alice's next beat is what notices.
	people, changed = h.mark("p1", "a@x.io", "Alice", "guide.md", t0.Add(3*time.Second+presenceTTL+time.Second))
	if !changed {
		t.Fatal("an expiry reported no change")
	}
	if len(people) != 1 || people[0].Name != "Alice" {
		t.Fatalf("after Bob expired: %+v", people)
	}
}

func TestPresenceDrop(t *testing.T) {
	h := &presenceHub{at: map[string]map[string]presenceEntry{}}
	now := time.Now()
	h.mark("p1", "a@x.io", "Alice", "index.md", now)
	h.mark("p1", "b@x.io", "Bob", "index.md", now)

	people, changed := h.drop("p1", "a@x.io")
	if !changed || len(people) != 1 || people[0].Name != "Bob" {
		t.Fatalf("drop: people=%+v changed=%v", people, changed)
	}
	// Dropping twice is not a change, and not an error.
	if _, changed = h.drop("p1", "a@x.io"); changed {
		t.Fatal("dropping an absent actor reported a change")
	}
}

// The roster is keyed by account email and goes to every member of the
// project. The key must never be in what they receive.
func TestPresenceRosterCarriesNoAccountKey(t *testing.T) {
	h := &presenceHub{at: map[string]map[string]presenceEntry{}}
	people, _ := h.mark("p1", "secret-address@x.io", "Alice", "index.md", time.Now())
	blob, err := json.Marshal(people)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "secret-address@x.io") {
		t.Fatalf("the roster carried the actor key: %s", blob)
	}
}

func TestPresenceBounded(t *testing.T) {
	h := &presenceHub{at: map[string]map[string]presenceEntry{}}
	now := time.Now()
	for i := 0; i < maxPresencePerProject; i++ {
		h.mark("p1", string(rune('a'+i%26))+strings.Repeat("x", i), "n", "p", now)
	}
	before := len(h.at["p1"])
	h.mark("p1", "one-too-many@x.io", "Nope", "p", now)
	if len(h.at["p1"]) > before {
		t.Fatalf("roster grew past the cap: %d", len(h.at["p1"]))
	}
}

// Rendering must be stable: Go map order is random, and an unstable roster
// would look like a change to every client on every beat.
func TestPresenceRosterIsSorted(t *testing.T) {
	h := &presenceHub{at: map[string]map[string]presenceEntry{}}
	now := time.Now()
	for _, n := range []string{"Carol", "Alice", "Bob"} {
		h.mark("p1", n+"@x.io", n, "index.md", now)
	}
	var first string
	for i := 0; i < 20; i++ {
		people, _ := h.mark("p1", "Alice@x.io", "Alice", "index.md", now)
		blob, _ := json.Marshal(people)
		if i == 0 {
			first = string(blob)
		} else if string(blob) != first {
			t.Fatalf("roster order is unstable:\n%s\n%s", first, blob)
		}
	}
}

// An auth-less hub has no account and no device: there is nobody to report,
// and inventing "someone" would put a phantom on every teammate's screen.
func TestPresenceAnonymousBeatReportsNobody(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()
	rec := do(t, h, "POST", "/api/p/"+p.ID+"/presence", map[string]any{"path": "index.md"})
	if rec.Code != 200 {
		t.Fatalf("presence: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		People []person `json:"people"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.People) != 0 {
		t.Fatalf("an anonymous beat put someone on the roster: %+v", out.People)
	}
}

// A path is echoed to every other member, so a hostile one is dropped rather
// than relayed.
func TestPresenceRefusesAnUnsafePath(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	srv.Auth = gatedAuth(t, nil)
	h := srv.Handler()
	for _, bad := range []string{"../../etc/passwd", "/abs/path", strings.Repeat("a", presencePathMax+1)} {
		req := httptest.NewRequest("POST", "/api/p/"+p.ID+"/presence",
			strings.NewReader(`{"path":`+quoteJSON(bad)+`}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// Unauthenticated here, so the roster is empty either way — what is
		// being pinned is that it does not 500 and does not echo the path.
		if rec.Code >= 500 {
			t.Fatalf("path %q: %d %s", bad, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "etc/passwd") {
			t.Fatalf("echoed an unsafe path: %s", rec.Body)
		}
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
