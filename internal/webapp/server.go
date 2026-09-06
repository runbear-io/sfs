// Package webapp serves the bdrive web server: a browsable web view of
// synced files (file tree reconstructed from the journals, rendered
// markdown, downloads), browser uploads, and — in hub mode — the sync API
// that lets storage-blind client devices sync whole projects through this
// server.
//
// Two modes:
//
//   - single-volume: Source is set (a DirSource for a plain folder, or a
//     RemoteSource in tests); the classic viewer.
//   - hub: Root + Projects are set; the server hosts many projects, each a
//     volume stored under <root>/<project-id>/ in the object store, managed
//     by a file-backed project registry.
//
// The client — browser or syncing device — is deliberately told nothing
// about the storage: no remote URL, bucket, or credentials ever appear in an
// API response.
package webapp

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"mime"
	"net/http"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/secrets"
	"github.com/runbear-io/beardrive/internal/templates"
)

// all:, not a bare `static` — a bare embed silently skips every file whose
// name begins with `_` or `.`, and Vite names shared chunks after the module
// they came from (`_commonjsHelpers-<hash>.js` is the first one this build
// produces). The miss is invisible at build time and total at runtime: the
// chunk 404s, the SPA fallback answers with index.html, and the browser
// refuses the whole entry over its MIME type — a blank app, not a degraded one.
//
//go:embed all:static
var staticFiles embed.FS

// Source supplies the file set and content of one volume. Implementations:
// RemoteSource (a beardrive remote) and DirSource (a plain local folder).
type Source interface {
	Files(ctx context.Context) (map[string]FileInfo, error)
	Open(ctx context.Context, path string, fi FileInfo) (io.ReadCloser, error)
}

// Server renders volumes as a website and, in hub mode, brokers sync for
// client devices.
type Server struct {
	// Single-volume mode: serve exactly this source.
	Source Source
	Volume string // display only

	// Hub mode (when Root is set): many projects on one storage root.
	Root     remote.Backend
	Projects *ProjectDB

	// Device identifies this server in ops it journals for browser uploads.
	Device  Identity
	Refresh time.Duration
	Upload  UploadConfig
	// Auth, when set, gates the whole API behind sign-in. Nil means the
	// historical trusted-network behavior: no accounts, everyone welcome.
	Auth AuthProvider
	// Devices, when set, records what the server observes about syncing
	// devices (name, OS, public IP, last activity) for history.
	Devices *DeviceRegistry
	// Shares, when set, enables public share links (/s/<token>).
	Shares *ShareDB
	// Reads, when set, aggregates read telemetry (viewer, share, and agent
	// reads) for the heat API. Nil means read tracking is off.
	Reads *ReadLedger
	// Dir, when set, walls projects off by organization membership and owns
	// every org read and write the hub performs. LocalDirectory is the
	// built-in implementation; a managed deployment supplies its own so that
	// orgs come from the same place identities do. Nil means single-volume
	// mode: no orgs, every authenticated request passes.
	Dir Directory
	// Quota, when set, enforces plan limits (managed deployments). Nil
	// means UnlimitedQuota: the open-source server never says no.
	Quota QuotaProvider
	// Billing, when set, surfaces a billing entry in the frontend's account
	// menu: the billing page URL plus the signed-in user's current plan name
	// (/api/config `billing`). The OSS hub has no billing; managed
	// deployments plug this in. Nil — or ok=false for a user with no org —
	// hides the entry. The mirror of the Quota seam: Quota enforces the
	// plan, Billing displays it.
	Billing func(email string) (plan, url string, ok bool)
	// Analytics, when its Key is set, tells the frontend to load PostHog
	// (/api/config `analytics`). The third managed-deployment seam beside
	// Quota and Billing, and deliberately server-supplied rather than
	// bundled: with no key the OSS frontend ships no analytics code and
	// makes no third-party request, so a self-hosted hub cannot phone home
	// even by accident.
	Analytics AnalyticsConfig
	// ShareRPM is the per-IP request rate on public share links (/s/*);
	// 0 means DefaultShareRPM.
	ShareRPM int
	// TrustProxy honors X-Forwarded-For from ANY peer. Only needed for a
	// proxy on a public address: a proxy on loopback or a private network is
	// already trusted without it (see clientIP). Setting it on a directly-
	// reachable hub lets any client pick its own rate-limit bucket.
	TrustProxy bool

	xffWarnOnce sync.Once
	// unboundOnce logs, at most once, that this hub refused a journal write
	// while holding no device binding at all — the signature of a provider that
	// never calls the binder. See ownJournal.
	unboundOnce  sync.Once
	shareLimOnce sync.Once
	shareLim     *rateLimiter
	authLimOnce  sync.Once
	authLim      *rateLimiter

	volOnce sync.Once
	vol     *volume

	volsMu sync.Mutex
	vols   map[string]*volume // hub mode: per-project, keyed by project id

	evOnce sync.Once
	ev     *eventHub // live change fan-out (events.go)

	presOnce sync.Once
	pres     *presenceHub // who is looking at what (presence.go)

	resMu  sync.Mutex
	grants []grant // outstanding presigned upload reservations (reserve.go)

	// joinMu serializes invite redemption: the seat check reads the member
	// count and the join adds to it, and two clicks on the same link at the
	// same moment would otherwise both see the last seat free.
	// ponytail: one hub-wide lock; redemption is a rare, human-paced request —
	// make it per-org if that ever stops being true.
	joinMu sync.Mutex
}

// UploadConfig controls whether and how clients may write.
type UploadConfig struct {
	Enabled bool
	// TTL bounds the lifetime of presigned direct-upload URLs.
	TTL time.Duration
}

// AnalyticsConfig points the frontend at a PostHog project. The key is a
// public write-only project token, not a credential — it is served to signed-
// out visitors too, because the app shell loads before login.
type AnalyticsConfig struct {
	Key  string // PostHog project key; empty disables analytics entirely
	Host string // PostHog API host; empty means DefaultAnalyticsHost
}

// DefaultAnalyticsHost is PostHog's US cloud ingestion host.
const DefaultAnalyticsHost = "https://us.i.posthog.com"

// Endpoint is Host with the default applied. Exported because the same
// config drives more than the app shell in a managed deployment (the cloud
// module's marketing pages render their own loader from it).
func (a AnalyticsConfig) Endpoint() string {
	if a.Host != "" {
		return a.Host
	}
	return DefaultAnalyticsHost
}

// DefaultUploadTTL is used when UploadConfig.TTL is unset: long enough for a
// slow upload, short enough that a leaked URL goes stale quickly.
const DefaultUploadTTL = 15 * time.Minute

func (c UploadConfig) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultUploadTTL
}

// FileInfo is the resolved state of one path: content identity (Blob doubles
// as the ETag), plus provenance where the source knows it.
type FileInfo struct {
	Blob string
	Size int64
	Time time.Time
	// User/UserName are the signed-in account behind the change; Author is
	// the git/OS identity an offline device falls back to. History renders
	// the account and falls back to Author, so the viewer needs all three
	// to give the same answer — see whoChanged() in the frontend.
	User     string
	UserName string
	Author   string
	Device   string
}

// volume is one browsable/syncable file set: a source plus its snapshot
// cache. File listings are cached for refresh between fetches; if the source
// becomes unreachable, the last good snapshot keeps being served.
type volume struct {
	source  Source
	refresh time.Duration

	mu   sync.Mutex
	snap *snapshot
	at   time.Time
}

