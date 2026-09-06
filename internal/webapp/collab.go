package webapp

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Collaborative editing: two people typing in one document at the same time.
//
// The hub is a RELAY and an append-only update log. It never parses a Yjs
// update, never holds a document, and has no CRDT library linked into it —
// which is what keeps the build pure Go (a cgo y-crdt would break the
// cross-compiled release the same way a cgo sqlite would).
//
// Nothing here touches the journal. A room's updates are ephemeral; the
// DOCUMENT is still an ordinary file, written by an ordinary upload/content
// call from whichever client is holding the pen (Editor.tsx). So every
// desktop device, every agent and every older client sees exactly what they
// saw before: a path with a blob behind it. journal.Less and Replay are
// untouched, and a peer that has never heard of collab converges identically.
//
// The one piece of coordination the relay cannot avoid: a Yjs document seeded
// independently by two clients from the same text is NOT the same document —
// the items carry different ids, and merging them duplicates the text. So the
// room tells exactly one joiner that it is first, under the room lock, and
// everyone else rebuilds from the log that joiner produced.

const (
	// maxRoomBytes bounds one document's update log. Yjs updates are small
	// (a keystroke is tens of bytes) but a long session is unbounded, and
	// this is memory a member can grow by typing. Past it the room asks its
	// clients to snapshot and start over.
	maxRoomBytes = 8 << 20
	// roomIdle is how long a room with no subscribers is kept before its log
	// is dropped. The file is the durable copy — the log only has to outlive
	// a reload or a flaky connection.
	roomIdle = 10 * time.Minute
	// maxUpdateBytes bounds one POSTed update.
	maxUpdateBytes = 1 << 20
)

type collabRoom struct {
	mu      sync.Mutex
	updates [][]byte // opaque Yjs updates, in arrival order
	bytes   int
	subs    map[*subscriber]struct{}
	touched time.Time
	// seeded is CLAIMED at join, not inferred from the log being non-empty.
	// The log only fills once the seeding client has posted, and every joiner
	// arriving inside that window would otherwise be told to seed too — each
	// building a different Yjs document from the same text, which on merge
	// duplicates every character.
	seeded bool
}

type collabHub struct {
	mu    sync.Mutex
	rooms map[string]*collabRoom
}

func (s *Server) collab() *collabHub {
	s.colOnce.Do(func() { s.col = &collabHub{rooms: map[string]*collabRoom{}} })
	return s.col
}

// roomKey is (project, path). NUL-separated because it cannot appear in
// either: journal.SafePath refuses it in a path, and a project id is a
// restricted charset.
func roomKey(project, path string) string { return project + "\x00" + path }

func (h *collabHub) room(key string) *collabRoom {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepLocked()
	r := h.rooms[key]
	if r == nil {
		r = &collabRoom{subs: map[*subscriber]struct{}{}, touched: time.Now()}
		h.rooms[key] = r
	}
	return r
}

// sweepLocked drops rooms nobody has been in for roomIdle. Lazy, like
// presence expiry: the only thing that needs a room is a client arriving, and
// that is when this runs.
func (h *collabHub) sweepLocked() {
	now := time.Now()
	for k, r := range h.rooms {
		r.mu.Lock()
		idle := len(r.subs) == 0 && now.Sub(r.touched) > roomIdle
		r.mu.Unlock()
		if idle {
			delete(h.rooms, k)
		}
	}
}

// join adds a subscriber and reports the log so far, plus whether this client
// is the one that must seed the document from the file. Both under one lock:
// "am I first" and "here is what exists" have to be answered together or two
// clients both seed and the text doubles.
func (r *collabRoom) join(sub *subscriber) (log [][]byte, first bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs[sub] = struct{}{}
	r.touched = time.Now()
	first = !r.seeded
	r.seeded = true
	log = make([][]byte, len(r.updates))
	copy(log, r.updates)
	return log, first
}

func (r *collabRoom) leave(sub *subscriber) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.subs, sub)
	r.touched = time.Now()
	// The claim is released when the last editor leaves WITHOUT having posted
	// anything. Otherwise a client that opened the document and closed it
	// before typing would leave the room permanently claimed but empty, and
	// the next joiner — told it is not first — would open a blank document
	// and then snapshot that emptiness over the file.
	if len(r.subs) == 0 && len(r.updates) == 0 {
		r.seeded = false
	}
}

// post records an update and fans it out. Returns false when the room is full,
// which the client turns into "snapshot and rejoin".
func (r *collabRoom) post(update []byte, from *subscriber) bool {
	r.mu.Lock()
	if r.bytes+len(update) > maxRoomBytes {
		r.mu.Unlock()
		return false
	}
	r.updates = append(r.updates, update)
	r.bytes += len(update)
	r.touched = time.Now()
	peers := make([]*subscriber, 0, len(r.subs))
	for sub := range r.subs {
		if sub != from { // the sender already has it
			peers = append(peers, sub)
		}
	}
	r.mu.Unlock()

	frame, err := json.Marshal(collabFrame{Type: "update", Update: b64(update)})
	if err != nil {
		return true
	}
	for _, sub := range peers {
		select {
		case sub.ch <- frame:
		default:
			// A client that cannot keep up with a document's edits has lost
			// the thread of it: dropping one update is not recoverable for a
			// CRDT peer the way a dropped file-change notification is, so it
			// is told to rebuild rather than left silently diverged.
			sub.lost.Store(true)
		}
	}
	return true
}

