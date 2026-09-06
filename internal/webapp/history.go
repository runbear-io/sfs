package webapp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// History is read straight from the journals: every put/delete ever made,
// newest first, with the account that made it and what the server knows
// about the device it came from. Content is content-addressed and retained
// forever, so each entry links to its exact version — the groundwork for
// the revert/rollback phase, where restoring is just writing an old blob
// back as a new op.

// historyDevice is the slice of the device registry history is allowed to
// report: who/what made the change, not where they connected from. The
// registry keeps observing and persisting the IP (devices.go) — it just
// doesn't ride along here, where every project member reads it. Mirrors
// heatByDevice (reads.go).
type historyDevice struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	OS   string `json:"os,omitempty"`
}

// HistoryEntry is one change as the history API reports it.
type HistoryEntry struct {
	Time     string        `json:"time"`
	Kind     string        `json:"kind"` // add | edit | delete
	Path     string        `json:"path"`
	Size     int64         `json:"size,omitempty"`
	Blob     string        `json:"blob,omitempty"` // sha256; fetch via the blob endpoint
	User     string        `json:"user,omitempty"`
	UserName string        `json:"user_name,omitempty"`
	Author   string        `json:"author,omitempty"` // offline/git fallback identity
	Device   historyDevice `json:"device"`
	Note     string        `json:"note,omitempty"`
	// Session is the agent session the op was committed during (hook-set,
	// see journal.Op.Session). It is the run card's group key and the only
	// place a session id is ever served: it is never enumerated, never a
	// column in /heat's output, and never in ?by=device — it appears here,
	// on the op that carries it, and is accepted as a ?session= filter INPUT.
	Session string `json:"session,omitempty"`
}

// histLess is the display order of the history feed: newest wall-clock time
// first, ties in reverse journal order. Journal order is causal, not
// chronological (see journal.Less) — this is the one place that reconciles
// the two, and the cursor skips with the same function the feed sorts with,
// so paging can never disagree with what a reader sees. journal.Less is a
// total order and (device, seq) is unique per op, so histLess is total too:
// no stable sort needed, and equal timestamps come back in the same order
// on every request.
func histLess(a, b journal.Op) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.After(b.Time)
	}
	return journal.Less(b, a)
}

// histCursor is the ordering tuple of the last entry of a page — everything
// histLess reads, and nothing else. It rides the wire base64'd so a client
// treats it as opaque: HistoryEntry.time is formatted to whole seconds and
// carries no lamport/seq, so a client-computed cursor would be lossy across
// same-second ops.
type histCursor struct {
	// RFC3339Nano, not UnixNano: Op.Time is unvalidated peer JSON and
	// UnixNano is undefined outside [1678, 2262], so a date of 2300 read back
	// as 1715 — the skip loop then walked past every entry and returned a
	// clean end of feed. One journal push hid the whole audit trail past page
	// one from every other member.
	T string `json:"t"` // op time, RFC3339Nano
	L int64  `json:"l"` // lamport
	S int64  `json:"s"` // per-device seq
	D string `json:"d"` // device
}

func encodeCursor(op journal.Op) string {
	b, err := json.Marshal(histCursor{T: op.Time.UTC().Format(time.RFC3339Nano), L: op.Lamport, S: op.Seq, D: op.Device})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor rebuilds the four ordering fields into a bare Op — JSON
// rather than a delimited string, so a device id can never collide with a
// separator.
func decodeCursor(s string) (journal.Op, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return journal.Op{}, err
	}
	var c histCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return journal.Op{}, err
	}
	ts, err := time.Parse(time.RFC3339Nano, c.T)
	if err != nil {
		return journal.Op{}, err
	}
	return journal.Op{Time: ts.UTC(), Lamport: c.L, Seq: c.S, Device: c.D}, nil
}

// parseHistTime accepts RFC3339 or a bare YYYY-MM-DD (UTC). A bare date is
// the start of that day; `end` bumps it by 24h so ?until=<day> includes the
// whole day — the bound is then exclusive on both parses, which is what an
// inclusive "until this date" means once you stop thinking in instants.
func parseHistTime(s string, end bool) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if end {
			t = t.Add(time.Nanosecond) // RFC3339 bound is inclusive to the second
		}
		return t.UTC(), true
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	if end {
		t = t.AddDate(0, 0, 1)
	}
	return t.UTC(), true
}