type snapshot struct {
	files map[string]FileInfo
	// moves is the derived rename index (see moves.go), cached with the
	// listing it was replayed alongside. Nil for a source that has no
	// journals to derive it from — every resolver then answers "not found",
	// so the DirSource exclusion falls out instead of needing a rule.
	moves moveIndex
}

// MoveSource is a Source that can also report where its files came from.
// Optional, like Uploader: implementing it keeps the replay ONE pass, so the
// move index costs no extra journal read.
type MoveSource interface {
	FilesWithMoves(context.Context) (map[string]FileInfo, moveIndex, error)
}

func (v *volume) snapshot(ctx context.Context) (*snapshot, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.snap != nil && time.Since(v.at) < v.refresh {
		return v.snap, nil
	}
	var (
		files map[string]FileInfo
		moves moveIndex
		err   error
	)
	if ms, ok := v.source.(MoveSource); ok {
		files, moves, err = ms.FilesWithMoves(ctx)
	} else {
		files, err = v.source.Files(ctx)
	}
	if err != nil {
		if v.snap != nil {
			return v.snap, nil // serve stale rather than fail
		}
		return nil, err
	}
	v.snap, v.at = &snapshot{files: files, moves: moves}, time.Now()
	return v.snap, nil
}

// invalidate forces the next snapshot to refetch, so an upload shows up in
// the tree immediately instead of after refresh.
func (v *volume) invalidate() {
	v.mu.Lock()
	v.at = time.Time{}
	v.mu.Unlock()
}

func (v *volume) uploader() Uploader {
	u, _ := v.source.(Uploader)
	return u
}

// single returns the single-volume mode volume.
func (s *Server) single() *volume {
	s.volOnce.Do(func() {
		s.vol = &volume{source: s.Source, refresh: s.Refresh}
	})
	return s.vol
}

// projectVolume resolves a project id to its record and its volume, creating
// the (cached) source over the project's storage prefix on first use. It
// returns the Project so the permission check does not have to resolve the id
// a second time — see projectPermOf.
func (s *Server) projectVolume(id string) (Project, *volume, error) {
	if s.Root == nil || s.Projects == nil {
		return Project{}, nil, fmt.Errorf("this server does not host projects")
	}
	if !projectIDRe.MatchString(id) {
		return Project{}, nil, fmt.Errorf("invalid project id %q", id)
	}
	p, ok := s.Projects.Get(id)
	if !ok {
		return Project{}, nil, fmt.Errorf("no such project %q", id)
	}
	s.volsMu.Lock()
	defer s.volsMu.Unlock()
	if s.vols == nil {
		s.vols = make(map[string]*volume)
	}
	v, ok := s.vols[id]
	if !ok {
		v = &volume{
			source: &RemoteSource{
				Backend: remote.Prefixed(s.Root, id), Device: s.Device,
				// The real TTL the presign doors hand out, so verify seals a
				// blob no earlier than the last URL for it can expire.
				PresignTTL: s.Upload.ttl(),
			},
			refresh: s.Refresh,
		}
		s.vols[id] = v
	}
	return p, v, nil
}

// RemoteSource reads a beardrive remote: it fetches every journal and folds the
// ops into the current volume state (same total order as journal.Replay,
// but keeping author/device/time of the winning op per path). With Device set
// it also accepts uploads, journaled under that identity.
type RemoteSource struct {
	Backend remote.Backend
	// Device identifies this server in ops it journals for uploads. Required
	// for uploads; irrelevant for reading.
	Device Identity
	// PresignTTL is the lifetime the hub gives a presigned upload URL. It is
	// how long a blob stays writable by anyone but the hub, and therefore when
	// verify may stop re-hashing it. Zero means DefaultUploadTTL — set it to
	// the server's real UploadConfig.ttl(), or a longer configured TTL would
	// seal an object that can still change.
	PresignTTL time.Duration

	upmu sync.Mutex // serializes read-modify-write of our own journal
	// sealed holds the blobs this process has verified AND proved immutable.
	// See verify.
	sealed sync.Map // sha (string) → struct{}

	jmu    sync.Mutex               // guards jcache and jbytes
	jcache map[string]cachedJournal // "journal/<dev>.jsonl" → parsed ops
	jbytes int64                    // raw journal bytes currently cached
}

// cachedJournal is one journal's parsed ops plus the (size, modified) that
// proves the parse is still current. See loadSourcedOps.
type cachedJournal struct {
	size  int64
	mod   time.Time
	bytes int64 // raw bytes this entry stands for, for the ceiling
	ops   []journal.Op
}

// journalFetchConcurrency bounds the parallel journal fetches on a cold load.
// A cache can never help a first request, and that loop is one serial round
// trip to S3 per device that has ever synced the project.
const journalFetchConcurrency = 8

// ponytail: a per-project raw-byte cap with all-or-nothing eviction, so the
// real ceiling is maxCachedJournalBytes x the number of projects a hub has
// touched since start — s.vols never evicts. Closing it properly means one
// budget shared across RemoteSources with LRU, or reading only the appended
// tail of each journal, which makes the cache small in the first place.
const maxCachedJournalBytes = 64 << 20

// OpenBlob is the one way a blob's bytes leave the hub, and on a hub whose
// storage can presign it is also the only place left that can tell a content
// address the truth. handleStorePut hashes what it relays — but a presigned
// PUT writes straight into the object store, so those bytes were never
// examined by anything: any device with write permission could store arbitrary
// content under a sha256 it chose, and the viewer, share links, history and
// every peer would then serve it.
//
// Skipped entirely on a backend that cannot presign, where the write path
// already checked. It used to be once per blob per process, on the premise
// that blobs are immutable — which is false on the hub that needs the check:
// SignPut hands out a URL that stays valid for its whole TTL and an object
// store accepts every PUT to it, not the first. So uploading the honest bytes,
// letting one reader populate the cache, and then replaying the same URL with
// hostile bytes served them under the reviewed sha to the viewer, history,
// share links and every syncing device.
//
// Verifying on EVERY read closed that, and cost every S3/GCS hub 2x object-
// store egress and a serialized full-object hash before the reader's first
// byte — on every viewer open, render, download and /s/* hit. The cache is
// back, keyed on the one thing that makes the premise TRUE rather than assumed:
// see verify.
func (r *RemoteSource) OpenBlob(ctx context.Context, sha string) (io.ReadCloser, error) {
	if !blobRe.MatchString(sha) {
		return nil, fmt.Errorf("invalid content reference")
	}
	if err := r.verify(ctx, sha); err == nil {
		if rc, err := r.Backend.Get(ctx, "blobs/"+sha); err == nil {
			return rc, nil
		}
	}
	// No whole blob (or one that fails verification): the content may exist
	// only as chunks + a manifest (delta sync, docs/delta-sync-prd.md).
	// Reassembly is hash-verified against the sha requested, so this arm is
	// exactly as strong as verify — including when blobs/<sha> holds bytes
	// that do NOT hash to sha, where the backfill below heals it.
	return r.reassemble(ctx, sha)
}

// maxReassembleBytes caps what one reassembly may spool. It mirrors `bdrive
// import`'s maxImportBlob (256 << 20): comfortably above the device-side
// materialization ceiling (syncer's maxPullBytes, 100 MiB), so nothing a
// device can hold is refused here, while a hostile manifest cannot spool
// gigabytes through the temp dir per read. The spool lands in os.TempDir —
// on a tmpfs deployment that is RAM, so this constant is also the per-request
// memory bound; set TMPDIR to disk if that matters.
const maxReassembleBytes = 256 << 20

