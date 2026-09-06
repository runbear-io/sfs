package webapp

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Live change notification: the hub tells its clients what changed instead of
// making them ask.
//
// Without this a teammate's edit surfaces only on the frontend's 15s tree poll
// — and an OPEN file body never refreshes at all, because file content has no
// refetch interval (hooks/useBlob.ts). A device is worse off still: it waits
// out the daemon's 10s remote gate, so an edit takes ~10s to cross two
// machines in the typical case.
//
// The transport is server-sent events, not a websocket: the traffic is
// one-directional (the hub talks, clients listen), SSE needs no new dependency
// and no upgrade handshake, and browsers reconnect on their own. Clients that
// want to talk back use ordinary POSTs.
//
// Delivery is best-effort by construction. publish() sits on the sync push
// path, so it must never block and never fail a write: a subscriber that
// cannot keep up is marked lost and told to resync rather than waited for.
// The frontend keeps its 15s tree poll as the floor under all of this.

const (
	// subBuffer is how many frames a slow client may fall behind before it is
	// told to resync instead. Small on purpose: the resync is one refetch, and
	// holding a long queue for a tab that has stopped reading buys nothing.
	subBuffer = 32
	// maxSubsPerProject bounds one project's fan-out, maxSubsTotal the hub's.
	// A browser holds one stream per open tab, a device one per mount, so
	// these are far above any real deployment and exist only so an
	// unauthenticated-adjacent surface cannot be made to allocate without end.
	maxSubsPerProject = 256
	maxSubsTotal      = 4096
	// eventPathLimit caps how many paths one frame names. A sync push can
	// carry thousands of ops; past this the frame says "more" and the client
	// refetches wholesale, which is cheaper than the frame would have been.
	eventPathLimit = 64
	// keepalive keeps intermediaries from reaping an idle stream. A comment
	// line is not an event, so clients never see it.
	keepalive = 20 * time.Second
	// streamMaxAge ends a stream on purpose after an hour. Both clients
	// re-dial (EventSource on its own, the daemon on a closed channel), so
	// this costs nothing — and without it the only thing that ever frees a
	// subscriber slot is the client hanging up, which a half-open connection
	// never does. maxSubsTotal is a bound on concurrent streams, not on
	// abandoned ones.
	streamMaxAge = time.Hour
)

