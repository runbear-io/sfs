package syncer

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
)

// VerifyReport is what Verify found. Every slice holds mount-relative paths,
// sorted; an empty slice means that category is clean.
//
// A path can legitimately land in two categories at once — a file edited after
// its last scan and never pushed is both drifted and never-pushed — so the
// counts are independent and dedup happens only within a category.
type VerifyReport struct {
	Files   int           // files hashed this run
	Bytes   int64         // bytes hashed this run
	Elapsed time.Duration // how long the hashing took, so the cost is visible

	// Drifted: on disk and in the journal state, but sha256(disk) != state.Blob.
	Drifted []string
	// NeverPushed: this device's own ops past SyncState.PushedOps — content
	// the hub has never seen.
	NeverPushed []string
	// MissingLocally: a path the journal state holds and the local filter
	// includes, that this folder does not have.
	MissingLocally []string
	// NotFetched is how many of MissingLocally are simply not downloaded yet
	// (their blob is absent from the local store). That is the benign half of
	// this category — materializeFile returns early on it and the next cycle
	// fixes it — and without the distinction a freshly cloned device reads as
	// broken.
	NotFetched int
	// NotYetScanned: a file on disk the filter syncs, with no op anywhere yet.
	NotYetScanned []string
	// MissingOnHub: content this device believes is synced that the hub does
	// not hold. Only populated with a backend.
	MissingOnHub []string
	// RemoteErr is set when the remote leg could not finish. The local half of
	// the report still stands — this is the repo's "never break on the remote"
	// posture, the same one Result.Offline takes.
	RemoteErr error
}

// Problems is how many findings the report holds, across every category. It is
// what decides `bdrive verify`'s exit status.
func (r VerifyReport) Problems() int {
	return len(r.Drifted) + len(r.NeverPushed) + len(r.MissingLocally) +
		len(r.NotYetScanned) + len(r.MissingOnHub)
}

// Verify compares the bytes in the working folder against the content the
// journal records for them, hashing every synced file.
//
// It is a pure read, the same contract as Drift and Explain: no Session, no
// volume lock, no ops, no journal write, no materialize — and with be == nil,
// no network either. `bdrive verify` is what someone runs to check a folder
// they already suspect; a version of it that repaired anything would destroy
// the evidence it was asked to describe.
//
// Unlike Drift, which compares size+mtime against the state cache, this hashes
// content — that is the whole point, and there is deliberately no fast path.
// A file whose bytes changed but whose size and mtime were restored is exactly
// what Drift cannot see.
//
// device is this device's ID, for the never-pushed count. be may be nil, which
// skips the hub leg entirely.
//
// Known limitation, and the command prints it: the journals AllOps replays are
// this device's LOCAL copies. Without a pull, this proves "the folder matches
// what I last pulled" — and with a backend, "and the hub still holds all of
// it". A teammate's newer op committed since the last cycle is invisible here.
func Verify(ctx context.Context, folder string, include []string, st *store.Store, device string, be remote.Backend) (VerifyReport, error) {
	var rep VerifyReport
	start := time.Now()

	sync, err := st.LoadSync()
	if err != nil {
		return rep, err
	}

	// A fresh filter: addNestedMount mutates it during a walk, so this must
	// never be shared with a live cycle. AcceptRules for the reason Explain
	// documents — the scan applies Filter.SkipUp, and omitting it would
	// disagree with the very next cycle.
	filter, err := loadFilter(folder, include)
	if err != nil {
		return rep, err
	}
	filter.AcceptRules(sync.IgnoreAccepted)

	ops, err := st.AllOps()
	if err != nil {
		return rep, err
	}
	// The identical object Cycle materializes from (syncer.go, the pull phase),
	// so what this compares against is what the cycle would write.
	state := journal.Replay(ops)

	paths, err := SyncedFiles(folder, include, sync.IgnoreAccepted)
	if err != nil {
		return rep, err
	}

	seen := make(map[string]bool, len(paths))
	for _, rel := range paths {
		seen[rel] = true
		want, known := state[rel]
		if !known {
			rep.NotYetScanned = append(rep.NotYetScanned, rel)
			continue
		}
		abs := filepath.Join(folder, filepath.FromSlash(rel))
		sum, err := hashFile(abs)
		if err != nil {
			continue // vanished or unreadable; skipped, never fatal, as everywhere in this walk
		}
		rep.Files++
		if fi, err := os.Stat(abs); err == nil {
			rep.Bytes += fi.Size()
		}
		if sum != want.Blob {
			rep.Drifted = append(rep.Drifted, rel)
		}
	}

	for rel, want := range state {
		// The same guard materialize applies before it would write: a path the
		// local filter excludes is LEGITIMATELY absent from disk, because the
		// rules are applied symmetrically in scan and materialize. Without
		// this, every project narrowed by `bdrive scope --only` would report
		// every out-of-scope path as missing.
		if seen[rel] || filter.Skip(rel) || neverSync(rel) {
			continue
		}
		rep.MissingLocally = append(rep.MissingLocally, rel)
		if !st.HasBlob(want.Blob) {
			rep.NotFetched++
		}
	}

	// The same arithmetic push and `bdrive status` use: our own ops past the
	// push cursor are the ones the hub has never seen.
	if myOps, err := st.DeviceOps(device); err == nil {
		from := sync.PushedOps
		if from > int64(len(myOps)) {
			from = int64(len(myOps))
		}
		if from < 0 {
			from = 0
		}
		dedup := map[string]bool{}
		for _, op := range myOps[from:] {
			if dedup[op.Path] {
				continue
			}
			dedup[op.Path] = true
			rep.NeverPushed = append(rep.NeverPushed, op.Path)
		}
	}

	if be != nil {
		verifyRemote(ctx, be, state, &rep)
	}

	for _, s := range [][]string{rep.Drifted, rep.NeverPushed, rep.MissingLocally, rep.NotYetScanned, rep.MissingOnHub} {
		sort.Strings(s)
	}
	rep.Elapsed = time.Since(start)
	return rep, nil
}

