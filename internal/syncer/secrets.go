package syncer

import (
	"io"

	"github.com/runbear-io/beardrive/internal/secrets"
	"github.com/runbear-io/beardrive/internal/store"
)

// The credential check runs on the path every file takes — the sync scan —
// and it only ever WARNS. The op is journaled and pushed exactly as it was
// before this check existed; a hit adds a line to `bdrive status` and a
// sentence to the agent hook's context, and nothing else. Holding the op back
// would mean a false positive silently parks someone's changes, which costs
// more trust than the check buys, and it would break the cycle's "degrade to
// offline, never fail" posture. Warn keeps that; hold breaks it.
//
// It reads the BLOB the scan just wrote, not the working file again: those are
// the exact bytes that were hashed and journaled, so a line number can never
// describe content no op ever captured, and the read is the same page-cache
// read the hash pass just did.
//
// Nothing here may fail a cycle. Every error path drops the finding.

// secretsPerFile caps what one file contributes. A generated file full of
// key-shaped strings would otherwise put thousands of lines into a status
// report nobody can read, and the first few are what a person acts on.
const secretsPerFile = 8

// secretLog is one cycle's view of the mount's findings: loaded whole, merged
// per path, written back only if the cycle actually touched it. Per-path merge
// is the point — nearly every cycle scans zero changed files, and a whole-set
// rewrite would erase the warning seconds after it appeared.
type secretLog struct {
	found map[string][]secrets.Finding
	dirty bool
}

// scanBlob records what the blob just written for rel contains. Called only on
// the branches that ran PutBlobFile, so an unchanged file is never re-read.
func (l *secretLog) scanBlob(st *store.Store, rel, sum string) {
	if l == nil {
		return
	}
	f, err := st.OpenBlob(sum)
	if err != nil {
		return // unreadable: skipped like every other unreadable-file case
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, secrets.ScanLimit))
	if err != nil {
		return
	}
	found := secrets.Scan(buf)
	if len(found) > secretsPerFile {
		found = found[:secretsPerFile]
	}
	l.set(rel, found)
}

// set records rel's current findings, dropping the entry when there are none —
// which is how fixing the file clears the warning with no command and no flag.
func (l *secretLog) set(rel string, found []secrets.Finding) {
	if len(found) == 0 {
		l.drop(rel)
		return
	}
	if !sameFindings(l.found[rel], found) {
		l.found[rel] = found
		l.dirty = true
	}
}

// drop forgets a path: its credential is gone, or the path is.
func (l *secretLog) drop(rel string) {
	if l == nil {
		return
	}
	if _, ok := l.found[rel]; ok {
		delete(l.found, rel)
		l.dirty = true
	}
}

func sameFindings(a, b []secrets.Finding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