// reassemble serves a blob that exists only as chunks: fetch the manifest
// keyed by the file's sha, concatenate its chunks into a spool while hashing,
// refuse the lot unless the result hashes to the sha requested, then backfill
// blobs/<sha> so the next read is a plain Get. The spool is unavoidable — the
// promise "bytes hash to the key" cannot be made about a stream whose response
// has already started — and self-limiting: upgraded clients fetch chunks and
// verify locally, so only legacy readers ever pay it, once per blob.
func (r *RemoteSource) reassemble(ctx context.Context, sha string) (io.ReadCloser, error) {
	mrc, err := r.Backend.Get(ctx, "manifests/"+sha)
	if err != nil {
		return nil, err // no manifest either: the object genuinely is not there
	}
	var man struct {
		Chunks []struct {
			H string `json:"h"`
			N int64  `json:"n"`
		} `json:"chunks"`
	}
	derr := json.NewDecoder(io.LimitReader(mrc, 8<<20)).Decode(&man)
	mrc.Close()
	if derr != nil {
		return nil, fmt.Errorf("unreadable manifest for %s: %w", sha, derr)
	}
	// A manifest is member-written and cannot be hash-checked at ingest, so
	// its declared total is the one number a hostile member controls: listing
	// one real 4 MiB chunk 100k times would spool ~400 GB through the temp
	// dir (RAM, on a tmpfs deployment) per legacy read. The sum is enforced
	// here and the per-chunk copy below verifies declared == actual, so the
	// declared total IS the spool bound. An honest manifest past the cap is
	// refused the same way — the hub will not assemble what a device would
	// not accept either (the client's own ceiling is maxPullBytes).
	var declared int64
	for _, c := range man.Chunks {
		if c.N < 0 || declared > maxReassembleBytes-c.N {
			return nil, fmt.Errorf("manifest for %s declares more than %d bytes", sha, int64(maxReassembleBytes))
		}
		declared += c.N
	}
	tmp, err := os.CreateTemp("", ".bdrive-reassemble-*")
	if err != nil {
		return nil, err
	}
	os.Remove(tmp.Name()) // serve from the fd; nothing to clean up on any path
	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	for _, c := range man.Chunks {
		if !blobRe.MatchString(c.H) {
			tmp.Close()
			return nil, fmt.Errorf("manifest for %s names an invalid chunk", sha)
		}
		crc, err := r.Backend.Get(ctx, "chunks/"+c.H)
		if err != nil {
			tmp.Close()
			return nil, err
		}
		// Bounded by the DECLARED size, which the cap above already summed —
		// a stored object longer than its manifest entry (a replayed presign)
		// must not turn the cap into fiction. Short or long, the mismatch
		// fails here; equal-but-wrong bytes fail the whole-file hash below.
		n, cerr := io.Copy(w, io.LimitReader(crc, c.N+1))
		crc.Close()
		if cerr != nil {
			tmp.Close()
			return nil, cerr
		}
		if n != c.N {
			tmp.Close()
			return nil, fmt.Errorf("chunk %s of %s is not its declared size", c.H[:12], sha)
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != sha {
		tmp.Close()
		return nil, fmt.Errorf("manifest for %s does not reassemble to its key", sha)
	}
	size, err := tmp.Seek(0, io.SeekEnd)
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		tmp.Close()
		return nil, err
	}
	// Backfill so the next read is a plain blobs/ Get. Best-effort: a failed
	// write must never fail the read that triggered it. The object is fresh,
	// so verify will re-hash it until it seals — same as any new upload.
	if err := r.Backend.Put(ctx, "blobs/"+sha, tmp, size); err != nil {
		log.Printf("beardrive: backfill of reassembled blob %s failed: %v", sha, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		return nil, err
	}
	return tmp, nil
}

// verify re-hashes a stored blob, unless this process has already proved that
// nobody but the hub can write it any more.
//
// The proof is the presign TTL and the fact that BOTH presign doors —
// handleStoreSign and handleUploadInit — refuse to sign a key that already
// exists. So every presigned URL a blob ever gets was minted BEFORE its first
// PUT, and expires at mint+TTL, which is earlier than firstPUT+TTL. Once the
// stored object is older than the TTL, no live URL for it can exist and none
// will ever be minted again: the hub is the only writer left, and the hub
// hashes what it relays. That is when the object really is immutable, and only
// then is the verification cached — for the life of the process, keyed on the
// sha, no expiry needed.
//
// The age is read AFTER the hash on purpose. A replay lands a NEW object with
// a new modification time, so an object that was rewritten mid-check reads as
// seconds old and is not sealed.
//
// Two premises this rests on, both true today and both worth breaking loudly:
// blobs are never deleted (remote.Backend has no delete at all — history keeps
// every version forever), and PresignTTL is the real TTL the doors use. A
// backend that does not report Modified never seals, which is the safe answer.
//
// ponytail: per-process, so the first read of each blob after a restart still
// pays the full hash. Persisting it needs somewhere to record "the hub has
// seen these bytes", which is a metadata-store change for a cost paid once.
func (r *RemoteSource) verify(ctx context.Context, sha string) error {
	if _, canSign := r.Backend.(remote.PutSigner); !canSign {
		return nil
	}
	if _, ok := r.sealed.Load(sha); ok {
		return nil
	}
	rc, err := r.Backend.Get(ctx, "blobs/"+sha)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, rc)
	rc.Close()
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != sha {
		return fmt.Errorf("stored content does not hash to its key")
	}
	// sealAfter, not presignTTL: o.Modified is the STORAGE service's clock and
	// time.Since is the hub's. A hub whose clock runs ahead of storage would
	// otherwise overstate the object's age and seal it while a minted URL is
	// still live — after which a replay is served from cache for the life of
	// the process.
	if o, ok, err := r.blobStat(ctx, sha); err == nil && ok &&
		!o.Modified.IsZero() && time.Since(o.Modified) > r.sealAfter() {
		r.sealed.Store(sha, struct{}{})
	}
	return nil
}

// sealAfter is how old a stored blob must be before its verification may be
// cached: the presign TTL plus an allowance for clock skew between the hub and
// the object store. The allowance is the whole point — the correctness
// argument is "no live URL can exist any more", and that is a claim about time
// measured on two machines the hub cannot reconcile.
//
// Waiting longer costs only a few extra hashes on a blob younger than this,
// which is the behavior the check had for every blob anyway.
//
// ponytail: a fixed allowance, so it is a bound and not a proof — a hub whose
// clock runs more than an hour ahead of its object store can still seal early.
// Closing it properly means measuring the age on ONE clock (record the hub time
// of the first verification, seal on a later one that finds Modified
// unchanged), which costs a second map and never seals on a first read.
func (r *RemoteSource) sealAfter() time.Duration {
	const skewAllowance = time.Hour
	return r.presignTTL() + skewAllowance
}

// Identity is the device identity uploads are journaled under.
type Identity struct {
	ID, Name, Author string
}

// loadOps fetches and parses every journal on the remote.
func (r *RemoteSource) loadOps(ctx context.Context) ([]journal.Op, error) {
	sourced, err := r.loadSourcedOps(ctx)
	if err != nil {
		return nil, err
	}
	all := make([]journal.Op, len(sourced))
	for i, s := range sourced {
		all[i] = s.Op
	}
	return all, nil
}

// sourcedOp is an op plus the device whose journal it was actually read from.
// Everything inside an op — including its Device field — is JSON the pusher
// chose; the journal KEY is the one part the hub binds to the pushing device
// (store.go's ownJournal), so From is the only trustworthy attribution.
type sourcedOp struct {
	Op   journal.Op
	From string
}

// loadSourcedOps folds every journal on the remote into one slice of ops. It
// is the funnel every reader goes through — history, folder listings, restore,
// and the hub's own appendOp — so re-downloading and re-parsing every journal
// here was the whole cost of a history page, paid again per "load more".
//
// Journals only ever GROW: a device appends only to its own (the one-writer
// invariant) and the hub's appendOp rewrites its key with strictly more bytes.
// Nothing shrinks or rewrites one in place, so the (Size, Modified) that List
// already reports proves a parse we still hold is current — no staleness
// window, no time-based expiry, and no new Backend method. The List is the one
// round trip every request still pays.
//
// No singleflight: two requests that miss the same journal at the same moment
// both fetch it, which is what every request did before this existed.
func (r *RemoteSource) loadSourcedOps(ctx context.Context) ([]sourcedOp, error) {
	objs, err := r.Backend.List(ctx, "journal/")
	if err != nil {
		return nil, fmt.Errorf("list journals: %w", err)
	}
	keep := make([]remote.Object, 0, len(objs))
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".jsonl") {
			keep = append(keep, o)
		}
	}

	parsed := make([][]journal.Op, len(keep))
	var misses []int
	r.jmu.Lock()
	// Pruning here rather than after the fetch: a journal that VANISHED is a
	// pass with no misses at all, so a prune on the store path would never see
	// it and the entry would stay resident for the life of the process.
	live := make(map[string]bool, len(keep))
	for i, o := range keep {
		live[o.Key] = true
		if c, ok := r.jcache[o.Key]; ok && c.size == o.Size && c.mod.Equal(o.Modified) {
			parsed[i] = c.ops
			continue
		}
		misses = append(misses, i)
	}
	for k, c := range r.jcache {
		if !live[k] {
			r.jbytes -= c.bytes
			delete(r.jcache, k)
		}
	}
	r.jmu.Unlock()

	if len(misses) > 0 {
		sizes := make([]int64, len(keep))
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(journalFetchConcurrency)
		for _, i := range misses {
			g.Go(func() error {
				rc, err := r.Backend.Get(gctx, keep[i].Key)
				if err != nil {
					return fmt.Errorf("fetch %s: %w", keep[i].Key, err)
				}
				data, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					return err
				}
				sizes[i] = int64(len(data))
				// A corrupt journal is ignored rather than breaking the view —
				// and cached as zero ops, or it is re-downloaded on every
				// request for as long as it stays corrupt.
				if ops, err := journal.Parse(data); err == nil {
					parsed[i] = ops
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
		r.cacheJournals(keep, misses, parsed, sizes)
	}

	// A fresh outer slice on every call: handleHistory and Files both sort what
	// they are handed, and the cached ops must never be the thing they reorder.
	// journal.Op is all value fields, so appending one copies it.
	var all []sourcedOp
	for i, o := range keep {
		from := strings.TrimSuffix(strings.TrimPrefix(o.Key, "journal/"), ".jsonl")
		for _, op := range parsed[i] {
			all = append(all, sourcedOp{Op: op, From: from})
		}
	}
	return all, nil
}

// cacheJournals records what the fetch pass parsed, and enforces the byte
// ceiling by dropping everything — a cold pass costs what every pass cost
// before this cache existed, so the fallback is today's behavior, not an error.
func (r *RemoteSource) cacheJournals(keep []remote.Object, misses []int, parsed [][]journal.Op, sizes []int64) {
	r.jmu.Lock()
	defer r.jmu.Unlock()
	if r.jcache == nil {
		r.jcache = make(map[string]cachedJournal, len(keep))
	}
	for _, i := range misses {
		if c, ok := r.jcache[keep[i].Key]; ok {
			r.jbytes -= c.bytes
		}
		r.jcache[keep[i].Key] = cachedJournal{
			size: keep[i].Size, mod: keep[i].Modified, bytes: sizes[i], ops: parsed[i],
		}
		r.jbytes += sizes[i]
	}
	if r.jbytes > maxCachedJournalBytes {
		r.jcache, r.jbytes = nil, 0
	}
}

func (r *RemoteSource) Files(ctx context.Context) (map[string]FileInfo, error) {
	files, _, err := r.FilesWithMoves(ctx)
	return files, err
}

// FilesWithMoves is the replay, plus the rename index derived from the same
// sorted ops — one pass, so the index rides in the cached snapshot.
func (r *RemoteSource) FilesWithMoves(ctx context.Context) (map[string]FileInfo, moveIndex, error) {
	all, err := r.loadOps(ctx)
	if err != nil {
		return nil, nil, err
	}
	journal.Sort(all)
	files := make(map[string]FileInfo)
	for _, op := range all {
		switch op.Kind {
		case journal.KindPut:
			// A journal is arbitrary JSONL a device pushed: Blob is a storage
			// key suffix ("blobs/"+Blob), not a checked field, so anything but
			// a bare sha256 is a path the writer chose — another project's
			// prefix, or out of the storage root entirely. Same rule as every
			// ?sha= route. An op that fails it is ignored (the path keeps its
			// previous version) rather than treated as a delete.
			if !blobRe.MatchString(op.Blob) {
				continue
			}
			files[op.Path] = FileInfo{
				Blob: op.Blob, Size: op.Size, Time: op.Time,
				User: op.User, UserName: op.UserName,
				Author: op.Author, Device: op.DeviceName,
			}
		case journal.KindDelete:
			delete(files, op.Path)
		}
	}
	return files, buildMoveIndex(all), nil
}

func (r *RemoteSource) Open(ctx context.Context, _ string, fi FileInfo) (io.ReadCloser, error) {
	// Files already drops ops with a bogus Blob; re-checked in OpenBlob because
	// that is where the key is built, and a FileInfo can reach it from anywhere.
	return r.OpenBlob(ctx, fi.Blob)
}

// Handler returns the HTTP handler: /api/* plus the embedded frontend.
func (s *Server) Handler() http.Handler {
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // embedded FS; cannot fail at runtime
	}
	mux := http.NewServeMux()

	// One account-removal path, one cleanup. Everything downstream of it is
	// keyed by email, so removal has to take the org role, the project grants
	// and (through membership) the share links with it.
	if a, ok := s.Auth.(*BuiltinAuth); ok && a.Offboard == nil {
		a.Offboard = s.offboard
	}
	// A device identity is bound to an account when its token is minted, and
	// nowhere else. Wired here rather than at startup because the fixtures (and
	// a hub rebuilt from its repos) assemble Auth and Devices independently.
	//
	// EVERY provider, not `if a, ok := s.Auth.(*BuiltinAuth); ok`. ownJournal
	// refuses a journal write for any provider, and the type assertion meant a
	// hub running a managed provider enforced a gate nothing could ever satisfy:
	// no device bound, every push 403 forever, while login and permissions read
	// perfectly healthy. See AuthProvider.UseDeviceBinder.
	if s.Auth != nil {
		s.Auth.UseDeviceBinder(s.bindDevice)
	}

	// Volume resolution per route family: fixed single volume, or by
	// project id in hub mode. One handler implementation serves both.
	// Single-volume mode has no per-project permissions, so it ignores the
	// declared level; hub mode enforces it.
	single := func(_ string, h func(*volume, http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if s.Source == nil {
				http.Error(w, "this server hosts projects; use /api/p/<project-id>/...", http.StatusNotFound)
				return
			}
			h(s.single(), w, r)
		}
	}
	proj := func(level string, h func(*volume, http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("project")
			p, v, err := s.projectVolume(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			// p, not id: the resolver has already read the registry once for
			// this request and re-reading it is the hub's per-request cost.
			if !s.requirePermOn(w, r, p, level) {
				return
			}
			// Read recording (and anything else downstream) finds the project
			// id in the context; permission has already passed at this point.
			h(v, w, withProjectID(r, id))
		}
	}

	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/projects", s.handleProjectList)
	mux.HandleFunc("POST /api/projects", s.handleProjectCreate)
	mux.HandleFunc("GET /api/projects/{project}", s.handleProjectGet)

	for prefix, resolve := range map[string]func(string, func(*volume, http.ResponseWriter, *http.Request)) http.HandlerFunc{
		"/api/":             single,
		"/api/p/{project}/": proj,
	} {
		mux.HandleFunc("GET "+prefix+"tree", resolve(PermRead, s.handleTree))
		mux.HandleFunc("GET "+prefix+"resolve", resolve(PermRead, s.handleResolve))
		mux.HandleFunc("GET "+prefix+"file", resolve(PermRead, s.handleFile))
		mux.HandleFunc("GET "+prefix+"download", resolve(PermRead, s.handleDownload))
		mux.HandleFunc("GET "+prefix+"render", resolve(PermRead, s.handleRender))
		// Reading the change stream is reading the project: it names paths,
		// so it sits behind the same permission the tree does.
		mux.HandleFunc("GET "+prefix+"events", resolve(PermRead, s.handleEvents))
		// Saying "I am reading this" is not a write — a read-only member is
		// exactly who a teammate most wants to see on a file.
		mux.HandleFunc("POST "+prefix+"presence", resolve(PermRead, s.handlePresence))
		mux.HandleFunc("POST "+prefix+"upload/init", resolve(PermWrite, s.handleUploadInit))
		mux.HandleFunc("PUT "+prefix+"upload/content", resolve(PermWrite, s.handleUploadContent))
		mux.HandleFunc("POST "+prefix+"upload/commit", resolve(PermWrite, s.handleUploadCommit))
	}

	mux.HandleFunc("GET /api/orgs", s.handleOrgList)
	mux.HandleFunc("PATCH /api/orgs/{org}", s.handleOrgRename)
	mux.HandleFunc("POST /api/orgs/{org}/invites", s.handleInviteCreate)
	mux.HandleFunc("GET /api/orgs/{org}/invites", s.handleInviteList)
	mux.HandleFunc("DELETE /api/orgs/{org}/invites/{token}", s.handleInviteRevoke)
	mux.HandleFunc("PATCH /api/orgs/{org}/members/{email}", s.handleMemberUpdate)
	mux.HandleFunc("DELETE /api/orgs/{org}/members/{email}", s.handleMemberRemove)
	mux.HandleFunc("GET /api/orgs/{org}/shares", s.handleOrgShares)
	mux.HandleFunc("POST /api/invites/{token}", s.handleInviteAccept)

	mux.HandleFunc("PATCH /api/projects/{project}", s.handleProjectUpdate)
	mux.HandleFunc("DELETE /api/projects/{project}", s.handleProjectDelete)

	mux.HandleFunc("GET /api/admin/policy", s.handleAdminPolicy)
	mux.HandleFunc("POST /api/admin/policy", s.handleAdminPolicy)
	mux.HandleFunc("GET /api/admin/pending", s.handleAdminPending)
	mux.HandleFunc("POST /api/admin/pending/{id}/approve", s.handleAdminApprove)
	mux.HandleFunc("POST /api/admin/pending/{id}/deny", s.handleAdminDeny)

	mux.HandleFunc("GET /api/p/{project}/history", proj(PermRead, s.handleHistory))
	mux.HandleFunc("GET /api/p/{project}/blob", proj(PermRead, s.handleBlob))
	// Restore needs a journal to look the version up in, so it exists only
	// per project — never on the single-volume (DirSource) prefix. Remove
	// writes to that same journal, so it lives here too.
	mux.HandleFunc("POST /api/p/{project}/restore", proj(PermWrite, s.handleRestore))
	mux.HandleFunc("POST /api/p/{project}/remove", proj(PermWrite, s.handleRemove))
	// The run-wide form of the two above: one journal write that puts every
	// path an agent run touched back where it was.
	mux.HandleFunc("POST /api/p/{project}/undo-run", proj(PermWrite, s.handleUndoRun))
	mux.HandleFunc("GET /api/p/{project}/heat", proj(PermRead, s.handleHeat))
	mux.HandleFunc("POST /api/p/{project}/reads", proj(PermRead, s.handleReadReport))
	mux.HandleFunc("POST /api/p/{project}/shares", proj(PermWrite, s.handleShareCreate))
	mux.HandleFunc("GET /api/p/{project}/shares", proj(PermRead, s.handleShareList))
	mux.HandleFunc("PATCH /api/shares/{token}", s.handleShareExpiry)
	mux.HandleFunc("DELETE /api/shares/{token}", s.handleShareRevoke)
	mux.HandleFunc("GET /s/{token}", s.handleShared)

	mux.HandleFunc("GET /api/p/{project}/permissions", s.handleProjectPerms)
	mux.HandleFunc("PUT /api/p/{project}/permissions", s.handleProjectPermDefault)
	mux.HandleFunc("PUT /api/p/{project}/permissions/{email}", s.handleProjectPermSet)
	mux.HandleFunc("DELETE /api/p/{project}/permissions/{email}", s.handleProjectPermClear)

	// The sync (store) API only exists per project: hub mode is what
	// storage-blind devices sync through. Reading the store is how a
	// pull-only (read) device stays current; writing needs write.
	mux.HandleFunc("GET /api/p/{project}/store/list", proj(PermRead, s.handleStoreList))
	mux.HandleFunc("GET /api/p/{project}/store/object", proj(PermRead, s.handleStoreGet))
	mux.HandleFunc("GET /api/p/{project}/store/exists", proj(PermRead, s.handleStoreExists))
	mux.HandleFunc("POST /api/p/{project}/store/sign", proj(PermWrite, s.handleStoreSign))
	mux.HandleFunc("PUT /api/p/{project}/store/object", proj(PermWrite, s.handleStorePut))

	mux.Handle("GET /", s.frontend(static))
	if s.Auth != nil {
		s.Auth.Register(mux)
	}
	return s.rateLimitAuth(s.authGate(mux))
}

// frontend serves the embedded single-page app. Real asset files (app.js,
// style.css) are served directly; every other GET that isn't an API, auth,
// or share route — or, on a hub, a root-level path shaped like a file —
// returns index.html, so client-side routes like /<project-id>/<path> and
// /join/<token> survive a deep link or refresh.
func (s *Server) frontend(static fs.FS) http.HandlerFunc {
	files := http.FileServerFS(static)
	index, _ := fs.ReadFile(static, "index.html")
	return func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		// This document carries the session cookie and drives share creation,
		// permission edits and project deletion, so it must not be framed by
		// another origin or MIME-sniffed. /s/* sets its own sandbox CSP and
		// never reaches here.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		// Vite emits content-hashed filenames under assets/, safe to cache
		// forever. Everything else (index.html above all) must revalidate:
		// embedded files carry no modtime, so without no-cache browsers
		// cache heuristically and users see a stale frontend after upgrades.
		// Set on the real asset only, below: deciding on the URL prefix meant
		// a MISS under assets/ answered the app shell marked immutable for a
		// year, so a shared cache pinned index.html at an asset URL forever.
		w.Header().Set("Cache-Control", "no-cache")
		// Reserved prefixes that fell through to the catch-all are genuine
		// 404s — don't mask a mistyped API/auth/share URL with the app shell.
		if strings.HasPrefix(upath, "api/") || strings.HasPrefix(upath, "auth/") || strings.HasPrefix(upath, "s/") {
			http.NotFound(w, r)
			return
		}
		// A hub whose organizations live elsewhere has no org page to show:
		// send the browser where they are actually administered rather than
		// painting a console whose every control would 409. The account menu
		// already links to the same place; this covers bookmarks, history, and
		// hand-typed URLs, which are the paths a link cannot reach.
		if id, ok := strings.CutPrefix(upath, "orgs/"); ok && s.Dir != nil {
			if u := s.Dir.ManageURL(id); !strings.HasPrefix(u, "/") {
				http.Redirect(w, r, u, http.StatusFound)
				return
			}
		}
		if upath != "" && upath != "index.html" {
			if f, err := static.Open(upath); err == nil {
				fi, statErr := f.Stat()
				f.Close()
				if statErr == nil && !fi.IsDir() {
					if strings.HasPrefix(upath, "assets/") {
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					}
					// The /s/ share page runs under `sandbox allow-scripts`, so
					// its origin is opaque: a module script and every import()
					// it makes are fetched with `Origin: null` and blocked
					// without this. That is how share-mermaid.js and mermaid's
					// chunks reach a share page at all.
					//
					// On the real asset ONLY, never on the index.html fallback
					// below: these files are already public, unauthenticated
					// and cookie-less, and `*` forbids credentialed requests by
					// definition, so this grants a cross-origin reader nothing
					// it could not already fetch in no-cors mode.
					w.Header().Set("Access-Control-Allow-Origin", "*")
					files.ServeHTTP(w, r) // a real asset
					return
				}
			}
		}
		// No embedded asset matched, so a root-level dotted path is a request
		// for a file that does not exist, not a client route: in hub mode the
		// first segment is a project id (projectIDRe: UUID or p-xxxxxxxx) or a
		// reserved word (orgs/, billing/, join/), none of which contain a dot.
		// Answering the shell made /llms.txt, /robots.txt and every mistyped
		// root file look like they exist — a soft 200 of login HTML to any
		// crawler probing a conventional path. Deeper dots (/<id>/notes/a.md)
		// are real client routes and untouched. index.html is excluded because
		// the asset block above skips it deliberately and it must keep
		// answering the shell.
		if s.Root != nil && upath != "index.html" &&
			!strings.Contains(upath, "/") && strings.Contains(upath, ".") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	}
}

// handleConfig tells the client how this server is configured. Deliberately
// nothing about the storage backend.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	mode := "volume"
	if s.Root != nil {
		mode = "hub"
	}
	auth := map[string]any{"enabled": s.Auth != nil}
	if s.Auth != nil {
		auth["cli_login"] = s.Auth.CLILoginPath()
	}
	// Tell the frontend whether self-signup is offered and whether the
	// signed-in user is a hub admin, so it can hide the "Sign up" link and
	// show the admin surfaces. Never leak more than these booleans.
	me := s.requestUser(r)
	brand := ""
	if a, ok := s.Auth.(AccountApprover); ok {
		// Only a hub that owns its accounts can offer self-signup or an admin
		// queue; one whose identities come from elsewhere offers neither.
		auth["allow_signup"] = a.Policy().AllowSignup
		auth["admin"] = me.Admin
	}
	if b, ok := s.Auth.(Brander); ok {
		brand = b.Branding()
	}
	// No fallback: the volume is a storage basename, not a brand. An
	// unconfigured brand stays empty and each app picks its own default
	// (hub: "BearDrive", volume mode: the folder name).
	out := map[string]any{
		"mode":   mode,
		"volume": s.Volume,
		"brand":  brand,
		"upload": map[string]any{
			"enabled": s.Upload.Enabled,
		},
		"auth":  auth,
		"reads": map[string]any{"enabled": s.Reads != nil},
		// The starting structures the create dialog offers. Served rather
		// than hardcoded in the frontend so a hub that ships another one
		// needs no frontend change.
		"templates": templates.List(),
	}
	// Outside a managed deployment this block is absent and the frontend
	// never loads a tracker. Outside the `me` check on purpose: a hub with
	// auth off has no signed-in user and should still be measurable.
	// Note the funnel gap this leaves — /auth/* is server-rendered HTML
	// (authlocal.go authPage) with no analytics, so a visitor is counted on
	// the marketing page and again once the app boots, but the signup page
	// itself reports nothing. Same origin means the anonymous id survives
	// the round trip, so attribution holds; only signup-page drop-off is
	// invisible. Wire authPage up if that becomes the question.
	if s.Analytics.Key != "" {
		out["analytics"] = map[string]string{"key": s.Analytics.Key, "host": s.Analytics.Endpoint()}
	}
	if me.Email != "" {
		out["me"] = map[string]string{"email": me.Email, "name": me.Name}
		if s.Billing != nil {
			if plan, url, ok := s.Billing(me.Email); ok {
				out["billing"] = map[string]string{"plan": plan, "url": url}
			}
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return
	}
	// Each row carries the caller's own level, so the frontend can hide write
	// affordances without a second fetch per project on every render.
	visible := []projectView{}
	for _, p := range s.Projects.List() {
		perm := s.projectPermOf(r, p)
		if !atLeast(perm, PermRead) {
			continue
		}
		visible = append(visible, projectJSON(p, perm))
	}
	writeJSON(w, map[string]any{"projects": visible})
}

// projectJSON renders a project for the API with the caller's effective level.
// projectView is a Project plus the caller's own effective level on it.
// It embeds rather than re-listing fields on purpose: hand-listing them means
// every new Project field silently fails to reach the client until someone
// remembers to add it here.
type projectView struct {
	Project
	Perm string `json:"perm"`
}

func projectJSON(p Project, perm string) projectView {
	// The grant list and the default belong to /api/p/{id}/permissions, which
	// has its own gate; they'd be noise on every row of every project list.
	p.Perms, p.Default = nil, ""
	return projectView{p, perm}
}

func (s *Server) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return
	}
	p, ok := s.Projects.Get(r.PathValue("project"))
	perm := s.projectPermOf(r, p)
	if !ok || !atLeast(perm, PermRead) {
		http.Error(w, "no such project", http.StatusNotFound)
		return
	}
	writeJSON(w, projectJSON(p, perm))
}

