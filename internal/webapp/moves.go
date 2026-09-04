package webapp

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Moves. There is no rename in beardrive: the scanner emits a put for the
// path it has never seen and a delete for the cache key it no longer sees,
// both in the same cycle, from the same device, carrying the same blob
// (internal/syncer/syncer.go). journal.Op has put and delete and nothing
// else, so a move is only ever inferred — never recorded.
//
// Everything keyed on a path therefore breaks the moment a file moves: the
// viewer 404s, history loses the file's own past versions, restore refuses
// them, and a share link either 404s or — worse — silently serves whatever
// unrelated file later lands on the address it was minted for.
//
// This file derives the pairing from the ops the replay already walks. It
// deliberately does NOT add a rename op: journal.Less and Replay are what
// every device converges to, and every already-shipped journal would still
// need the heuristic to read its own history. Derivation is reversible; a
// new op kind is not.
//
// Everything here is read-side. Nothing in this file writes an op.

// moveWindow is how far apart the two halves of a move may land. Wider than
// one daemon cycle (--scan-interval 3s, so a move split across two cycles
// still pairs), narrower than a person's editing session. It is the one
// number worth revisiting under real journals.
const moveWindow = 30 * time.Second

// maxChain bounds every walk in this file. Op.Time and Op.Path are peer JSON,
// so a journal can describe a cycle (A→B→A); a visited set alone stops the
// loop, and this stops a pathological chain from making one request walk a
// journal-sized graph.
const maxChain = 64

// pathEvent is one moment a path stopped being the file it was.
type pathEvent struct {
	At   time.Time // the delete that ended it
	To   string    // where the content went; "" = deleted, not moved
	ToAt time.Time // when the destination was created (move only)
}

// hop is the instant the file changed address. The two halves of a move
// arrive in either order (an in-cycle move is put-then-delete; one split
// across cycles is delete-then-put), so the boundary between the old path's
// window and the new one's is the earlier of the two — otherwise the
// destination's own creating op falls outside its own history.
func (e pathEvent) hop() time.Time {
	if e.To != "" && !e.ToAt.IsZero() && e.ToAt.Before(e.At) {
		return e.ToAt
	}
	return e.At
}

// moveIndex is, per path, the events that ended it — in time order.
//
// One time-stamped list rather than two flat maps (movedTo + deletedAt): a
// path that is deleted, recreated and then moved has an entry under the same
// key in both maps and nothing left to say which came first, so a share
// minted before the delete would follow the move and serve the file that
// REPLACED its own.
type moveIndex map[string][]pathEvent

