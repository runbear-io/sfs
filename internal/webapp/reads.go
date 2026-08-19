package webapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Read telemetry: who consumes what, aggregated. Together with the write
// history the journals already carry, this completes the read×write matrix —
// heavily-read + long-unwritten is the danger zone an admin should fix first
// (see docs/design/read-heatmap.md).
//
// What counts as a read: viewer file/render/download hits (human), share-link
// hits (share), and agent tool reads reported by syncing devices (agent).
// /store/* sync traffic is replication, not reading, and is never counted;
// history /blob views are spelunking, not consumption, and aren't either.
//
// One exception to "never an event log": session reads (SessionRead), the
// per-session detail behind a History run card. They are a separate table
// with their own, shorter retention, and they never enter the ledger's
// in-memory bucket map — see SessionReadRepo for why that separation is the
// whole point.
//
// Privacy: rows are daily aggregation buckets, never an event log. The actor
// column (account email / device id / share token) exists only to count
// distinct readers and never appears in an API response — with exactly one
// stated exception, ?by=device (handleHeat), which reports the ids of agent
// devices. A device id is not a person and is already visible to every
// project member through History's device join; nothing else in the column —
// no email, no share token, no unowned device id — ever leaves the server.
// The exception is only sound because the ingest path validates the id
// (handleReadReport → ownsDevice): an actor recorded as an agent is shaped
// like a device id and is not one another account is syncing — never an
// arbitrary string, and never someone else's machine. This route also never
// registers a device: registering the id it is about to judge is what turned
// the round-2 check into a one-request speed bump.
//
// A session id is identity-adjacent and gets the same ruling, written down
// before anything serves it: a session id appears ONLY in History responses,
// on the op that carries it, and as a ?session= filter INPUT. It is never
// enumerated — no "list sessions" response, no session column in /heat's
// output, nothing new in ?by=device. That is sound because the id is already
// visible to every project member inside Op.Note today, so serving it as its
// own field discloses nothing new, while refusing to enumerate keeps /heat
// identity-free exactly as documented above.

// Read kinds.
const (
	ReadKindHuman = "human"
	ReadKindAgent = "agent"
	ReadKindShare = "share"
)

// ReadStat is one aggregation bucket: reads of one path by one actor on one
// day. Day == "" is the all-time fold that survives retention.
type ReadStat struct {
	Project string    `json:"project"`
	Path    string    `json:"path"`
	Day     string    `json:"day"` // "2006-01-02" UTC, or "" for all-time
	Kind    string    `json:"kind"`
	Actor   string    `json:"actor"`
	Count   int64     `json:"count"`
	Last    time.Time `json:"last"`
}

// ReadStatKey identifies one bucket.
type ReadStatKey struct {
	Project, Path, Day, Kind, Actor string
}

func (s ReadStat) key() ReadStatKey {
	return ReadStatKey{s.Project, s.Path, s.Day, s.Kind, s.Actor}
}

// SessionRead records that one agent session read one path, from one device.
// Not a count and not a bucket: the run card asks "did this session read this
// file?", and one row per (session, device, path) answers it with no
// aggregation. Device is always the hub-validated device the report arrived
// from, never anything the client put in the body.
type SessionRead struct {
	Project string    `json:"project"`
	Session string    `json:"session"`
	Device  string    `json:"device"`
	Path    string    `json:"path"`
	Last    time.Time `json:"last"`
}

type sessionReadKey struct{ Project, Session, Device, Path string }

func (s SessionRead) key() sessionReadKey {
	return sessionReadKey{s.Project, s.Session, s.Device, s.Path}
}

// HeatEntry is the per-path aggregate the heat API returns. Counts only —
// never identities.
type HeatEntry struct {
	Human    int64     `json:"human,omitempty"`
	Agent    int64     `json:"agent,omitempty"`
	Share    int64     `json:"share,omitempty"`
	Readers  int       `json:"readers,omitempty"` // distinct human readers
	LastRead time.Time `json:"last_read,omitzero"`
}

