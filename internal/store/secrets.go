package store

import (
	"github.com/runbear-io/beardrive/internal/secrets"
)

// The secrets record is what a cycle's credential findings survive in. The scan
// flags a file the moment it changes; the two surfaces that report it —
// `bdrive status` and the agent hook — run minutes later, long after that file
// stopped changing and the cheap size+mtime path stopped reading it. So the
// record has to outlive the cycle that made it, for the same reason the inbound
// spool does (see inbound.go).
//
// It is merged PER PATH, never replaced wholesale: nearly every cycle scans
// zero changed files, so a whole-set rewrite would erase the warning three
// seconds after it appeared. A finding stands until the file changes again
// without it — which is what makes fixing the file the only thing needed to
// clear the warning, no command and no flag.
//
// It is advisory telemetry about paths this device already journaled and
// pushed. It must never be able to fail a cycle.

// secretsPath names the per-mount findings file, validated exactly like
// cachePath: the mount id comes from a folder's .bdrive/config.json, so a
// separator in it would put this file wherever its author chose.
func (s *Store) secretsPath(mountID string) (string, error) {
	return s.mountStatePath("secrets-", mountID)
}

// LoadSecrets returns the findings recorded for each mount-relative path.
func (s *Store) LoadSecrets(mountID string) (map[string][]secrets.Finding, error) {
	p, err := s.secretsPath(mountID)
	if err != nil {
		return nil, err
	}
	out := map[string][]secrets.Finding{}
	if err := readJSON(p, &out); err != nil {
		return nil, err
	}
	// Same treatment as the cache's keys: plain JSON in $BDRIVE_HOME that
	// anything running as the user can write, rendered into a terminal and
	// joined against cache keys. An out-of-volume path is dropped, not shown.
	for rel, f := range out {
		if !cleanRel(rel) || len(f) == 0 {
			delete(out, rel)
		}
	}
	return out, nil
}

func (s *Store) SaveSecrets(mountID string, f map[string][]secrets.Finding) error {
	p, err := s.secretsPath(mountID)
	if err != nil {
		return err
	}
	return WriteJSONAtomic(p, f)
}