// relay fans a frame out without recording it. Used for awareness, which is
// true for a moment and then is not.
func (r *collabRoom) relay(ev collabFrame) {
	frame, err := json.Marshal(ev)
	if err != nil {
		return
	}
	r.mu.Lock()
	peers := make([]*subscriber, 0, len(r.subs))
	for sub := range r.subs {
		peers = append(peers, sub)
	}
	r.touched = time.Now()
	r.mu.Unlock()
	for _, sub := range peers {
		select {
		case sub.ch <- frame:
		default:
			// A dropped cursor position is not worth a resync: the next one
			// is along in a moment and corrects it.
		}
	}
}

// reset empties a full room so the clients that just snapshotted can rebuild
// it from the file.
func (r *collabRoom) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates, r.bytes = nil, 0
	r.seeded = false // the log is gone; someone has to rebuild from the file
	r.touched = time.Now()
}

type collabFrame struct {
	Type string `json:"type"` // "hello" | "update" | "awareness" | "resync"
	// Seed is set on hello: this client must build the document from the
	// file's current text, because the room is empty and someone has to.
	Seed bool `json:"seed,omitempty"`
	// Log is the room's updates so far, on hello for a non-seeding client.
	Log []string `json:"log,omitempty"`
	// Update is one Yjs update, base64. The hub never looks inside it.
	Update string `json:"update,omitempty"`
	// Awareness is a cursor/selection/identity update. Relayed but NEVER
	// logged: it describes where someone's caret is this second, so replaying
	// it to a joiner would paint cursors for people who have left, and storing
	// it would grow the room without bound for something with no history.
	Awareness string `json:"awareness,omitempty"`
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// handleCollabStream serves GET {prefix}collab?path= — the document's update
// stream. PermWrite, not PermRead: this is the editing channel, and a
// read-only member has nothing to send on it.
func (s *Server) handleCollabStream(v *volume, w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" || !journal.SafePath(path) {
		http.Error(w, "collab needs a valid path", http.StatusBadRequest)
		return
	}
	rc := http.NewResponseController(w)
	key := roomKey(projectID(r), path)
	room := s.collab().room(key)

	sub, ok := s.events().subscribe("collab:" + key)
	if !ok {
		http.Error(w, "too many editing sessions open; try again shortly", http.StatusServiceUnavailable)
		return
	}
	defer s.events().unsubscribe("collab:"+key, sub)
	log, first := room.join(sub)
	defer room.leave(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Content-Encoding", "identity")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	hello := collabFrame{Type: "hello", Seed: first}
	for _, u := range log {
		hello.Log = append(hello.Log, b64(u))
	}
	frame, err := json.Marshal(hello)
	if err != nil || !writeFrame(w, rc, frame) {
		return
	}

	tick := time.NewTicker(keepalive)
	defer tick.Stop()
	old := time.NewTimer(streamMaxAge)
	defer old.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-old.C:
			return
		case f := <-sub.ch:
			if sub.lost.Swap(false) {
				if !writeFrame(w, rc, []byte(`{"type":"resync"}`)) {
					return
				}
			}
			if !writeFrame(w, rc, f) {
				return
			}
		case <-tick.C:
			if sub.lost.Swap(false) {
				if !writeFrame(w, rc, []byte(`{"type":"resync"}`)) {
					return
				}
				continue
			}
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// handleCollabPost serves POST {prefix}collab?path= — one Yjs update from an
// editor, relayed to everyone else in the document.
func (s *Server) handleCollabPost(v *volume, w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" || !journal.SafePath(path) {
		http.Error(w, "collab needs a valid path", http.StatusBadRequest)
		return
	}
	var req struct {
		Update    string `json:"update"`
		Awareness string `json:"awareness"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxUpdateBytes*2)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Awareness is the ephemeral half: broadcast to the room, never stored.
	if req.Awareness != "" {
		aw, err := base64.StdEncoding.DecodeString(req.Awareness)
		if err != nil || len(aw) == 0 || len(aw) > maxUpdateBytes {
			http.Error(w, "awareness must be base64 and under the size limit", http.StatusBadRequest)
			return
		}
		s.collab().room(roomKey(projectID(r), path)).relay(collabFrame{
			Type: "awareness", Awareness: b64(aw),
		})
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	update, err := base64.StdEncoding.DecodeString(req.Update)
	if err != nil || len(update) == 0 || len(update) > maxUpdateBytes {
		http.Error(w, "update must be base64 and under the size limit", http.StatusBadRequest)
		return
	}
	room := s.collab().room(roomKey(projectID(r), path))
	if !room.post(update, nil) {
		room.reset()
		writeJSON(w, map[string]any{"ok": true, "full": true})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