// handleHistory serves ?path=<file> (one file's versions) or
// ?prefix=<folder/> (everything underneath, "" = the whole project),
// newest first by wall-clock time, at most ?n= entries (default 100).
//
// Reader filters — ?q= (case-insensitive substring of the path), ?user=
// (exact account), ?since=/?until= (UTC bounds, inclusive at both ends) —
// compose with each other and with path/prefix. They are applied in the same
// walk as path/prefix, i.e. BEFORE the sort and the cursor skip, so
// next_cursor keeps meaning "the next matching entry" and paging under a
// filter needs no new machinery.
//
// Paging: the response carries next_cursor when more entries exist, and
// ?cursor= resumes just past the entry it was minted from — so history older
// than one page is reachable, and the UI can tell whether it is hiding
// anything. ?n= alone returns exactly the entries it always did.
//
// A cursor is a position in an ordering, not a snapshot: an offline device
// that pushes mid-scroll lands ops in the middle of the feed by timestamp,
// and the reader sees them on refresh rather than mid-page. Deliberate —
// pinning the feed to a read time means server-side state for the life of a
// scroll.
func (s *Server) handleHistory(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	q := r.URL.Query()
	path, prefix := q.Get("path"), q.Get("prefix")
	if path != "" && q.Has("prefix") {
		http.Error(w, "use ?path= or ?prefix=, not both", http.StatusBadRequest)
		return
	}
	n := 100
	if raw := q.Get("n"); raw != "" {
		var err error
		if n, err = strconv.Atoi(raw); err != nil || n < 1 {
			http.Error(w, "invalid n", http.StatusBadRequest)
			return
		}
	}
	needle := strings.ToLower(q.Get("q")) // lowered once, not per op
	user := q.Get("user")
	var since, until time.Time
	// since > until is not an error: it means "nothing", which is what it returns.
	for _, b := range []struct {
		name string
		end  bool
		into *time.Time
	}{{"since", false, &since}, {"until", true, &until}} {
		raw := q.Get(b.name)
		if raw == "" {
			continue
		}
		t, ok := parseHistTime(raw, b.end)
		if !ok {
			http.Error(w, "invalid "+b.name, http.StatusBadRequest)
			return
		}
		*b.into = t
	}
	all, err := rs.loadSourcedOps(r.Context())
	if err != nil {
		storageErr(w, http.StatusBadGateway, "history is temporarily unavailable", err)
		return
	}
	sort.SliceStable(all, func(i, j int) bool { return journal.Less(all[i].Op, all[j].Op) })
	// A put is an "add" when the path didn't exist just before it (first
	// version, or first after a delete), an "edit" otherwise. Existence is
	// replayed over ALL ops in journal order, before any path/prefix filter,
	// so a filtered view classifies the same as the full feed.
	kinds := make([]string, len(all))
	exists := make(map[string]bool, len(all))
	for i, so := range all {
		op := so.Op
		switch {
		case op.Kind == journal.KindDelete:
			kinds[i] = "delete"
			exists[op.Path] = false
		case exists[op.Path]:
			kinds[i] = "edit"
		default:
			kinds[i] = "add"
			exists[op.Path] = true
		}
	}
	type timed struct {
		entry HistoryEntry
		op    journal.Op
	}
	visible := s.deviceVisibleIn(projectID(r))
	// A file that moved keeps its past — under its old path. ?path= resolves
	// through the move chain so the feed for docs/a.md includes the versions
	// written while it was a.md. Each hop is time-bounded (see segment), so
	// an unrelated NEW a.md created after the move does not leak in. `all`
	// is already sorted by journal.Less, so this costs no extra I/O.
	var chain []segment
	if path != "" {
		ops := make([]journal.Op, len(all))
		for i, sop := range all {
			ops[i] = sop.Op
		}
		chain = chainSegments(buildMoveIndex(ops), path)
	}
	// Resolved once, not per op: history walks every op in the project.
	vis := s.visibility(r)
	matched := make([]timed, 0, len(all))
	for i, sop := range all {
		op := sop.Op
		switch {
		// Before every other filter. History is a per-path audit feed, so an
		// unfiltered row leaks the path, who changed it and when — the same
		// three facts the tree would have leaked.
		case !vis.canRead(op.Path):
			continue
		case path != "" && !inSegments(chain, op.Path, op.Time):
			continue
		case path == "" && prefix != "" && !strings.HasPrefix(op.Path, strings.TrimSuffix(prefix, "/")+"/"):
			continue
		case needle != "" && !strings.Contains(strings.ToLower(op.Path), needle):
			continue
		case user != "" && op.User != user:
			continue
		case !since.IsZero() && op.Time.Before(since):
			continue
		case !until.IsZero() && !op.Time.Before(until):
			continue
		}
		// Attribution comes from the journal the op was READ from, never from
		// its own Device field: that field is arbitrary JSON any member with
		// write access can put in their own journal, so trusting it printed
		// another org's machine name into this feed and answered "does this
		// device id exist on the hub?" for anyone who asked. The registry join
		// is scoped the same way heat's is.
		dev := historyDevice{ID: sop.From}
		if op.Device == sop.From {
			dev.Name = op.DeviceName // the journal's owner describing itself
		}
		if info, ok := s.Devices.LookupIn(sop.From, visible); ok && info.ID != "" {
			dev = historyDevice{ID: info.ID, Name: info.Name, OS: info.OS}
		}
		matched = append(matched, timed{HistoryEntry{
			Time: op.Time.UTC().Format("2006-01-02T15:04:05Z"), Kind: kinds[i],
			Path: op.Path, Size: op.Size, Blob: op.Blob,
			User: op.User, UserName: op.UserName, Author: op.Author,
			Device: dev, Note: op.Note, Session: op.Session,
		}, op})
	}
	// Truncation happens AFTER the sort: cutting during the walk above would
	// pick the n highest-Lamport entries and merely display them in time order.
	sort.Slice(matched, func(a, b int) bool { return histLess(matched[a].op, matched[b].op) })
	if raw := q.Get("cursor"); raw != "" {
		cur, err := decodeCursor(raw)
		if err != nil {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		i := 0
		for i < len(matched) && !histLess(cur, matched[i].op) { // skip to just past it
			i++
		}
		matched = matched[i:]
	}
	// Exact, with no over-fetch probe: every op is already in memory, so
	// "there is more" is a length check and the last page simply omits the key.
	var next string
	if len(matched) > n {
		next = encodeCursor(matched[n-1].op)
		matched = matched[:n]
	}
	entries := make([]HistoryEntry, len(matched))
	for i, m := range matched {
		entries[i] = m.entry
	}
	out := map[string]any{"entries": entries}
	if next != "" {
		out["next_cursor"] = next
	}
	writeJSON(w, out)
}

// handleBlob streams one exact version by content hash — view or download
// any point in a file's history.
func (s *Server) handleBlob(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	sha := r.URL.Query().Get("sha")
	if !blobRe.MatchString(sha) {
		http.Error(w, "invalid sha", http.StatusBadRequest)
		return
	}
	// Content addressing means this door names bytes, not a path — so the
	// question "may you read this?" has to be asked of every path that
	// references them. Answered before the fetch, and answered as a 404: a
	// past version of a hidden file is exactly as absent as one that never
	// existed.
	if !s.visibility(r).visibleSHA(r.Context(), rs, sha) {
		http.Error(w, "no such version", http.StatusNotFound)
		return
	}
	rc, err := rs.OpenBlob(r.Context(), sha)
	if err != nil {
		http.Error(w, "no such version", http.StatusNotFound)
		return
	}
	defer rc.Close()
	name := r.URL.Query().Get("name")
	if name != "" {
		ct := contentType(name)
		// Same wall and the same inert declaration as the live-file door: a
		// past version is the same bytes, so the two must never differ.
		w.Header().Set("Content-Type", inlineType(ct))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		sandboxInline(w, ct)
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeFilename(name)))
		}
	} else {
		// Same door, same stored bytes, two lines down — and it did not get the
		// header the arm above did. The rule is "nosniff on every door that
		// streams stored bytes", so it goes on unconditionally: a declared
		// octet-stream a browser is free to sniff is not a wall.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	io.Copy(w, rc)
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	return strings.Map(func(r rune) rune {
		if r < 32 || r == '"' {
			return '-'
		}
		return r
	}, name)
}