const (
	// readDebounce collapses request storms (reloads, render-then-raw double
	// fetches) into visits: repeat reads of a path by the same actor within
	// the window don't count again.
	readDebounce = 10 * time.Minute
	// readFlushEvery throttles persistence; dirty buckets ride in memory
	// between flushes, so a crash loses at most this much telemetry.
	readFlushEvery = 30 * time.Second
	// DefaultReadRetentionDays is how long daily buckets keep per-day
	// resolution before folding into the all-time row.
	DefaultReadRetentionDays = 400
	// DefaultSessionRetentionDays is how long per-session read detail is
	// kept. Much shorter than the bucket retention: this is event-shaped
	// data whose only consumer is a History run card, and a month covers a
	// retro. Rows past it are deleted, not folded — the heat totals were
	// never derived from them, so nothing is lost from any count.
	DefaultSessionRetentionDays = 30
	// sessionPruneEvery throttles the retention delete; it rides the same
	// flush the buckets use rather than owning a goroutine.
	sessionPruneEvery = time.Hour
)

// ReadLedger is the in-memory read-telemetry service over a ReadRepo, in the
// mold of DeviceRegistry: reads stay in memory, writes are throttled. There is
// no background goroutine — flushes piggyback on Record calls, and telemetry
// failures never surface to the request that triggered them.
type ReadLedger struct {
	repo      ReadRepo
	retention time.Duration

	// Session-read detail, optional (nil = off) and deliberately outside
	// byKey: these rows are never loaded into memory in bulk, so Heat's full
	// map scan and the boot load are unaffected by session cardinality. If a
	// future change ever moves them into byKey, Heat (below) is what pays.
	sessions         SessionReadRepo
	sessionRetention time.Duration

	// scans counts ShareOpens passes over byKey. Tests assert one per
	// project per list render — the "never one scan per share" rule is
	// invisible in the response body, so this is the only thing that can
	// catch the regression.
	scans atomic.Int64

	mu           sync.Mutex
	byKey        map[ReadStatKey]ReadStat
	dirty        map[ReadStatKey]bool
	pendingDel   []ReadStatKey             // retention deletions awaiting a successful flush
	seen         map[ReadStatKey]time.Time // debounce; Day field unused ("")
	pendingSess  map[sessionReadKey]SessionRead
	lastFlush    time.Time
	lastSessPrun time.Time
	warned       bool
	sessWarned   bool
}

// NewReadLedger loads the ledger and immediately folds buckets older than the
// retention horizon into their all-time rows. retentionDays <= 0 means the
// default.
func NewReadLedger(repo ReadRepo, retentionDays int) (*ReadLedger, error) {
	if retentionDays <= 0 {
		retentionDays = DefaultReadRetentionDays
	}
	l := &ReadLedger{
		repo:      repo,
		retention: time.Duration(retentionDays) * 24 * time.Hour,
		byKey:     map[ReadStatKey]ReadStat{},
		dirty:     map[ReadStatKey]bool{},
		seen:      map[ReadStatKey]time.Time{},
		lastFlush: time.Now(),
	}
	stats, err := repo.Load()
	if err != nil {
		return nil, err
	}
	for _, st := range stats {
		l.byKey[st.key()] = st
	}
	// Fold anything past the retention horizon right away. A failed persist
	// is not a boot failure — the fold stays dirty and later flushes retry.
	l.mu.Lock()
	l.compactLocked()
	if err := l.persistLocked(); err != nil {
		log.Printf("beardrive: read telemetry compact failed (will retry): %v", err)
	}
	l.mu.Unlock()
	return l, nil
}

// OpenReadLedger loads the file-backed ledger at path.
func OpenReadLedger(path string, retentionDays int) (*ReadLedger, error) {
	return NewReadLedger(newFileReadRepo(path), retentionDays)
}

// OpenSessionReadRepo is the file-backed session-read store, for hubs
// running without a MetaStore (the historical JSON-files layout).
func OpenSessionReadRepo(path string) SessionReadRepo { return newFileSessionReadRepo(path) }

