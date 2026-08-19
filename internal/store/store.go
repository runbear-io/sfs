// Package store manages a volume's local on-disk state: a content-addressed
// blob store, the per-device journals, and small JSON state files. Everything
// a volume needs works offline; the remote is only used to exchange blobs and
// journals.
//
// Layout under <beardrive home>/volumes/<volume>/:
//
//	blobs/<aa>/<sha256>   content-addressed file contents (immutable)
//	journal/<device>.jsonl per-device op logs (own + cached copies of peers)
//	state.json            what is currently materialized in the folder
//	sync.json             lamport clock + push cursor
//	lock                  flock guarding cycles
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/runbear-io/beardrive/internal/journal"
)

type Store struct {
	dir string
}

func Open(dir string) (*Store, error) {
	for _, d := range []string{dir, filepath.Join(dir, "blobs"), filepath.Join(dir, "journal"), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Dir() string    { return s.dir }
func (s *Store) tmpDir() string { return filepath.Join(s.dir, "tmp") }

// ---- blobs ----

// blobSumRe is the only thing BlobPath will resolve. Op.Blob is copied
// verbatim out of a peer's journal, so it is a string someone else chose, not
// a hash: "../secret.txt" made HasBlob answer true for a file outside the
// store and OpenBlob hand its contents to writeFile as that path's content —
// a peer reading any file on every teammate's machine — and a Blob shorter
// than two characters panicked the daemon on sum[:2].
var blobSumRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// BlobPath is the on-disk location of a blob. A key that is not a sha256
// resolves to a name inside the blob dir that PutBlobReader never writes, so
// HasBlob and OpenBlob fail naturally instead of reaching somewhere else.
func (s *Store) BlobPath(sum string) string {
	if !blobSumRe.MatchString(sum) {
		return filepath.Join(s.dir, "blobs", "invalid")
	}
	return filepath.Join(s.dir, "blobs", sum[:2], sum)
}

func (s *Store) HasBlob(sum string) bool {
	if !blobSumRe.MatchString(sum) {
		return false
	}
	_, err := os.Stat(s.BlobPath(sum))
	return err == nil
}

// PutBlobReader streams r into the blob store, returning its sha256 and size.
func (s *Store) PutBlobReader(r io.Reader) (string, int64, error) {
	tmp, err := os.CreateTemp(s.tmpDir(), "blob-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if s.HasBlob(sum) {
		return sum, n, nil
	}
	dst := s.BlobPath(sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", 0, err
	}
	return sum, n, nil
}

func (s *Store) PutBlobFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return s.PutBlobReader(f)
}

func (s *Store) PutBlobBytes(b []byte) (string, int64, error) {
	return s.PutBlobReader(strings.NewReader(string(b)))
}

func (s *Store) OpenBlob(sum string) (*os.File, error) {
	return os.Open(s.BlobPath(sum))
}

// ---- journals ----

func (s *Store) JournalPath(device string) string {
	return filepath.Join(s.dir, "journal", device+".jsonl")
}

func (s *Store) AppendOps(device string, ops []journal.Op) error {
	return journal.Append(s.JournalPath(device), ops)
}

func (s *Store) DeviceOps(device string) ([]journal.Op, error) {
	return journal.ReadFile(s.JournalPath(device))
}

// Devices lists device IDs that have a local journal copy.
func (s *Store) Devices() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "journal"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, strings.TrimSuffix(e.Name(), ".jsonl"))
		}
	}
	sort.Strings(out)
	return out, nil
}

// AllOps returns the union of every journal known locally.
func (s *Store) AllOps() ([]journal.Op, error) {
	devs, err := s.Devices()
	if err != nil {
		return nil, err
	}
	var all []journal.Op
	for _, d := range devs {
		ops, err := s.DeviceOps(d)
		if err != nil {
			return nil, fmt.Errorf("journal %s: %w", d, err)
		}
		all = append(all, ops...)
	}
	return all, nil
}

// ---- materialized-state cache (state.json) ----

