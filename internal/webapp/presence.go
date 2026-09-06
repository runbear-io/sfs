package webapp

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Who else is here. Clients heartbeat the path they are looking at; the hub
// keeps that in memory and fans the roster out on the change stream that
// already exists (events.go).
//
// Deliberately NOT persisted, and never a MetaStore repo: presence is true for
// fifteen seconds and then it is a lie, so writing it down would only create
// something to serve staler than the thing it describes. A hub restart empties
// it and the next heartbeat rebuilds it.
//
// It is walled by the same proj(PermRead) as the stream itself, so a roster
// only ever reaches members of the project it describes. What it publishes is
// a display name and a path — the same pair History already shows every member
// of a project — and nothing else: no device id, no share token, no IP.

const (
	// presenceTTL is how long a heartbeat vouches for someone. Clients beat
	// well inside it, so a missed beat is a network hiccup and not a
	// disappearance.
	presenceTTL = 15 * time.Second
	// maxPresencePerProject bounds one project's roster. Far above any real
	// team; it exists so the map cannot be grown without end by a member with
	// a script.
	maxPresencePerProject = 256
	// presencePathMax bounds the path a client claims to be looking at. It is
	// echoed to every other member, so it is untrusted text like any other.
	presencePathMax = 1024
)

type presenceEntry struct {
	name string
	path string
	seen time.Time
}

// person is one roster row as clients receive it.
type person struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

type presenceHub struct {
	mu sync.Mutex
	// project id -> actor key -> entry. The actor key is an account email (or
	// a device id on an auth-less hub); it is a MAP KEY only and is never
	// serialized — see roster.
	at map[string]map[string]presenceEntry
}

func (s *Server) presence() *presenceHub {
	s.presOnce.Do(func() {
		s.pres = &presenceHub{at: map[string]map[string]presenceEntry{}}
	})
	return s.pres
}

// mark records that actor is looking at path, and reports the roster and
// whether it changed.
//
// Expiry is lazy — computed here rather than by a sweeper goroutine. Nobody
// needs to be told a roster shrank except the people still in it, and they are
// exactly the ones still heartbeating, so the next beat notices within its own
// interval. A ticker would buy nothing and would have to be shut down.
func (h *presenceHub) mark(project, actor, name, path string, now time.Time) ([]person, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.at[project]
	if room == nil {
		room = map[string]presenceEntry{}
		h.at[project] = room
	}
	prev, existed := room[actor]
	changed := !existed || prev.path != path || prev.name != name
	for k, e := range room {
		if now.Sub(e.seen) > presenceTTL && k != actor {
			delete(room, k)
			changed = true
		}
	}
	if !existed && len(room) >= maxPresencePerProject {
		return rosterOf(room), false // full: this beat vouches for nobody
	}
	room[actor] = presenceEntry{name: name, path: path, seen: now}
	return rosterOf(room), changed
}

// drop removes an actor immediately, for a client that says it is leaving.
func (h *presenceHub) drop(project, actor string) ([]person, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.at[project]
	if room == nil {
		return nil, false
	}
	if _, ok := room[actor]; !ok {
		return rosterOf(room), false
	}
	delete(room, actor)
	if len(room) == 0 {
		delete(h.at, project)
	}
	return rosterOf(room), true
}

// rosterOf renders a room. The actor key never appears in the result: it is an
// email, and the roster goes to every member of the project.
func rosterOf(room map[string]presenceEntry) []person {
	out := make([]person, 0, len(room))
	for _, e := range room {
		out = append(out, person{Name: e.name, Path: e.path})
	}
	// Sorted so an unchanged roster serializes identically and the frontend's
	// structural sharing sees no update: Go map order is deliberately random.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// handlePresence serves POST {prefix}presence — one heartbeat.
func (s *Server) handlePresence(v *volume, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path  string `json:"path"`
		Leave bool   `json:"leave,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	u := s.requestUser(r)
	// The actor key identifies WHO, and the display name is what everyone
	// sees. An auth-less hub (the plain-folder viewer) has neither, so it
	// falls back to the device — and to nothing, in which case there is no
	// one to report and the beat is a no-op rather than a fake "someone".
	actor, name := u.Email, u.Name
	if actor == "" {
		actor = deviceID(r)
		name = actor
	}
	if actor == "" {
		writeJSON(w, map[string]any{"ok": true, "people": []person{}})
		return
	}
	if name == "" {
		name = u.Email // History shows the address too when there is no name
	}
	project := projectID(r)

	if req.Leave {
		people, changed := s.presence().drop(project, actor)
		if changed {
			s.events().publish(project, changeEvent{Type: "presence", People: people})
		}
		writeJSON(w, map[string]any{"ok": true, "people": people})
		return
	}

	// A claimed path is untrusted text echoed to every other member. It is
	// display-only — nothing is looked up by it — but it still goes through
	// the same rule every other path in this codebase does, rather than a
	// fourth private copy of "looks fine to me".
	path := req.Path
	if len(path) > presencePathMax || (path != "" && !journal.SafePath(path)) {
		path = ""
	}
	people, changed := s.presence().mark(project, actor, name, path, time.Now())
	if changed {
		s.events().publish(project, changeEvent{Type: "presence", People: people})
	}
	writeJSON(w, map[string]any{"ok": true, "people": people})
}