// WithSessions turns on per-session read detail (the data behind a History
// run card). Separate from the constructor so every existing caller — and
// every backend that has no session repo — keeps working with it off.
// retentionDays <= 0 means the default.
func (l *ReadLedger) WithSessions(repo SessionReadRepo, retentionDays int) *ReadLedger {
	if l == nil || repo == nil {
		return l
	}
	if retentionDays <= 0 {
		retentionDays = DefaultSessionRetentionDays
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessions = repo
	l.sessionRetention = time.Duration(retentionDays) * 24 * time.Hour
	l.pendingSess = map[sessionReadKey]SessionRead{}
	return l
}

// Record counts one read. Nil-safe and never fails: telemetry must not break
// the page view (or sync cycle) that triggered it.
func (l *ReadLedger) Record(project, path, kind, actor string) {
	if l == nil || project == "" || path == "" {
		return
	}
	now := time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	visit := ReadStatKey{Project: project, Path: path, Kind: kind, Actor: actor}
	if t, ok := l.seen[visit]; ok && now.Sub(t) < readDebounce {
		return
	}
	l.seen[visit] = now
	key := visit
	key.Day = now.Format("2006-01-02")
	st := l.byKey[key]
	st.Project, st.Path, st.Day, st.Kind, st.Actor = project, path, key.Day, kind, actor
	st.Count++
	st.Last = now
	l.byKey[key] = st
	l.dirty[key] = true
	if now.Sub(l.lastFlush) >= readFlushEvery {
		l.flushLocked()
	}
}

// RecordSession notes that one agent session read one path from one device.
// Nil-safe, off when no session repo is configured, and — like Record —
// never fails: telemetry must not break the sync cycle that reported it.
// Unlike Record it is NOT debounced: a row is a fact ("this session read this
// file"), not a count, so repeats are the same row rewritten.
func (l *ReadLedger) RecordSession(project, session, device, path string) {
	if l == nil || project == "" || session == "" || device == "" || path == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sessions == nil {
		return
	}
	now := time.Now()
	sr := SessionRead{Project: project, Session: session, Device: device, Path: path, Last: now.UTC()}
	l.pendingSess[sr.key()] = sr
	// Same throttle the buckets use. Record's own flush check sits behind its
	// debounce return, so a report whose buckets are all debounced would
	// otherwise leave these rows buffered indefinitely.
	if now.Sub(l.lastFlush) >= readFlushEvery {
		l.flushLocked()
	}
}

// SessionPaths returns the paths one session read from one device, for the
// History run card. Both the session and the device are required by the
// caller (handleHeat): a session-only lookup would return rows a member
// reported under someone else's session id, which pinning to the validated
// device is what makes harmless.
func (l *ReadLedger) SessionPaths(project, session, device string) []string {
	if l == nil || project == "" || session == "" || device == "" {
		return nil
	}
	l.mu.Lock()
	repo := l.sessions
	// Flush first, so a card opened seconds after a sync sees that sync's
	// reads instead of an empty list.
	if repo != nil {
		l.flushSessionsLocked()
	}
	l.mu.Unlock()
	if repo == nil {
		return nil
	}
	rows, err := repo.ListBySession(project, session, device)
	if err != nil {
		log.Printf("beardrive: session reads lookup failed: %v", err)
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Path)
	}
	return out
}

// flushSessionsLocked persists buffered session rows and, at most hourly,
// deletes the ones past the session retention. Failures keep the buffer for
// the next attempt and log once — a read_sessions failure must never affect
// read_stats, so this is deliberately separate from persistLocked. Callers
// hold mu.
func (l *ReadLedger) flushSessionsLocked() {
	if l.sessions == nil {
		return
	}
	if len(l.pendingSess) > 0 {
		batch := make([]SessionRead, 0, len(l.pendingSess))
		for _, sr := range l.pendingSess {
			batch = append(batch, sr)
		}
		if err := l.sessions.PutBatch(batch); err != nil {
			if !l.sessWarned {
				l.sessWarned = true
				log.Printf("beardrive: session read flush failed (will retry): %v", err)
			}
		} else {
			l.sessWarned = false
			l.pendingSess = map[sessionReadKey]SessionRead{}
		}
	}
	now := time.Now()
	if now.Sub(l.lastSessPrun) < sessionPruneEvery {
		return
	}
	l.lastSessPrun = now
	if err := l.sessions.PruneBefore(now.UTC().Add(-l.sessionRetention)); err != nil {
		log.Printf("beardrive: session read prune failed (will retry): %v", err)
	}
}