// handleProjectCreate creates a project by name, or returns the existing one
// with that name (create-or-join). Creating is a write, so it follows the
// upload setting.
func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	if s.Projects == nil {
		http.Error(w, "this server does not host projects", http.StatusNotFound)
		return
	}
	if !s.Upload.Enabled {
		http.Error(w, "this server is read-only; projects cannot be created", http.StatusForbidden)
		return
	}
	var req struct {
		Name string `json:"name"`
		Org  string `json:"org,omitempty"`
		// Template is the starting structure to seed, "" for an empty
		// project (the historical behavior).
		Template string `json:"template,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Resolve the template before anything is created, so an unknown name
	// leaves no project behind.
	var tpl templates.Template
	if req.Template != "" {
		var err error
		if tpl, err = templates.Get(req.Template); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	org, err := s.orgForCreate(r, req.Org)
	if err != nil {
		if errors.Is(err, ErrManagedElsewhere) {
			// A user with no organization on a hub that cannot create one:
			// send them where organizations actually come from, rather than a
			// 403 naming an org that does not exist.
			s.writeDirErr(w, "", err)
			return
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	p, created, err := s.Projects.GetOrCreate(req.Name, org)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if created {
		// The creator is the project's first admin. Both writes are
		// best-effort in the sense that a failure leaves a usable project
		// governed by org owners — but report it rather than lie.
		me := normEmail(s.requestUser(r).Email)
		if me != "" {
			if err := s.Projects.SetCreator(p.ID, me); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// An org owner is already implicitly admin; an explicit grant on
			// one is refused elsewhere, so don't write one here either.
			if s.Dir == nil || org == "" || s.Dir.Role(org, me) != RoleOwner {
				if err := s.Projects.SetPerm(p.ID, me, PermAdmin); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			p, _ = s.Projects.Get(p.ID)
		}
		if tpl.Name != "" {
			// Seeding failure leaves a real, usable project holding part of a
			// template. Say so rather than reporting success; there is no
			// rollback, and deleting a project over a storage hiccup is worse
			// than an honest error.
			if err := s.seedTemplate(r.Context(), p.ID, tpl, s.requestUser(r)); err != nil {
				http.Error(w, fmt.Sprintf("project %s was created, but seeding the %s template failed: %v",
					p.Name, tpl.Name, err), http.StatusBadGateway)
				return
			}
			if err := s.Projects.SetTemplate(p.ID, tpl.Name); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			p, _ = s.Projects.Get(p.ID)
		}
	} else if !atLeast(s.projectPermOf(r, p), PermRead) {
		// GetOrCreate is create-or-join by name: without this, POSTing the
		// name of a project you've been cut off from would hand back its id.
		http.Error(w, permDenied(PermRead), http.StatusForbidden)
		return
	}
	writeJSON(w, map[string]any{"project": projectJSON(p, s.projectPermOf(r, p)), "created": created})
}

// orgForCreate resolves which org a new project lands in: the explicitly
// requested one (must be a membership), else the caller's only org, else —
// for an account in no org yet — a fresh org named after the account, so
// nobody is ever blocked from starting to sync. Orgs disabled → "".
func (s *Server) orgForCreate(r *http.Request, requested string) (string, error) {
	if s.Dir == nil || s.Auth == nil {
		return "", nil
	}
	me := s.requestUser(r)
	if requested != "" {
		if s.Dir.Role(requested, me.Email) == "" {
			return "", fmt.Errorf("you are not a member of organization %q", requested)
		}
		return requested, nil
	}
	mine := s.Dir.OrgsFor(me.Email)
	if len(mine) > 0 {
		return mine[0].ID, nil
	}
	name := me.Name
	if name == "" {
		name = strings.SplitN(me.Email, "@", 2)[0]
	}
	o, err := s.Dir.Create(name, me.Email)
	if err != nil {
		return "", err
	}
	return o.ID, nil
}

// Node is one entry of the file tree returned by the tree endpoint.
type Node struct {
	Name string    `json:"name"`
	Path string    `json:"path"`
	Dir  bool      `json:"dir"`
	Size int64     `json:"size,omitempty"`
	Time time.Time `json:"time,omitzero"`
	// Same three-field "who" shape as HistoryEntry (history.go), so the
	// frontend has one attribution helper for every surface.
	User     string  `json:"user,omitempty"`
	UserName string  `json:"user_name,omitempty"`
	Author   string  `json:"author,omitempty"`
	Device   string  `json:"device,omitempty"`
	Children []*Node `json:"children,omitempty"`
}

func (s *Server) handleTree(v *volume, w http.ResponseWriter, r *http.Request) {
	snap, err := v.snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, buildTree(snap.files))
}

func buildTree(files map[string]FileInfo) *Node {
	root := &Node{Name: "/", Dir: true}
	dirs := map[string]*Node{"": root}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		fi := files[p]
		parent := root
		segs := strings.Split(p, "/")
		for i := 0; i < len(segs)-1; i++ {
			dp := strings.Join(segs[:i+1], "/")
			n, ok := dirs[dp]
			if !ok {
				n = &Node{Name: segs[i], Path: dp, Dir: true}
				dirs[dp] = n
				parent.Children = append(parent.Children, n)
			}
			parent = n
		}
		parent.Children = append(parent.Children, &Node{
			Name: segs[len(segs)-1], Path: p,
			Size: fi.Size, Time: fi.Time,
			User: fi.User, UserName: fi.UserName, Author: fi.Author, Device: fi.Device,
		})
	}
	sortTree(root)
	return root
}

func sortTree(n *Node) {
	sort.SliceStable(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if a.Dir != b.Dir {
			return a.Dir // folders first, like Obsidian
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	for _, c := range n.Children {
		if c.Dir {
			sortTree(c)
		}
	}
}

// lookup resolves ?path= against the volume's current snapshot.
func lookup(v *volume, r *http.Request) (string, FileInfo, int, error) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "", FileInfo{}, http.StatusBadRequest, fmt.Errorf("missing ?path=")
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		log.Printf("beardrive: read project snapshot: %v", err)
		return "", FileInfo{}, http.StatusBadGateway, fmt.Errorf("content temporarily unavailable")
	}
	fi, ok := snap.files[p]
	if !ok {
		// The address is empty — but the file may have moved out of it. A
		// LIVE path always wins, which falls out of the ordering: the
		// snapshot hit above returns first, so nothing redirects while
		// something still answers at the old address.
		to, moved := resolveForward(snap.moves, snap.files, p)
		if !moved {
			return "", FileInfo{}, http.StatusNotFound, fmt.Errorf("no such file: %s", p)
		}
		// The canonical path is what gets returned, so the read is recorded
		// against it (heat doesn't split across old and new) and the render
		// payload names it.
		p, fi = to, snap.files[to]
	}
	return p, fi, 0, nil
}

func (s *Server) serveBlob(v *volume, w http.ResponseWriter, r *http.Request, attach bool) {
	p, fi, code, err := lookup(v, r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	setCanonical(w, r, p)
	// Count the read before the ETag check: a 304 render is still a person
	// reading the file, and skipping it would undercount the hottest pages.
	s.recordRead(r, p)
	etag := `"` + fi.Blob + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	rc, err := v.source.Open(r.Context(), p, fi)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "content temporarily unavailable", err)
		return
	}
	defer rc.Close()
	w.Header().Set("ETag", etag)
	ct := contentType(p)
	w.Header().Set("Content-Type", ct)
	setContentLength(w, rc)
	// nosniff on both branches, the sandbox CSP only on the inline one: an
	// attachment is not rendered, and TestInlineHTMLIsSandboxed pins that
	// /download answers with a disposition INSTEAD of a CSP.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if attach {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(p)))
	} else {
		w.Header().Set("Content-Type", inlineType(ct))
		sandboxInline(w, ct)
	}
	io.Copy(w, rc)
}