// changeEvent is one "something changed" frame. Paths are relative to the
// project root, exactly as the journal spells them.
type changeEvent struct {
	Type string `json:"type"` // "change" | "resync" | "presence"
	// People is the project's roster, on a "presence" frame only. Display
	// names and paths — never the account key they are stored under.
	People  []person `json:"people,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	More    bool     `json:"more,omitempty"` // paths were truncated; refetch everything
	Puts    int      `json:"puts,omitempty"`
	Deletes int      `json:"deletes,omitempty"`
	Source  string   `json:"source,omitempty"` // "sync" | "browser"
	// Device is the device that pushed the change, so a client can tell its
	// own writes from a peer's. It is the id the pusher named in its own
	// header — display-grade, exactly like the one History already shows
	// every project member — never an authorization input.
	Device string `json:"device,omitempty"`
}

type subscriber struct {
	ch chan []byte
	// lost is set when a frame could not be queued. The writer turns it into
	// a single resync frame: a client that missed one change and a client
	// that missed fifty need the same thing, which is to refetch.
	lost atomic.Bool
}

type eventHub struct {
	mu    sync.Mutex
	subs  map[string]map[*subscriber]struct{} // project id ("" in single-volume mode)
	total int
}

// events returns the hub's fan-out, built on first use. Servers are assembled
// field by field (fixtures, a hub rebuilt from its repos), so there is no
// constructor to do this in.
func (s *Server) events() *eventHub {
	s.evOnce.Do(func() {
		s.ev = &eventHub{subs: map[string]map[*subscriber]struct{}{}}
	})
	return s.ev
}

// subscribe registers a listener for one project, or reports that the hub is
// already carrying as many streams as it will.
func (h *eventHub) subscribe(project string) (*subscriber, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.total >= maxSubsTotal || len(h.subs[project]) >= maxSubsPerProject {
		return nil, false
	}
	sub := &subscriber{ch: make(chan []byte, subBuffer)}
	if h.subs[project] == nil {
		h.subs[project] = map[*subscriber]struct{}{}
	}
	h.subs[project][sub] = struct{}{}
	h.total++
	return sub, true
}

func (h *eventHub) unsubscribe(project string, sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.subs[project]; m != nil {
		if _, ok := m[sub]; ok {
			delete(m, sub)
			h.total--
		}
		if len(m) == 0 {
			delete(h.subs, project)
		}
	}
}

// publish fans one frame out to a project's listeners. It never blocks and
// never returns an error: it is called from write handlers, and a client that
// has stopped reading is not a reason to fail somebody's sync push.
func (h *eventHub) publish(project string, ev changeEvent) {
	frame, err := json.Marshal(ev)
	if err != nil {
		return // a struct of strings and ints; unreachable, and not worth a log
	}
	h.mu.Lock()
	subs := make([]*subscriber, 0, len(h.subs[project]))
	for sub := range h.subs[project] {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	// Sending outside the lock: a full channel is handled by the default arm,
	// but holding the hub lock across a fan-out to thousands of listeners
	// would put every writer behind the slowest one.
	for _, sub := range subs {
		select {
		case sub.ch <- frame:
		default:
			sub.lost.Store(true)
		}
	}
}

// publishChange announces a write to a project's listeners.
//
// Deliberately NOT folded into captureChange, which is the analytics path:
// that function's contract is that "nothing here carries a path or a file
// name" (it feeds PostHog), and its signature is counts-only. The two want
// the same call sites and nothing else.
func (s *Server) publishChange(r *http.Request, source string, paths []string, puts, deletes int) {
	if puts+deletes == 0 {
		return
	}
	ev := changeEvent{
		Type: "change", Puts: puts, Deletes: deletes,
		Source: source, Device: deviceID(r),
	}
	if len(paths) > eventPathLimit {
		paths, ev.More = paths[:eventPathLimit], true
	}
	ev.Paths = paths
	s.events().publish(projectID(r), ev)
}

// newOpPaths is countOps' sibling: the paths of the ops this push actually
// added, in journal order. Same storedMax filter — an op the hub already had
// is not news. Deduped, because a push that rewrote one file ten times is one
// path as far as a client refetch is concerned.
func newOpPaths(ops []journal.Op, storedMax int64) []string {
	seen := map[string]bool{}
	var out []string
	for _, op := range ops {
		if op.Seq <= storedMax || seen[op.Path] {
			continue
		}
		seen[op.Path] = true
		out = append(out, op.Path)
	}
	return out
}

func (s *Server) handleEvents(v *volume, w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	project := projectID(r)
	sub, ok := s.events().subscribe(project)
	if !ok {
		http.Error(w, "too many event streams open; try again shortly", http.StatusServiceUnavailable)
		return
	}
	defer s.events().unsubscribe(project, sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	// The frames are small and the stream is long-lived: gzip here buffers
	// and defeats the entire point of pushing.
	h.Set("Content-Encoding", "identity")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // nginx and friends
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return // not a streaming-capable writer; nothing to do but leave
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
		case frame := <-sub.ch:
			if sub.lost.Swap(false) {
				// Frames were dropped between this one and the last: what the
				// client missed is unknowable, so say so instead of guessing.
				if !writeFrame(w, rc, []byte(`{"type":"resync"}`)) {
					return
				}
			}
			if !writeFrame(w, rc, frame) {
				return
			}
		case <-tick.C:
			// The resync also rides the keepalive, not just the next frame:
			// a client that overflowed and then saw the project go quiet
			// would otherwise stay stale until its stream aged out, since
			// the thing that tells it to refetch is the very traffic that
			// has stopped.
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

func writeFrame(w http.ResponseWriter, rc *http.ResponseController, frame []byte) bool {
	if _, err := w.Write([]byte("data: ")); err != nil {
		return false
	}
	if _, err := w.Write(frame); err != nil {
		return false
	}
	if _, err := w.Write([]byte("\n\n")); err != nil {
		return false
	}
	return rc.Flush() == nil
}