// Heat aggregates reads per path for one project. since bounds the window
// (zero = all time, including retention folds); prefix "" means the whole
// project, otherwise paths under "<prefix>/".
func (l *ReadLedger) Heat(project, prefix string, since time.Time) map[string]HeatEntry {
	if l == nil {
		return nil
	}
	sinceDay := ""
	if !since.IsZero() {
		sinceDay = since.UTC().Format("2006-01-02")
	}
	prefix = strings.TrimSuffix(prefix, "/")
	out := map[string]HeatEntry{}
	humans := map[string]map[string]bool{} // path → distinct human actors
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, st := range l.byKey {
		if key.Project != project {
			continue
		}
		if prefix != "" && !strings.HasPrefix(key.Path, prefix+"/") {
			continue
		}
		if key.Day == "" {
			if sinceDay != "" {
				continue // all-time fold is older than any windowed query
			}
		} else if key.Day < sinceDay {
			continue
		}
		e := out[key.Path]
		switch key.Kind {
		case ReadKindHuman:
			e.Human += st.Count
			set := humans[key.Path]
			if set == nil {
				set = map[string]bool{}
				humans[key.Path] = set
			}
			set[key.Actor] = true
		case ReadKindAgent:
			e.Agent += st.Count
		case ReadKindShare:
			e.Share += st.Count
		}
		if st.Last.After(e.LastRead) {
			e.LastRead = st.Last
		}
		out[key.Path] = e
	}
	for p, set := range humans {
		e := out[p]
		e.Readers = len(set)
		out[p] = e
	}
	return out
}

// AgentHeat aggregates agent reads per device per top-level folder ("" for
// root files) — the coverage-matrix data. Agent buckets only, by design:
// agent actors are device ids, which history already exposes; human actors
// (emails) must never leave the server, so human/share buckets are not
// consulted at all.
func (l *ReadLedger) AgentHeat(project string, since time.Time) map[string]map[string]int64 {
	if l == nil {
		return nil
	}
	sinceDay := ""
	if !since.IsZero() {
		sinceDay = since.UTC().Format("2006-01-02")
	}
	out := map[string]map[string]int64{}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, st := range l.byKey {
		if key.Project != project || key.Kind != ReadKindAgent {
			continue
		}
		if key.Day == "" {
			if sinceDay != "" {
				continue
			}
		} else if key.Day < sinceDay {
			continue
		}
		folder := ""
		if i := strings.IndexByte(key.Path, '/'); i >= 0 {
			folder = key.Path[:i]
		}
		m := out[key.Actor]
		if m == nil {
			m = map[string]int64{}
			out[key.Actor] = m
		}
		m[folder] += st.Count
	}
	return out
}

// ShareOpen is share-link consumption for one path: visits, and when.
type ShareOpen struct {
	Count int64
	Last  time.Time
}

// ShareOpens aggregates share-kind reads per path for one project — the
// receipt a person who shared something actually wants. All-time, because a
// link's lifetime is the question a receipt answers.
//
// Share buckets only, and that is what makes Last mean *last opened*:
// HeatEntry.LastRead is cross-kind, so a member viewing the file in the hub
// would otherwise move the "opened through the link" date.
//
// Counts, never identities — the share actor is token+"/"+IP+"/"+UA hash, a
// public credential joined to a network and a browser, and it must not leave
// the ledger. There is deliberately no distinct-openers field.
//
// One byKey scan per project, never one per share: callers build this map
// once and index it, because byKey is the full map and a project with 40
// links would otherwise pay 40 full scans per list render.
func (l *ReadLedger) ShareOpens(project string) map[string]ShareOpen {
	if l == nil {
		return nil // reads disabled: absent, not zero
	}
	l.scans.Add(1)
	out := map[string]ShareOpen{}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, st := range l.byKey {
		if key.Project != project || key.Kind != ReadKindShare {
			continue
		}
		// No day filter: both the daily buckets and the folded Day == ""
		// all-time row count.
		e := out[key.Path]
		e.Count += st.Count
		if st.Last.After(e.Last) {
			e.Last = st.Last
		}
		out[key.Path] = e
	}
	return out
}

// Close flushes any pending buckets.
func (l *ReadLedger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
	if n := len(l.dirty); n > 0 {
		return fmt.Errorf("read ledger: flush failed, %d buckets pending", n)
	}
	return nil
}