// setContentLength promises a body length only when the thing about to be
// streamed can be measured. FileInfo.Size comes off a journal op — JSON a
// client pushed — so echoing it made the hub promise a length it had no way
// to keep: a padded or truncated response for every download of that file,
// declared by anyone who can push a journal. When the source cannot measure
// (an object store's response body), no header goes out and net/http streams
// chunked, which is a slightly worse progress bar and a true one.
func setContentLength(w http.ResponseWriter, rc io.Reader) {
	switch v := rc.(type) {
	case interface{ Stat() (fs.FileInfo, error) }: // *os.File: file:// backend, DirSource
		if fi, err := v.Stat(); err == nil && fi.Mode().IsRegular() {
			w.Header().Set("Content-Length", fmt.Sprint(fi.Size()))
		}
	case interface{ Size() int64 }: // GCS *storage.Reader, bytes.Reader
		w.Header().Set("Content-Length", fmt.Sprint(v.Size()))
	}
}

func (s *Server) handleFile(v *volume, w http.ResponseWriter, r *http.Request) {
	s.serveBlob(v, w, r, false)
}

func (s *Server) handleDownload(v *volume, w http.ResponseWriter, r *http.Request) {
	s.serveBlob(v, w, r, true)
}

func (s *Server) handleRender(v *volume, w http.ResponseWriter, r *http.Request) {
	if sha := r.URL.Query().Get("sha"); sha != "" {
		s.renderVersion(v, w, r, sha)
		return
	}
	p, fi, code, err := lookup(v, r)
	if err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	setCanonical(w, r, p)
	s.recordRead(r, p)
	rc, err := v.source.Open(r.Context(), p, fi)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "content temporarily unavailable", err)
		return
	}
	src, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	pairs, html, err := RenderMarkdownPairs(src)
	if err != nil {
		http.Error(w, fmt.Sprintf("render: %v", err), http.StatusInternalServerError)
		return
	}
	doc := map[string]any{
		"path": p, "html": html,
		"size": fi.Size, "time": fi.Time, "author": fi.Author, "device": fi.Device,
	}
	// Frontmatter travels as data, not as a table baked into html — the
	// viewer lays it out in a side panel. Omitted when the document has
	// none, so the client shows no panel rather than an empty one.
	if len(pairs) > 0 {
		doc["frontmatter"] = pairs
	}
	// Omitted rather than sent empty, so a journal from before accounts
	// existed still renders its Author instead of a blank attribution.
	if fi.User != "" {
		doc["user"] = fi.User
	}
	if fi.UserName != "" {
		doc["user_name"] = fi.UserName
	}
	if f := renderFindings(src); len(f) > 0 {
		doc["findings"] = f
	}
	writeJSON(w, doc)
}