// buildMoveIndex pairs deletes with creates. ops must already be sorted by
// journal.Less — every caller has them that way (the replay, the history
// feed) so the index costs no extra I/O and no extra pass over storage.
//
// A delete of A pairs with the first-ever put of B when all hold: same
// Op.Device (one device's scan produces both halves), |Δt| ≤ moveWindow,
// B's blob is the blob A held immediately before its delete (content
// identity is the only link the journal gives us), and the pairing is
// one-to-one in both directions. Anything ambiguous — duplicated content,
// empty files — stays unpaired and becomes a plain deletion. Silence beats
// a wrong destination.
func buildMoveIndex(ops []journal.Op) moveIndex {
	type half struct {
		path, blob, dev string
		at              time.Time
	}
	var creates, deletes []half
	live := map[string]string{} // path -> the blob it currently holds
	ever := map[string]bool{}   // path has ever been put
	for _, op := range ops {
		switch op.Kind {
		case journal.KindPut:
			// Files ignores a put whose Blob is not a bare sha256 (it is a
			// storage key suffix, so anything else is a path the writer
			// chose). The index has to agree, or it would pair against a
			// version the viewer never shows.
			if !blobRe.MatchString(op.Blob) {
				continue
			}
			if !ever[op.Path] {
				ever[op.Path] = true
				creates = append(creates, half{op.Path, op.Blob, op.Device, op.Time})
			}
			live[op.Path] = op.Blob
		case journal.KindDelete:
			blob, ok := live[op.Path]
			if !ok {
				continue // deleting nothing
			}
			delete(live, op.Path)
			deletes = append(deletes, half{op.Path, blob, op.Device, op.Time})
		}
	}

	key := func(h half) string { return h.dev + "\x00" + h.blob }
	byKey := map[string][]int{}
	for i, c := range creates {
		byKey[key(c)] = append(byKey[key(c)], i)
	}
	// Deletes bucketed the same way, for the reverse one-to-one check below:
	// that check used to rescan EVERY delete and rebuild key(d2) per step, so
	// a volume with n moves paid n^2 string builds — 168M of them, ~8s, on
	// every viewer request. Same buckets, same answer, one map earlier.
	byKeyDel := map[string][]int{}
	for i, d := range deletes {
		byKeyDel[key(d)] = append(byKeyDel[key(d)], i)
	}
	pairs := func(a, b half) bool {
		return a.path != b.path && abs(a.at.Sub(b.at)) <= moveWindow
	}
	// ponytail: O(n²) inside one (device, blob) bucket. A bucket is the
	// identical copies of ONE file, not the volume — bucket by time too if
	// a duplicate-heavy volume ever shows up in a profile.
	idx := moveIndex{}
	for _, d := range deletes {
		ev := pathEvent{At: d.at}
		var only int = -1
		for _, i := range byKey[key(d)] {
			if !pairs(creates[i], d) {
				continue
			}
			if only >= 0 {
				only = -1 // two candidates: ambiguous, decline
				break
			}
			only = i
		}
		if only >= 0 {
			// ...and one-to-one the other way: two deletes claiming one
			// create is the same ambiguity seen from the other side.
			c, n := creates[only], 0
			for _, j := range byKeyDel[key(c)] {
				if pairs(c, deletes[j]) {
					n++
				}
			}
			if n == 1 {
				ev.To, ev.ToAt = c.path, c.at
			}
		}
		idx[d.path] = append(idx[d.path], ev)
	}
	for p := range idx {
		evs := idx[p]
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.Before(evs[j].At) })
	}
	return idx
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// resolveForward answers "this address is empty — where did the file go?".
// It follows the LAST event of each path (the most recent move wins) until
// it lands somewhere live. Used by the viewer, where a live path always
// wins: the caller only reaches here on a snapshot miss.
func resolveForward(idx moveIndex, files map[string]FileInfo, p string) (string, bool) {
	seen := map[string]bool{p: true}
	for i := 0; i < maxChain; i++ {
		evs := idx[p]
		if len(evs) == 0 {
			return "", false
		}
		to := evs[len(evs)-1].To
		if to == "" || seen[to] {
			return "", false // deleted, or a cycle a journal described
		}
		seen[to] = true
		if _, ok := files[to]; ok {
			return to, true
		}
		p = to
	}
	return "", false
}

// resolveShare answers the opposite question: "where is the file this token
// was minted for?" — following the file even when a new one has taken its
// old address. A viewer URL is an address; a share token is a promise about
// one file, and must never resolve to a file it wasn't minted for.
//
// since is the share's creation time: only what happened AFTER the link was
// minted can move it. A path that was deleted rather than moved is gone,
// even if something now occupies the address.
func resolveShare(idx moveIndex, files map[string]FileInfo, p string, since time.Time) (string, bool) {
	seen := map[string]bool{p: true}
	for i := 0; i < maxChain; i++ {
		var next *pathEvent
		for j := range idx[p] {
			if idx[p][j].At.After(since) {
				next = &idx[p][j]
				break
			}
		}
		if next == nil {
			// Nothing happened to this path since: it is still the file.
			if _, ok := files[p]; ok {
				return p, true
			}
			return "", false
		}
		if next.To == "" || seen[next.To] {
			return "", false
		}
		seen[next.To] = true
		p, since = next.To, next.At
	}
	return "", false
}

// segment is one path plus the window during which it WAS the file being
// asked about. Time-bounded on purpose: a bare set of paths would make
// history?path=docs/a.md show the ops of the new a.md that took the old
// address. A zero From/To is open at that end.
type segment struct {
	Path     string
	From, To time.Time
}

