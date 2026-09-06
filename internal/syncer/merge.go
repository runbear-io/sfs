package syncer

// Three-way merge for text files, so two people editing different parts of
// one file get one file instead of a conflict copy each.
//
// Hand-written, no dependency — the same call the frontend's line diff made
// and documented ("an LCS is ~40 lines, which is less code than auditing a
// diff package would be"). The same reasoning applies harder here: this runs
// inside conflictCopies, which has a security history, so the code that
// decides what a merged file contains should be readable in one sitting.
//
// Line-based on purpose. Word- or character-level merging resolves more
// cases and silently produces sentences neither author wrote; a line is the
// unit people actually edit, and anything overlapping falls through to the
// conflict copy that exists today.

import (
	"bytes"
	"io"
	"unicode/utf8"

	"github.com/runbear-io/beardrive/internal/journal"
)

// mergeText merges two edits of the same base. ok is false when the two sides
// changed the same region, which is a real conflict and stays one.
//
// Determinism matters more than cleverness here: two devices can each observe
// the same concurrency and merge independently, so merge(base, a, b) and
// merge(base, b, a) must agree or the fleet diverges. Only NON-overlapping
// hunks are ever taken, which makes the result the union either way.
func mergeText(base, a, b []byte) ([]byte, bool) {
	ls, la, lb := splitLines(base), splitLines(a), splitLines(b)
	ca, cb := changedRegions(ls, la), changedRegions(ls, lb)
	if overlaps(ca, cb) {
		return nil, false
	}

	// Walk the base once, emitting whichever side replaced each region.
	type edit struct {
		r    region
		with [][]byte
	}
	var edits []edit
	for _, r := range ca {
		edits = append(edits, edit{r, la[r.toFrom:r.toTo]})
	}
	for _, r := range cb {
		edits = append(edits, edit{r, lb[r.toFrom:r.toTo]})
	}
	// Sorted by base position; regions are disjoint, so this is a total order.
	for i := 1; i < len(edits); i++ {
		for j := i; j > 0 && edits[j].r.from < edits[j-1].r.from; j-- {
			edits[j], edits[j-1] = edits[j-1], edits[j]
		}
	}

	var out [][]byte
	at := 0
	for _, e := range edits {
		for ; at < e.r.from; at++ {
			out = append(out, ls[at])
		}
		out = append(out, e.with...)
		at = e.r.to
	}
	for ; at < len(ls); at++ {
		out = append(out, ls[at])
	}
	return joinLines(out, trailingNewline(a) || trailingNewline(b)), true
}

// region is a half-open span of BASE lines one side replaced, plus where the
// replacement lives in that side.
type region struct {
	from, to     int // in base
	toFrom, toTo int // in the edited side
}

// changedRegions is the coarsest useful diff: trim the common prefix and
// suffix and call the rest one changed span. That is exactly what makes the
// merge safe to reason about — a side "changed" one contiguous region, and
// two sides that changed different regions cannot interfere.
//
// It resolves the case that matters (two people working in different parts of
// a document) and declines everything else to the conflict copy, rather than
// resolving more cases with a hunk matcher nobody can audit.
func changedRegions(base, side [][]byte) []region {
	p := 0
	for p < len(base) && p < len(side) && bytes.Equal(base[p], side[p]) {
		p++
	}
	if p == len(base) && p == len(side) {
		return nil // identical
	}
	s := 0
	for s < len(base)-p && s < len(side)-p &&
		bytes.Equal(base[len(base)-1-s], side[len(side)-1-s]) {
		s++
	}
	return []region{{from: p, to: len(base) - s, toFrom: p, toTo: len(side) - s}}
}

// overlaps reports whether the two sides touched the same base lines. Regions
// that merely ABUT do not overlap — one side replacing lines 1-3 and the other
// 3-5 do, because line 3 is in both.
func overlaps(a, b []region) bool {
	if len(a) == 0 || len(b) == 0 {
		return false // one side did not change anything to conflict with
	}
	for _, x := range a {
		for _, y := range b {
			if x.from < y.to && y.from < x.to {
				return true
			}
			// A pure insertion is an empty base span; two insertions at the
			// same point are ordered by nothing and must not be merged.
			if x.from == x.to && y.from == y.to && x.from == y.from {
				return true
			}
		}
	}
	return false
}