// renderFindings is the share gate's credential scan on the path every file
// takes. The gate could name the rule and the line well enough to refuse to
// publish a file while the viewer rendered the same key as ordinary prose
// (BEA-147), so the render response carries the finding too — advisory only,
// nothing is blocked and nothing is redacted, since a member who can open the
// file could already read the key.
//
// Rule ids and line numbers only. The matched text must never reach a
// response body; see the doc comment on secrets.Scan.
//
// The cap is a slice rather than the LimitReader the two streaming callers
// use: the render path already holds the whole file, because that is what
// RenderMarkdown needs. Same ScanLimit either way, so the badge and the share
// dialog can never disagree about the same file.
func renderFindings(src []byte) []secrets.Finding {
	if len(src) > secrets.ScanLimit {
		src = src[:secrets.ScanLimit]
	}
	return secrets.Scan(src)
}

// renderVersion renders one exact past version by content hash — the
// markdown counterpart of /blob?sha=, so opening an old .md from history
// shows a rendered page instead of raw source. Provenance is not returned:
// the caller already has the history entry it clicked. Viewing history is
// never a read (see the read-heat invariant), so nothing is recorded.
func (s *Server) renderVersion(v *volume, w http.ResponseWriter, r *http.Request, sha string) {
	if !blobRe.MatchString(sha) {
		http.Error(w, "invalid sha", http.StatusBadRequest)
		return
	}
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	rc, err := rs.OpenBlob(r.Context(), sha)
	if err != nil {
		http.Error(w, "no such version", http.StatusNotFound)
		return
	}
	src, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	pairs, html, err := RenderMarkdownPairs(src)
	if err != nil {
		http.Error(w, fmt.Sprintf("render: %v", err), http.StatusInternalServerError)
		return
	}
	doc := map[string]any{
		"path": r.URL.Query().Get("path"), "html": html, "size": len(src),
	}
	if len(pairs) > 0 {
		doc["frontmatter"] = pairs
	}
	// The history view goes through this same endpoint, so scanning here too
	// is what stops the badge vanishing the moment you click into history on
	// the very file it was warning about.
	if f := renderFindings(src); len(f) > 0 {
		doc["findings"] = f
	}
	writeJSON(w, doc)
}