// flushLocked persists dirty buckets (and, once a day, retention folds),
// pruning the debounce map along the way. Failures keep the buckets dirty for
// the next attempt and log once — telemetry never breaks a request.
func (l *ReadLedger) flushLocked() {
	now := time.Now()
	l.lastFlush = now
	for k, t := range l.seen {
		if now.Sub(t) >= readDebounce {
			delete(l.seen, k)
		}
	}
	l.compactLocked()
	if err := l.persistLocked(); err != nil {
		if !l.warned {
			l.warned = true
			log.Printf("beardrive: read telemetry flush failed (will retry): %v", err)
		}
	} else {
		l.warned = false
	}
	l.flushSessionsLocked()
}

// compactLocked folds daily buckets older than the retention horizon into
// their all-time rows, queueing the daily rows for deletion. Callers hold mu.
func (l *ReadLedger) compactLocked() {
	horizon := time.Now().UTC().Add(-l.retention).Format("2006-01-02")
	for key, st := range l.byKey {
		if key.Day == "" || key.Day >= horizon {
			continue
		}
		fold := key
		fold.Day = ""
		agg := l.byKey[fold]
		agg.Project, agg.Path, agg.Kind, agg.Actor = st.Project, st.Path, st.Kind, st.Actor
		agg.Day = ""
		agg.Count += st.Count
		if st.Last.After(agg.Last) {
			agg.Last = st.Last
		}
		l.byKey[fold] = agg
		l.dirty[fold] = true
		delete(l.byKey, key)
		delete(l.dirty, key)
		l.pendingDel = append(l.pendingDel, key)
	}
}

// persistLocked writes queued deletions and dirty buckets through the repo.
// Callers hold mu. Both queues survive a failure so the next flush retries —
// dropping a deletion would resurrect folded rows on the next load and
// double-count them.
func (l *ReadLedger) persistLocked() error {
	if len(l.pendingDel) > 0 {
		if err := l.repo.DeleteBatch(l.pendingDel); err != nil {
			// Same one-transaction problem as the put path below, worse for
			// being first: a key the store will never accept parks here
			// forever and PutBatch is then never reached at all, so the whole
			// hub's telemetry stops persisting. Retry one at a time; if some
			// land, the ones that did not are keys this store will never
			// accept, so drop them. If none land the store is down —
			// transient — and the queue stands for the next flush.
			if len(l.pendingDel) == 1 {
				return err
			}
			var stuck []ReadStatKey
			landed := 0
			for _, key := range l.pendingDel {
				if l.repo.DeleteBatch([]ReadStatKey{key}) == nil {
					landed++
				} else {
					stuck = append(stuck, key)
				}
			}
			if landed == 0 {
				return err
			}
			for _, key := range stuck {
				log.Printf("beardrive: read telemetry dropped an undeletable bucket (project %s, path %q): %v",
					key.Project, key.Path, err)
			}
		}
		l.pendingDel = nil
	}
	if len(l.dirty) == 0 {
		return nil
	}
	batch := make([]ReadStat, 0, len(l.dirty))
	for key := range l.dirty {
		batch = append(batch, l.byKey[key])
	}
	err := l.repo.PutBatch(batch)
	if err == nil {
		l.dirty = map[ReadStatKey]bool{}
		return nil
	}
	if len(batch) == 1 {
		return err
	}
	// One transaction, so one bucket the store refuses takes every other
	// bucket down with it — and, because they stay dirty, every bucket the hub
	// counts from then on. That is a hub-wide telemetry kill from the lowest
	// privilege there is (Postgres rejects a NUL byte in a path; sqlite and
	// the file backend store it happily). Retry one at a time: if some land,
	// the ones that did not are content this store will never accept, so drop
	// them rather than wedge the queue. If none land the store itself is
	// down — transient — and everything stays dirty for the next flush.
	var stuck []ReadStatKey
	landed := 0
	for key := range l.dirty {
		if l.repo.PutBatch([]ReadStat{l.byKey[key]}) == nil {
			delete(l.dirty, key)
			landed++
		} else {
			stuck = append(stuck, key)
		}
	}
	if landed == 0 {
		return err
	}
	for _, key := range stuck {
		log.Printf("beardrive: read telemetry dropped an unstorable bucket (project %s, path %q): %v",
			key.Project, key.Path, err)
		delete(l.dirty, key)
	}
	return nil
}