func splitLines(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}
	out := bytes.Split(b, []byte("\n"))
	if len(out) > 0 && len(out[len(out)-1]) == 0 {
		out = out[:len(out)-1] // a trailing newline ends the last line
	}
	return out
}

func joinLines(lines [][]byte, trailing bool) []byte {
	out := bytes.Join(lines, []byte("\n"))
	if trailing && len(out) > 0 {
		out = append(out, '\n')
	}
	return out
}

func trailingNewline(b []byte) bool { return len(b) > 0 && b[len(b)-1] == '\n' }

// mergeNote marks an op this device produced by merging two concurrent edits,
// rather than by a local write. Display only — like every other Note, it is
// never an input to Less or Replay.
const mergeNote = "merged concurrent edits"

// mergedBlob is what tryMerge stored.
type mergedBlob struct {
	blob string
	size int64
}

// maxMergeBytes caps what will be read into memory to merge. A file bigger
// than this is a file nobody is hand-editing two copies of, and the conflict
// copy is the honest answer for it.
const maxMergeBytes = 4 << 20

// tryMerge attempts a three-way merge of two concurrent puts at the same path
// and, on success, stores the result as a blob.
//
// The common ancestor is the newest op for this path that both sides are
// built on: the greatest op (by journal.Less) that is Less than both. That is
// the state each side started from, which is exactly what a three-way merge
// needs.
//
// Everything here declines rather than guesses. No ancestor, a binary file,
// anything too big, overlapping edits, a blob that will not read — all fall
// back to the conflict copy that existed before.
func (s *Session) tryMerge(path string, mine, theirs journal.Op, all []journal.Op) (mergedBlob, bool) {
	if mine.Kind != journal.KindPut || theirs.Kind != journal.KindPut {
		return mergedBlob{}, false // a delete has no text to merge
	}
	if mine.Blob == theirs.Blob {
		return mergedBlob{}, false // same content; the caller already handles this
	}
	if mine.Size > maxMergeBytes || theirs.Size > maxMergeBytes {
		return mergedBlob{}, false
	}
	base, ok := commonAncestor(path, mine, theirs, all)
	if !ok {
		return mergedBlob{}, false
	}
	baseBytes, ok1 := s.readBlob(base.Blob, maxMergeBytes)
	mineBytes, ok2 := s.readBlob(mine.Blob, maxMergeBytes)
	theirsBytes, ok3 := s.readBlob(theirs.Blob, maxMergeBytes)
	if !ok1 || !ok2 || !ok3 {
		return mergedBlob{}, false
	}
	// Text only. Merging bytes that are not lines produces a file that is
	// neither side's, and for a binary format that means a corrupt one.
	if !isMergeableText(baseBytes) || !isMergeableText(mineBytes) || !isMergeableText(theirsBytes) {
		return mergedBlob{}, false
	}
	out, ok := mergeText(baseBytes, mineBytes, theirsBytes)
	if !ok {
		return mergedBlob{}, false
	}
	sum, n, err := s.Store.PutBlobReader(bytes.NewReader(out))
	if err != nil {
		return mergedBlob{}, false
	}
	return mergedBlob{blob: sum, size: n}, true
}

// commonAncestor is the state both sides edited: the greatest op for this path
// ordered before both of them. Ops for other paths are irrelevant, and an op
// that is not Less than BOTH is not something both sides could have seen.
func commonAncestor(path string, mine, theirs journal.Op, all []journal.Op) (journal.Op, bool) {
	var best journal.Op
	found := false
	for _, op := range all {
		if op.Path != path || op.Kind != journal.KindPut || op.Blob == "" {
			continue
		}
		if !journal.Less(op, mine) || !journal.Less(op, theirs) {
			continue
		}
		if !found || journal.Less(best, op) {
			best, found = op, true
		}
	}
	return best, found
}

// isMergeableText refuses anything with a NUL and anything that is not valid
// UTF-8 — the same "is this text" question the viewer asks, kept local so the
// merge cannot be handed bytes it will mangle.
func isMergeableText(b []byte) bool {
	return !bytes.ContainsRune(b, 0) && utf8.Valid(b)
}

// readBlob reads a stored blob, refusing anything past the cap.
func (s *Session) readBlob(sum string, max int64) ([]byte, bool) {
	rc, err := s.Store.OpenBlob(sum)
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, false
	}
	return b, true
}