// inlineMarkup reports whether a Content-Type names something the browser
// parses as a DOCUMENT in a top-level navigation, which is what makes it a
// script-execution vehicle on whatever origin served it.
//
// It is deliberately a property and not a list of extensions. The list was the
// bug: it named text/html, image/svg and *xhtml*, and the whole XML family sat
// outside it while having exactly the property — an XML document carries its
// own `<?xml-stylesheet type="text/xsl"?>`, the browser applies the XSLT (the
// stylesheet is same-origin, the attacker uploads it to the same project) and
// renders the result, which is HTML, in the hub's origin with the reader's
// session. Anything that parses as markup belongs here; when in doubt, add it.
func inlineMarkup(ct string) bool {
	ct = strings.ToLower(ct)
	for _, m := range []string{"text/html", "xhtml", "svg", "/xml", "+xml"} {
		if strings.Contains(ct, m) {
			return true
		}
	}
	return false
}

// inlineType is the Content-Type the hub is willing to have a browser PARSE
// when it serves stored bytes inline.
//
// The XML family is declared inert. The sandbox CSP below already removes its
// capability — but it removes it by making the document render as nothing at
// all (the stylesheet an XML document names is sandboxed too, so the XSLT
// never runs and there is no document), and "you see nothing" is a poor answer
// for a reader who clicked a .xml. Declaring it text is both the stronger
// answer — it needs no CSP support in the browser, and nothing parses a
// document — and the more useful one: the reader sees the source.
//
// HTML, XHTML and SVG keep their real type. The app has always served them,
// and for them the sandbox is a complete wall rather than a blank page.
func inlineType(ct string) string {
	l := strings.ToLower(ct)
	if inlineMarkup(l) && !strings.Contains(l, "html") && !strings.Contains(l, "svg") {
		return "text/plain; charset=utf-8"
	}
	return ct
}

