package syncer

import (
	"io/fs"

	"github.com/runbear-io/beardrive/internal/store"
)

// Drift reports what the working folder holds that the state cache does not
// yet know about: files added, files whose size or mtime moved, and cached
// paths gone from disk.
//
// It is a pure read, the same contract as Explain and SyncedFiles: no Session,
// no volume lock, no network, no writes — and it neither stores blobs nor
// mints ops nor touches the cache it is handed. `bdrive status` is what someone
// runs when sync is stuck; a version of it that scanned-and-committed would
// change the thing it was asked to describe.
//
// The comparison is the scan's own cheap change detection (size + mtime against
// store.CachedFile), reached through walkFolder so the file verdict is the
// cycle's verdict and not a second copy of it.
//
// accepted is the ignore text this device has accepted (store.SyncState's
// IgnoreAccepted; "" when there is none), for the reason Explain documents: the
// scan applies Filter.SkipUp, and a drift count that omitted it would disagree
// with the very next cycle.
func Drift(folder string, include []string, accepted string, cache map[string]store.CachedFile) (added, modified, removed int, err error) {
	// A fresh filter: addNestedMount mutates it during the walk, so this must
	// never be shared with a live cycle.
	filter, err := loadFilter(folder, include)
	if err != nil {
		return 0, 0, 0, err
	}
	filter.AcceptRules(accepted)

	seen := make(map[string]bool, len(cache))
	err = walkFolder(folder, filter, func(_, rel string, d fs.DirEntry, v verdict) error {
		if v != vSync {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // vanished or unreadable; the next scan retries, as everywhere
		}
		seen[rel] = true
		c, ok := cache[rel]
		switch {
		case !ok:
			added++
		case c.Size != info.Size() || c.MTimeNS != info.ModTime().UnixNano():
			modified++
		}
		return nil
	})
	if err != nil {
		return 0, 0, 0, err
	}

	for rel := range cache {
		// The same drops scan applies before it would mint a delete: a path
		// the walk cannot have produced, or one that is newly filtered, is
		// dropped from the cache without a delete op — so it is not drift.
		if seen[rel] || neverSync(rel) || filter.Skip(rel) {
			continue
		}
		removed++
	}
	return added, modified, removed, nil
}