// CachedFile records what beardrive last wrote to / observed in the working folder
// for a path. Size+MTimeNS make change detection cheap; Blob ties it back to
// content. The cache is per mount (one volume can be materialized into
// several folders, each with its own stat fingerprints).
type CachedFile struct {
	Blob    string `json:"blob"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	MTimeNS int64  `json:"mtime_ns"`
}

// mountStatePath names a per-mount state file. The mount id comes from a
// folder's .bdrive/config.json — a file that travels with the folder — so it is
// checked here as well as where it is read: prefix+mountID+".json" is joined
// onto the volume dir, and a separator in it puts the cache (and everything
// keyed the same way) wherever its author chose.
func (s *Store) mountStatePath(prefix, mountID string) (string, error) {
	if mountID == "" || mountID == "." || mountID == ".." || strings.ContainsAny(mountID, `/\`) {
		return "", fmt.Errorf("invalid mount id %q", mountID)
	}
	return filepath.Join(s.dir, prefix+mountID+".json"), nil
}

// cachePath names the per-mount materialization cache (state-<mount>.json).
func (s *Store) cachePath(mountID string) (string, error) {
	return s.mountStatePath("state-", mountID)
}

func (s *Store) LoadCache(mountID string) (map[string]CachedFile, error) {
	p, err := s.cachePath(mountID)
	if err != nil {
		return nil, err
	}
	out := map[string]CachedFile{}
	if err := readJSON(p, &out); err != nil {
		return nil, err
	}
	// The keys are joined onto the working folder by the scan's delete pass
	// and by materialize's, and turned into journal ops this device signs.
	// The file is plain JSON in $BDRIVE_HOME, so anything running as the user
	// (an agent session, an install script, an older bdrive) chooses them —
	// validated where they are read, exactly like Project.ID.
	for rel := range out {
		if !cleanRel(rel) {
			delete(out, rel)
		}
	}
	return out, nil
}

// cleanRel reports whether rel is a clean, relative, in-volume path — the only
// shape a cache key, and therefore an op minted from one, may have.
func cleanRel(rel string) bool {
	if rel == "" || rel == "." || rel == ".." || path.IsAbs(rel) || filepath.IsAbs(rel) {
		return false
	}
	return path.Clean(rel) == rel && !strings.HasPrefix(rel, "../")
}

func (s *Store) SaveCache(mountID string, c map[string]CachedFile) error {
	p, err := s.cachePath(mountID)
	if err != nil {
		return err
	}
	return WriteJSONAtomic(p, c)
}

// ---- sync state (sync.json) ----

// Access records how the hub answered this device on the last cycle that
// reached it. It is persisted so `bdrive status` — which never runs a cycle —
// can report a degraded state, and so the daemon can log a transition once
// instead of on every tick.
const (
	AccessOK       = ""          // normal read+write sync
	AccessReadOnly = "read-only" // pushes refused: pull-only
	AccessNone     = "no-access" // pulls refused too: sync paused
)

type SyncState struct {
	Lamport   int64  `json:"lamport"`
	PushedOps int64  `json:"pushed_ops"`       // how many of our own ops the remote has
	Access    string `json:"access,omitempty"` // "", "read-only", or "no-access"
	// AccessReason is the hub's own words for the last refusal. Without it every
	// 403 renders as the same "read-only (pull only)" line, and the hub's most
	// actionable answer — "this device is not registered to your account on this
	// hub; run `bdrive login`" — reached nobody: the one state a user cannot
	// diagnose from the outside was the one the CLI summarized away.
	AccessReason string `json:"access_reason,omitempty"`

	// IgnoreAccepted is the .bdriveignore text whose scan scope THIS device has
	// accepted, and IgnorePulled is the text a peer's version last wrote here.
	// Together they tell a locally-authored rule change from one that arrived
	// over the wire, which is what keeps a teammate's `!` from widening what
	// leaves this disk. See syncer.Filter.SkipUp.
	IgnoreAccepted string `json:"ignore_accepted,omitempty"`
	IgnorePulled   string `json:"ignore_pulled,omitempty"`
}

func (s *Store) LoadSync() (SyncState, error) {
	var st SyncState
	if err := readJSON(filepath.Join(s.dir, "sync.json"), &st); err != nil {
		return st, err
	}
	return st, nil
}

func (s *Store) SaveSync(st SyncState) error {
	return WriteJSONAtomic(filepath.Join(s.dir, "sync.json"), st)
}

// ---- locking ----

// Lock takes an exclusive flock for the volume, serializing sync cycles
// between the daemon and one-shot commands. Blocks until acquired.
func (s *Store) Lock() (func() error, error) {
	f, err := os.OpenFile(filepath.Join(s.dir, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() error {
		defer f.Close()
		return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}, nil
}

// ---- small JSON helpers ----

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, v)
}

// WriteJSONAtomic writes v as JSON via temp-file + rename. 0600 for the same
// reason as the journals: these files list a private project's paths and the
// accounts that touched them, inside a 0755 $BDRIVE_HOME.
func WriteJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o600)
}

// WriteFileAtomic writes data via a temp file in the same directory + rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