// sandboxInline walls off markup the hub serves from its own origin: synced
// HTML (any flavour), scriptable SVG and the XML family run in an opaque
// sandboxed origin — same posture as /s/* share pages — so they can never
// touch the API or the reader's session cookie. Every route that streams
// stored bytes inline calls this: the live file (serveBlob) and any past
// version (history's handleBlob), which serve identical content and must not
// differ in their wall.
// It also stamps nosniff on every response it sees. The wall above keys off
// the Content-Type the hub declared; without nosniff a browser is free to
// sniff attacker-written bytes into a document type the hub never named, which
// is the same capability arriving through a door the CSP never opened.
func sandboxInline(w http.ResponseWriter, ct string) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if inlineMarkup(ct) {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
	}
}

func contentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".txt", ".log", ".go", ".py", ".js", ".ts", ".sh", ".yaml", ".yml", ".toml", ".csv":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// storageErr answers a failed storage operation. The detail goes to the log,
// never to the client: an object-store error names the hub's absolute path
// (or, on S3, its bucket and key), which no project member has any business
// learning from a missing file.
func storageErr(w http.ResponseWriter, code int, msg string, err error) {
	log.Printf("beardrive: %s: %v", msg, err)
	http.Error(w, msg, code)
}

func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

// writeJSONStatus is writeJSON for the answers a client has to read the body
// of — a 409 whose findings the CLI and the browser both decode.
func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