// hasControlChars reports whether s carries a C0/C7F control character —
// never legitimate in a path, and fatal to a Postgres text column.
func hasControlChars(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// ---- server integration ----

// ctxProjectKey carries the resolved project id from the proj() route
// resolver to handlers that record reads.
type ctxProjectKey struct{}

func withProjectID(r *http.Request, id string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxProjectKey{}, id))
}

func projectID(r *http.Request) string {
	id, _ := r.Context().Value(ctxProjectKey{}).(string)
	return id
}

// recordRead counts a human read of path for the request's project. No-op
// outside hub mode (no project id) or when read tracking is off.
func (s *Server) recordRead(r *http.Request, path string) {
	if s.Reads == nil {
		return
	}
	project := projectID(r)
	if project == "" {
		return
	}
	actor := s.requestUser(r).Email
	if actor == "" {
		actor = "anonymous"
	}
	s.Reads.Record(project, path, ReadKindHuman, actor)
}

// handleHeat serves per-path read aggregates: ?prefix= bounds to a folder,
// ?days= bounds the window (default 30, 0 = all time). With ?by=device it
// returns the agent-kind breakdown instead: per device (registry-joined),
// reads per top-level folder — the one place an actor id is reported, and
// only ever a device the reporting account owned (see the package comment).
// Human and share actors never leave the server in any shape.
func (s *Server) handleHeat(v *volume, w http.ResponseWriter, r *http.Request) {
	if s.Reads == nil {
		http.Error(w, "read tracking is not enabled on this server", http.StatusNotFound)
		return
	}
	_ = v
	q := r.URL.Query()
	// ?session=&device= is the run-card join: which paths that agent session
	// read. Both are required — a session-only query would also return rows a
	// member reported under someone else's session id, which pinning the row
	// to the reporting device (handleReadReport) is what makes harmless. This
	// is a filter INPUT only: nothing here or anywhere else enumerates
	// sessions, and the response carries paths, no identities and no counts.
	if session := q.Get("session"); session != "" || q.Get("device") != "" {
		device := q.Get("device")
		if session == "" || device == "" {
			http.Error(w, "session and device must be given together", http.StatusBadRequest)
			return
		}
		paths := s.Reads.SessionPaths(projectID(r), session, device)
		if paths == nil {
			paths = []string{} // an empty list, never a null the client must special-case
		}
		writeJSON(w, map[string]any{"paths": paths})
		return
	}
	days := 30
	if raw := q.Get("days"); raw != "" {
		var err error
		if days, err = strconv.Atoi(raw); err != nil || days < 0 {
			http.Error(w, "invalid days", http.StatusBadRequest)
			return
		}
	}
	var since time.Time
	if days > 0 {
		since = time.Now().UTC().AddDate(0, 0, -days)
	}
	switch q.Get("by") {
	case "":
	case "device":
		s.heatByDevice(w, projectID(r), since)
		return
	default:
		http.Error(w, "invalid by (use device)", http.StatusBadRequest)
		return
	}
	entries := s.Reads.Heat(projectID(r), q.Get("prefix"), since)
	out := map[string]any{"entries": entries}
	if !since.IsZero() {
		out["since"] = since.Format("2006-01-02")
	}
	writeJSON(w, out)
}

// deviceHeat is one row of the ?by=device response.
type deviceHeat struct {
	ID      string           `json:"id"`
	Name    string           `json:"name,omitempty"`
	OS      string           `json:"os,omitempty"`
	Folders map[string]int64 `json:"folders"`
	Total   int64            `json:"total"`
}

func (s *Server) heatByDevice(w http.ResponseWriter, project string, since time.Time) {
	byDevice := s.Reads.AgentHeat(project, since)
	visible := s.deviceVisibleIn(project)
	devices := make([]deviceHeat, 0, len(byDevice))
	for id, folders := range byDevice {
		d := deviceHeat{ID: id, Folders: folders}
		// Scoped join: a device owned by an account outside this project's org
		// contributes no name or OS, so heat cannot become a window onto
		// another org's machines.
		if info, ok := s.Devices.LookupIn(id, visible); ok {
			d.Name, d.OS = info.Name, info.OS
		}
		for _, n := range folders {
			d.Total += n
		}
		devices = append(devices, d)
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Total != devices[j].Total {
			return devices[i].Total > devices[j].Total
		}
		return devices[i].ID < devices[j].ID
	})
	out := map[string]any{"devices": devices}
	if !since.IsZero() {
		out["since"] = since.Format("2006-01-02")
	}
	writeJSON(w, out)
}