// chainSegments walks backwards: p's own window plus each ancestor's,
// ending at the hop that carried the file out of it. Used by history and
// restore, so a moved file keeps reaching its own past versions.
func chainSegments(idx moveIndex, p string) []segment {
	type inbound struct {
		from string
		hop  time.Time
	}
	into := map[string][]inbound{}
	for src, evs := range idx {
		for _, e := range evs {
			if e.To != "" {
				into[e.To] = append(into[e.To], inbound{src, e.hop()})
			}
		}
	}
	var segs []segment
	seen := map[string]bool{p: true}
	cur, upper := p, time.Time{}
	for i := 0; i < maxChain; i++ {
		anc := into[cur]
		// No ancestor, or more than one claiming to be it: stop, leaving
		// this segment open at the start rather than guessing.
		if len(anc) != 1 || seen[anc[0].from] {
			return append(segs, segment{Path: cur, To: upper})
		}
		segs = append(segs, segment{Path: cur, From: anc[0].hop, To: upper})
		seen[anc[0].from] = true
		cur, upper = anc[0].from, anc[0].hop
	}
	return segs
}

// inSegments reports whether an op at path p, time at, belongs to the chain.
func inSegments(segs []segment, p string, at time.Time) bool {
	for _, s := range segs {
		switch {
		case s.Path != p:
		case !s.From.IsZero() && at.Before(s.From):
		case !s.To.IsZero() && !at.Before(s.To):
		default:
			return true
		}
	}
	return false
}

// resolveFolder derives a folder redirect from the file mappings — there are
// no folder ops, so a folder move is N file moves. All-or-nothing: every
// file that was under dir/ must land on the same newdir/<same suffix>, with
// nothing under dir/ still live and no non-move delete in the way. A partial
// match gets no folder redirect; the individual files still redirect on
// their own.
func resolveFolder(idx moveIndex, files map[string]FileInfo, dir string) (string, bool) {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return "", false
	}
	prefix := dir + "/"
	for p := range files {
		if strings.HasPrefix(p, prefix) {
			return "", false // the folder still exists; nothing to redirect
		}
	}
	dest := ""
	for src := range idx {
		if !strings.HasPrefix(src, prefix) {
			continue
		}
		to, ok := resolveForward(idx, files, src)
		if !ok {
			return "", false // deleted, or ambiguous: no honest destination
		}
		suffix := strings.TrimPrefix(src, prefix)
		if !strings.HasSuffix(to, "/"+suffix) {
			return "", false
		}
		nd := strings.TrimSuffix(to, "/"+suffix)
		if nd == "" || (dest != "" && nd != dest) {
			return "", false
		}
		dest = nd
	}
	return dest, dest != ""
}

// handleResolve serves GET .../resolve?path= — "this address is empty, where
// did it go?". The SPA calls it only on a miss (it decides a path is missing
// from /tree alone and never fetches the file, so the canonical header on
// /file would never reach the browser) and it is the only shape that covers
// a moved folder, which has no content fetch to hang a header on.
func (s *Server) handleResolve(v *volume, w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "missing ?path=", http.StatusBadRequest)
		return
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		storageErr(w, http.StatusBadGateway, "content temporarily unavailable", err)
		return
	}
	if _, live := snap.files[p]; !live {
		if to, ok := resolveForward(snap.moves, snap.files, p); ok {
			writeJSON(w, map[string]string{"to": to, "kind": "file"})
			return
		}
		if to, ok := resolveFolder(snap.moves, snap.files, p); ok {
			writeJSON(w, map[string]string{"to": to, "kind": "folder"})
			return
		}
	}
	http.Error(w, "no such file: "+p, http.StatusNotFound)
}

// setCanonical tells the caller the path moved. The value is the path that
// answered; a client that cares rewrites its URL to it.
func setCanonical(w http.ResponseWriter, r *http.Request, p string) {
	if p != r.URL.Query().Get("path") {
		w.Header().Set("X-Bdrive-Canonical-Path", p)
	}
}