// verifyRemote asks the hub whether it still holds every distinct blob the
// journal state references — one existence check per blob, not per path.
func verifyRemote(ctx context.Context, be remote.Backend, state map[string]journal.FileState, rep *VerifyReport) {
	byBlob := map[string][]string{}
	size := map[string]int64{}
	for rel, want := range state {
		if want.Blob == "" {
			continue
		}
		byBlob[want.Blob] = append(byBlob[want.Blob], rel)
		if want.Size > size[want.Blob] {
			size[want.Blob] = want.Size
		}
	}
	blobs := make([]string, 0, len(byBlob))
	for b := range byBlob {
		blobs = append(blobs, b)
	}
	sort.Strings(blobs) // stable order, so a partial run after RemoteErr is reproducible

	for _, b := range blobs {
		ok, err := existsEither(ctx, be, b, size[b])
		if err != nil {
			// Stop the remote leg, keep the local report. Never a hard failure.
			rep.RemoteErr = err
			return
		}
		if !ok {
			rep.MissingOnHub = append(rep.MissingOnHub, byBlob[b]...)
		}
	}
}

// existsEither asks for a blob under both key shapes it can live under.
//
// A file over chunkThreshold is pushed as content-defined chunks plus a
// manifest keyed by the FILE's own sha256 — manifests/<sha>, never
// blobs/<sha> (chunks.go). A check that only asked blobs/<sha> would report
// every large file as missing from the hub.
//
// The size only picks which key to try FIRST; it can never be a filter,
// because a large file legitimately lives under blobs/ in three ways: the
// browser upload path always writes blobs/<sha> at any size, pushChunked falls
// back to a whole-blob Put when the manifest key is refused, and anything
// pushed before delta sync existed is a whole blob regardless of size.
//
// Checking the manifest key alone is enough for a chunked file:
// "a manifest exists ⟹ its chunks exist" is enforced hub-side at ingest, not
// an honest-client convention, so there is no need to walk the chunk list.
func existsEither(ctx context.Context, be remote.Backend, blob string, size int64) (bool, error) {
	keys := [2]string{"blobs/" + blob, "manifests/" + blob}
	if size > chunkThreshold {
		keys[0], keys[1] = keys[1], keys[0]
	}
	for _, k := range keys {
		ok, err := be.Exists(ctx, k)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil // short-circuit: the common case is one round trip
		}
	}
	return false, nil
}