// handleReadReport ingests agent reads from a syncing device: the client's
// read spool, drained best-effort at sync time. Requires a device identity —
// the device id is the actor, so reads count as agent traffic.
func (s *Server) handleReadReport(v *volume, w http.ResponseWriter, r *http.Request) {
	if s.Reads == nil {
		http.Error(w, "read tracking is not enabled on this server", http.StatusNotFound)
		return
	}
	device := deviceID(r)
	if device == "" {
		http.Error(w, "agent read reports need a device identity", http.StatusBadRequest)
		return
	}
	var req struct {
		Reads []struct {
			Path string `json:"path"`
			// Session is the agent session the read happened in — a CLIENT
			// string, so it is only ever stored alongside the device the hub
			// validated below, never on its own. See the row write.
			Session string `json:"session,omitempty"`
			// Time is accepted for forward compatibility but buckets use
			// server time: client clocks are unreliable and late flushes are
			// telemetry noise, not data loss.
			Time time.Time `json:"time,omitzero"`
		} `json:"reads"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Reads) > 4096 {
		http.Error(w, "too many reads in one report", http.StatusBadRequest)
		return
	}
	// The device id becomes the actor these buckets are keyed by, and /heat
	// reports agent actors — so an unvalidated header would let any member
	// plant any string (an id from another org, or an account email) and have
	// the hub serve it back to the whole project as a reader. This route
	// deliberately does NOT observe the device: registering the id it is about
	// to judge is what made the round-2 check a one-request speed bump. Only
	// /store/* traffic registers a device.
	mine := s.ownsDevice(r, device)
	// A reported path is a claim about a file, and the heat map is what the
	// Dashboard's reads-x-staleness quadrant is built from — the view an
	// operator reads to decide what is stale. Any member with PermRead could
	// report any string, so the quadrant was member-writable fiction: a
	// "compliance/soc2-evidence-2026.md" nobody ever wrote showed up as read.
	// The project's own replayed state is the only thing that can say a path is
	// real, and it is right here. A snapshot the store cannot produce records
	// nothing this cycle: the client's spool is drained best-effort and retried,
	// and telemetry must never fail a request (nor invent one).
	snap, err := v.snapshot(r.Context())
	if err != nil {
		writeJSON(w, map[string]any{"accepted": 0})
		return
	}
	project := projectID(r)
	n := 0
	for _, e := range req.Reads {
		// A path is a bucket key that reaches the metadata store: a control
		// character (a NUL above all) is rejected outright by Postgres, and a
		// row the store will never accept has to be refused here rather than
		// discovered at flush time.
		// journal.SafePath is the rule, in the one place it is defined. This
		// was a fourth copy of it, and it disagreed in both directions: it
		// accepted "/etc/passwd", "a//b" and "./a", and refused "my..file".
		if !journal.SafePath(e.Path) {
			continue
		}
		if !mine {
			continue // not this account's device: counted for nobody
		}
		if _, real := snap.files[e.Path]; !real {
			continue // no such file in this project: a read of nothing is not a read
		}
		s.Reads.Record(project, e.Path, ReadKindAgent, device)
		// The session id is the one field here the hub cannot vouch for: it
		// arrives in the body, so any member could report reads naming a
		// teammate's session and paint files onto that teammate's run card.
		// The row is therefore pinned to `device` — the id ownsDevice just
		// validated — and the query side requires BOTH session and device, so
		// a forged row can only ever be found under the forger's own device,
		// which MayActAs guarantees is never someone else's.
		if sess := trimText(e.Session, 128); sess != "" && journal.SafeText(sess) {
			s.Reads.RecordSession(project, sess, device, e.Path)
		}
		n++
	}
	writeJSON(w, map[string]any{"accepted": n})
}
