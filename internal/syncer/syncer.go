// Package syncer drives a volume's sync cycle:
//
//	scan → commit local ops → pull peer journals → preserve conflicts →
//	materialize merged state → push blobs + own journal
//
// Scanning always happens before pulling, so local edits are committed to the
// journal (and their content captured in the blob store) before any remote
// state can overwrite the working folder. Concurrent edits resolve
// deterministically last-writer-wins; the losing local version is preserved
// as a "<name>.bdrive-conflict-<device>-<time>" file that syncs like any other.
//
// The exception is a device's FIRST cycle on a volume, which is a join rather
// than an edit: a local file at a path the project already holds is adopted —
// the project's version wins everywhere and no conflict copy is made — while
// the local content is still journaled (below every clock the project can hold)
// so history keeps it. See step 1b in Cycle.
package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/secrets"
	"github.com/runbear-io/beardrive/internal/store"
)

// pushConcurrency bounds how many blobs upload at once. The initial import of
// many files is latency-bound on serial round-trips, so uploading in parallel
// is the main speedup.
const pushConcurrency = 16

// Progress reports upload progress during a cycle's push phase, so the CLI can
// draw a bar. Total/TotalBytes are set once when the push starts; Done/Bytes
// climb as blobs finish. Nil OnProgress means no reporting (the daemon).
type Progress struct {
	Done, Total    int
	Bytes, ToBytes int64
}

// Session ties a working folder to its volume store and (optionally) remote.
type Session struct {
	Folder  string
	MountID string // the stable project mount id from .bdrive/config.json
	Store   *store.Store
	Device  config.Device
	// Account is the signed-in user (from `bdrive login`); ops carry it so
	// history shows who changed what. Zero on offline/no-auth setups —
	// Device.Author remains the fallback identity.
	Account config.Settings
	// Note, when set, is stamped into every op this session commits — session
	// context like "claude-code session <id>". Empty means fall back to the
	// store's persisted session note (store.LoadNote), which lets a one-shot
	// `bdrive sync --note` leave context that the daemon's later scans also
	// stamp. Conflict-copy ops keep their own explanatory note.
	Note string
	// SessionID is the agent session every op this cycle commits is stamped
	// with (journal.Op.Session). Set only by `bdrive sync --hook`, and
	// deliberately NOT persisted the way Note is (store.SaveNote): the note
	// is context that outlives the hook turn, the session id is an identity
	// that must not be attached to changes the daemon commits on its own
	// later. So a daemon scan after the hook turn carries the note and no
	// session — the asymmetry is intended.
	SessionID string
	// Prune makes this cycle reconcile the hub against the shared ignore
	// rules: every path the remote still holds that .bdriveignore (or a
	// builtin never-sync rule) now excludes is journaled as a delete, so it
	// leaves the hub while staying on disk on every device. Off by default —
	// plain `bdrive sync` and the daemon never set it, because pruning must
	// be a deliberate act, never a side effect of editing .bdriveignore.
	Prune   bool
	Backend remote.Backend // nil = work offline
	// OnProgress, when set, is called during push with upload progress. It may
	// be invoked concurrently from upload workers, so it must be safe to call
	// from multiple goroutines.
	OnProgress func(Progress)

	// inbound accumulates this cycle's materialized peer paths, for
	// Result.Inbound. Reset at the top of every cycle — a Session is reused
	// across cycles by the daemon and by the tests.
	inbound []store.InboundEvent
}

// logInbound records one materialized peer path both ways: on this cycle's
// Result (for the post_sync hook, which fires from the cycle that did the
// work) and on the store's spool (for `bdrive sync --hook`, which runs in a
// later process). Both consumers want the same event; neither may consume the
// other's copy.
func (s *Session) logInbound(rel string, deleted bool) {
	s.inbound = append(s.inbound, store.InboundEvent{Path: rel, Deleted: deleted, Time: time.Now().UTC()})
	_ = s.Store.LogInbound(rel, deleted) // best-effort: never fails a cycle
}

func (s *Session) mountID() string {
	if s.MountID != "" {
		return s.MountID
	}
	// Fallback for sessions built without a project (tests): key the state
	// cache by the folder path.
	sum := sha256.Sum256([]byte(s.Folder))
	return hex.EncodeToString(sum[:])[:12]
}

// Result summarizes one sync cycle.
//
// Offline, ReadOnly, and NoAccess are three different answers and must not be
// conflated: offline means the hub could not be reached and everything should
// be retried; ReadOnly means it refused our push (we keep pulling, local ops
// stay journaled and unpushed); NoAccess means it refused our pull too, so the
// cycle does nothing at all and leaves the working folder alone. Regaining
// access self-heals on a later cycle with no manual step.
type Result struct {
	LocalOps  int // local changes committed to the journal
	PulledOps int // ops received from other devices
	Conflicts int // conflict copies created
	// Adopted counts paths where this folder's own content gave way to the
	// project's on join (step 1b). Not a conflict and not an error — the
	// superseded content stays in history — but the user asked for none of it,
	// so it is worth a line.
	Adopted      int
	Pruned       int  // paths removed from the hub by --prune (kept on disk)
	Materialized int  // files written/removed in the working folder
	Pushed       bool // own journal/blobs uploaded
	// Inbound names the paths this cycle materialized on a peer's behalf —
	// the same events the inbound spool queues, carried out of the cycle that
	// made them so the post_sync hook can hand them to a local command. It
	// does NOT replace the spool: see the comment in internal/store/inbound.go
	// for why the two coexist.
	Inbound []store.InboundEvent
	// Offline reports that the remote leg of this cycle had a problem worth
	// telling the user about — usually "unreachable", but also "the hub served
	// bytes that are not their content address", which is the only signal that
	// case ever produces. It is a REPORT, not a gate: a content-level problem
	// with one object still lets this device push its own work (Pushed may be
	// true alongside it), because otherwise one peer's journal line decides
	// whether anyone else's edits ever leave their machine.
	Offline    bool
	OfflineErr error
	ReadOnly   bool // the hub refused our push: pull-only from here
	NoAccess   bool // the hub refused our pull: sync paused, nothing touched
	AccessErr  error
}

// accessReason renders a hub refusal as the sentence a person can act on. The
// hub answers a device-registration 403 with what to DO about it, and every
// caller used to collapse that into "read-only (pull only)" — the one refusal
// that is not about project permissions at all, reported as if it were, so the
// user re-checked their access, saw `write`, and had nowhere left to look.
//
// The wrapper chain the CLI itself added ("forbidden: server: 403 Forbidden: ")
// is dropped; it tells a reader nothing the line does not already say.
//
// The remainder is the HUB's text landing in a log file, a terminal and an
// agent's context, so it passes journal.SafeText — the rule this repo already
// applies to every other peer-written string it renders — or it does not travel
// at all. Bounded for the same reason: this is persisted in sync.json and
// printed on one status line.
func accessReason(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if _, rest, ok := strings.Cut(msg, "Forbidden: "); ok && rest != "" {
		msg = rest
	}
	if len([]rune(msg)) > 300 || !journal.SafeText(msg) {
		return ""
	}
	return msg
}

// Reason is the hub's own words for a ReadOnly or NoAccess answer, empty when
// the hub gave none. It is what the CLI prints under the summary line.
func (r *Result) Reason() string { return accessReason(r.AccessErr) }

func (r *Result) Activity() bool {
	return r.LocalOps > 0 || r.PulledOps > 0 || r.Conflicts > 0 || r.Adopted > 0 || r.Pruned > 0 || r.Materialized > 0
}

// The builtin exclusions (.bdrive — the mount's local identity, syncing it
// would let one device silently repoint another — and .git) are defined once
// in config, because the hub enforces the same set on the paths clients
// upload. Local aliases so the walk and the path checks below read plainly.
func ignoredDir(name string) bool { return config.ReservedDir(name) }

// maxLamport caps what this device's clock will absorb from a peer. Cycle
// raises st.Lamport to any value it pulls and scan increments it per local op,
// so one op carrying math.MaxInt64 wraps the clock negative and every op this
// device ever writes again sorts before everything it has already seen — a
// silent, permanent write lock installed by one line of JSON. A value this
// large is not a clock reading, so it is ignored rather than absorbed. Pulled
// ops are never rewritten: replay must agree between a device and its remote
// copy.
const maxLamport = int64(1) << 62

// absorbLamport advances the local clock to a peer's reading, ignoring absurd
// ones. tickLamport is the local increment, which stops at the ceiling rather
// than wrapping.
func absorbLamport(cur, peer int64) int64 {
	if peer > cur && peer <= maxLamport {
		return peer
	}
	return cur
}

// tickLamport is the local increment. The cap is on what this device ABSORBS,
// not on what it writes: a device that legitimately absorbed the ceiling must
// still be able to write an op that sorts after it, or the clock is frozen
// there forever and every later local edit falls through to Time — which the
// peer that sent the ceiling also chose. That is the same silent write lock
// the cap exists to prevent, reachable with the one value the cap accepts.
// Ticking past the ceiling is safe: it only ever climbs by one per local op,
// so only the wrap itself is refused.
func tickLamport(cur int64) int64 {
	if cur == math.MaxInt64 {
		return cur
	}
	return cur + 1
}

// Cycle runs one full scan/sync/materialize pass under the volume lock, then
// fires the folder's post_sync hook — after the lock is released, so a hook
// that itself runs a bdrive command starts working immediately instead of
// blocking on the flock the cycle that spawned it is still holding (a long
// push can hold it for a while). Splitting the body out is what makes that
// ordering a property of the code: no call site has to remember it.
func (s *Session) Cycle(ctx context.Context) (*Result, error) {
	res, err := s.cycleLocked(ctx)
	if err == nil && res != nil {
		s.firePostSync(res)
	}
	return res, err
}

func (s *Session) cycleLocked(ctx context.Context) (*Result, error) {
	unlock, err := s.Store.Lock()
	if err != nil {
		return nil, fmt.Errorf("lock volume: %w", err)
	}
	defer unlock()

	s.inbound = nil
	res := &Result{}
	cache, err := s.Store.LoadCache(s.mountID())
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	st, err := s.Store.LoadSync()
	if err != nil {
		return nil, fmt.Errorf("load sync state: %w", err)
	}
	// Advisory telemetry about files this cycle will journal and push anyway,
	// so an unreadable record starts the cycle empty rather than failing it.
	sec := &secretLog{found: map[string][]secrets.Finding{}}
	if found, err := s.Store.LoadSecrets(s.mountID()); err == nil {
		sec.found = found
	}
	myOps, err := s.Store.DeviceOps(s.Device.ID)
	if err != nil {
		return nil, fmt.Errorf("read own journal: %w", err)
	}
	proj, _, err := config.LoadProject(s.Folder)
	if err != nil {
		return nil, err
	}
	// This device has never contributed to this volume: it is JOINING a project,
	// not editing one. Step 1b is what that changes.
	joining := st.Lamport == 0 && st.PushedOps == 0
	filter, err := loadFilter(s.Folder, proj.Include)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", IgnoreFile, err)
	}
	// Accept the rules on disk as this device's own whenever they are not the
	// copy a peer's version last wrote here — i.e. whenever somebody at this
	// machine authored them (`bdrive init --only`, `bdrive scope`, an editor).
	// Runs before the scan and before the pull, so the floor the scan applies
	// is always the last rules this device agreed to. See Filter.SkipUp.
	if cur, err := os.ReadFile(filepath.Join(s.Folder, IgnoreFile)); err == nil || os.IsNotExist(err) {
		text := string(cur)
		// Both fields are omitempty, so a SyncState written before they existed
		// carries neither — and the accept test below then reads whatever is on
		// disk as locally authored, including a peer's widening that landed one
		// cycle earlier (scan runs before pull, so it always does). A device that
		// has already synced is an upgrade, not a new joiner: seed the pair from
		// what this mount is demonstrably syncing already (vouchedFloor) instead
		// of taking the file's word for it.
		if st.IgnoreAccepted == "" && st.IgnorePulled == "" && text != "" && !joining {
			synced := make([]string, 0, len(cache))
			for rel := range cache {
				synced = append(synced, rel)
			}
			st.IgnoreAccepted, st.IgnorePulled = vouchedFloor(text, synced), text
		} else if text != st.IgnorePulled {
			st.IgnoreAccepted = text
		}
	}
	filter.AcceptRules(st.IgnoreAccepted)

	// 1. Scan the working folder and journal any local changes.
	localOps, err := s.scan(cache, &st, int64(len(myOps)), filter, sec)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	commitLocal := func() error {
		if len(localOps) == 0 {
			return nil
		}
		if err := s.Store.AppendOps(s.Device.ID, localOps); err != nil {
			return fmt.Errorf("append journal: %w", err)
		}
		myOps = append(myOps, localOps...)
		res.LocalOps = len(localOps)
		localOps = nil
		return nil
	}
	if !joining {
		// The normal path: journal local edits before the pull, so nothing
		// remote can overwrite an edit that was never captured (see the package
		// doc). A joining device holds its ops back for the length of the pull
		// only — long enough to learn which paths the project already has.
		if err := commitLocal(); err != nil {
			return nil, err
		}
	}

	// 2. Pull journals + blobs from other devices.
	//
	// blocked is "the remote leg cannot continue this cycle" and is separate
	// from res.Offline, which is what the user is TOLD. They used to be one
	// flag, so any pull error withheld this device's own push — and one of
	// those errors is "a blob's bytes are not its content address", which one
	// peer produces at will by understating Op.Size on one journal line.
	blocked := false
	var pulled, gone []journal.Op
	if s.Backend != nil {
		pulled, gone, err = s.pull(ctx, cache)
		switch {
		case err == nil:
			if st.Access == store.AccessNone {
				// The pull that was refused now succeeds, so read access is back.
				// Whether writes are back is the push leg's to answer below.
				st.Access, st.AccessReason = store.AccessOK, ""
			}
		case errors.Is(err, remote.ErrForbidden):
			// Access to this project was revoked. Stop here: materializing a
			// replay we can no longer refresh would look like the hub
			// reverting the user's files. Nothing is pushed, nothing is
			// deleted, and the next cycle re-checks.
			res.NoAccess, res.AccessErr = true, err
			st.Access, st.AccessReason = store.AccessNone, accessReason(err)
			// The scan already claimed these files in the state cache, which
			// finish is about to persist — journal them or the next scan sees
			// nothing changed and this folder's content is never captured.
			if cerr := commitLocal(); cerr != nil {
				return nil, cerr
			}
			return res, s.finish(cache, st, sec)
		case errors.Is(err, errBlobContent):
			// Reported — it is the only signal a device ever gets that its hub
			// is serving bytes that are not what they are addressed as — but
			// NOT blocking: one object's content says nothing about whether the
			// hub is reachable, and treating it as such withheld this device's
			// own journal and blobs for one integer in one peer's journal line.
			// The path itself stays unwritten until real content shows up.
			res.Offline = true
			res.OfflineErr = err
		default:
			res.Offline = true
			res.OfflineErr = err
			blocked = true
		}
		res.PulledOps = len(pulled)
		for _, op := range pulled {
			st.Lamport = absorbLamport(st.Lamport, op.Lamport)
		}
	}

	// 1b. Adoption. A device joining a project it has never synced is not
	// editing that project's files: it is bringing a folder that happens to
	// hold some of the same paths — a git checkout of the same docs, an
	// agent-written AGENTS.md, the .bdriveignore `bdrive init` seeds. Treating
	// those as concurrent edits forked every one of them: whichever side's
	// clock sorted higher won, and the other landed beside it as a
	// `.bdrive-conflict-<device>-<time>` file. So connecting a folder littered
	// it with copies of files nobody had edited, and half the time the joiner's
	// stale copy is what won — replacing the team's version for everyone.
	//
	// The project's version wins instead, deterministically: the local op is
	// demoted under every op the project can hold (scan's clock starts at 1, so
	// lamport 0 loses to all of them on every device). It is still journaled and
	// pushed, so nothing is lost — the folder's content at join time is in
	// History and `bdrive restore` brings it back — it just never materializes.
	if joining && len(localOps) > 0 && len(pulled) > 0 {
		theirs := map[string]journal.Op{}
		for _, op := range pulled {
			if prev, ok := theirs[op.Path]; !ok || journal.Less(prev, op) {
				theirs[op.Path] = op
			}
		}
		for i, op := range localOps {
			// Only a path the project actually HOLDS is adopted. Where its
			// last op is a delete there is no version to adopt, so the local
			// file is this device's own and keeps its clock.
			if t, ok := theirs[op.Path]; ok && t.Kind == journal.KindPut {
				localOps[i].Lamport = 0
				localOps[i].Note = adoptNote
				res.Adopted++
			}
		}
	}
	if err := commitLocal(); err != nil {
		return nil, err
	}

	// 3. Preserve losing local edits as conflict copies.
	if len(pulled) > 0 {
		conflictOps, err := s.conflictCopies(myOps, st.PushedOps, pulled, &st)
		if err != nil {
			return nil, err
		}
		if len(conflictOps) > 0 {
			if err := s.Store.AppendOps(s.Device.ID, conflictOps); err != nil {
				return nil, fmt.Errorf("append conflict ops: %w", err)
			}
			myOps = append(myOps, conflictOps...)
			res.Conflicts = len(conflictOps)
		}
	}

	// 3b. Re-assert what a peer withdrew. An op this device already applied is
	// a file on this disk, and a file only leaves a disk because somebody
	// deleted it — so when a peer republishes its journal without an op we
	// already put on disk, we restate that op in OUR journal, keeping its
	// original clock so a genuinely later change still wins replay. The
	// alternative (refusing the peer's update) leaves this device holding a
	// state a first-time device can never reach, which is the divergence the
	// byte-offset resume exists to prevent.
	//
	// It runs AFTER conflictCopies, not before it. A re-asserted op carries the
	// withdrawn op's original — and therefore usually losing — lamport, so as a
	// member of the unpushed set it was a losing local edit by construction, and
	// step 3 did exactly what it exists to do: preserved it as a conflict copy.
	// Net effect, with no local edit on this side at all: a peer made this
	// device create, sign and push a file at a path that never existed in the
	// project, holding content the peer chose.
	if len(gone) > 0 {
		readopt := make([]journal.Op, 0, len(gone))
		for _, op := range gone {
			if !s.stillHold(op, cache) {
				continue // we cannot stand behind what this folder does not hold
			}
			if filter.Skip(op.Path) || neverSync(op.Path) || unsafeRel(op.Path) {
				// The filter is applied symmetrically in scan and materialize
				// precisely so a path this device refuses to write is also one
				// it never publishes. This is a third publishing site and owes
				// the same rule.
				continue
			}
			op.Device, op.DeviceName, op.Author = s.Device.ID, s.Device.Name, s.Device.Author
			op.Seq = int64(len(myOps)) + int64(len(readopt)) + 1
			op.Note = reassertNote
			readopt = append(readopt, op)
		}
		if len(readopt) > 0 {
			if err := s.Store.AppendOps(s.Device.ID, readopt); err != nil {
				return nil, fmt.Errorf("append re-asserted ops: %w", err)
			}
			myOps = append(myOps, readopt...)
			res.LocalOps += len(readopt)
		}
	}

	// 4. Materialize the merged state into the working folder.
	all, err := s.Store.AllOps()
	if err != nil {
		return nil, fmt.Errorf("read journals: %w", err)
	}
	target := journal.Replay(all)

	// The ignore rules sync like any other file, so a peer can receive the new
	// .bdriveignore and the delete ops it justifies in the same batch. The
	// filter was loaded at the top of the cycle, before the pull, so write the
	// rules first and reload from them — otherwise materialize's delete loop
	// runs against stale rules, its filter guard never fires, and it unlinks
	// files that merely left sync scope.
	if want, ok := target[IgnoreFile]; !ok {
		// A peer DELETED the shared rules — the maximal widening there is, and
		// the one shape the bookkeeping below cannot see, since it is written in
		// terms of a file that still exists. materialize's delete loop unlinks
		// the local copy either way; recording the deletion as PULLED is what
		// stops the next cycle reading the now-absent file as locally authored
		// and dropping the accepted floor with it.
		if len(pulled) > 0 {
			st.IgnorePulled = ""
		}
	} else if len(pulled) > 0 {
		wrote, err := s.materializeFile(IgnoreFile, want, cache)
		if err != nil {
			log.Printf("beardrive: could not write %s this cycle: %v", IgnoreFile, err)
		}
		if wrote {
			res.Materialized++
			// The reload rebuilds the rules from the new file — and only the
			// rules. Filter.nested is what walkFolder discovered during the
			// scan at the top of this cycle: it marks subfolders that sync
			// through their OWN project, with their own member list, so it is
			// a project boundary rather than an ignore rule. A fresh filter's
			// empty nested list would let this project's ops write into that
			// one, where its daemon picks them up and pushes them on.
			nested := filter.nested
			if filter, err = loadFilter(s.Folder, proj.Include); err != nil {
				return nil, fmt.Errorf("load %s: %w", IgnoreFile, err)
			}
			filter.nested = nested
			// Remember the pulled text as pulled, NOT as accepted: these rules
			// came from a peer, so they may narrow what this device uploads but
			// never widen it until somebody here authors a change. The accepted
			// floor carries over unchanged.
			if cur, err := os.ReadFile(filepath.Join(s.Folder, IgnoreFile)); err == nil {
				st.IgnorePulled = string(cur)
			}
			filter.AcceptRules(st.IgnoreAccepted)
		}
		// If the blob isn't fetched yet materializeFile skips it and the old
		// rules stand: the usual retry-next-cycle posture, and the guard in
		// materialize still protects the files either way.
	}

	// 4b. Prune: remove from the hub what the shared rules now exclude.
	if s.Prune {
		pruneOps, err := s.pruneOps(target, &st, int64(len(myOps)))
		if err != nil {
			return nil, err
		}
		if len(pruneOps) > 0 {
			if err := s.Store.AppendOps(s.Device.ID, pruneOps); err != nil {
				return nil, fmt.Errorf("append prune ops: %w", err)
			}
			myOps = append(myOps, pruneOps...)
			res.Pruned = len(pruneOps)
		}
	}

	n, err := s.materialize(target, cache, filter)
	if err != nil {
		return nil, fmt.Errorf("materialize: %w", err)
	}
	res.Materialized += n
	res.Inbound = s.inbound

	// 5. Push our blobs and journal.
	if s.Backend != nil && !blocked && int64(len(myOps)) > st.PushedOps {
		switch err := s.push(ctx, myOps, &st); {
		case err == nil:
			res.Pushed = true
			st.Access, st.AccessReason = store.AccessOK, ""
		case errors.Is(err, remote.ErrForbidden):
			// Read-only on this project: pull and materialize already ran, so
			// pull-only is the steady state. Our own ops stay in the local
			// journal — never pushed, never dropped. The push is still
			// attempted once per remote interval (no hot loop, and a re-grant
			// self-heals).
			res.ReadOnly, res.AccessErr = true, err
			st.Access, st.AccessReason = store.AccessReadOnly, accessReason(err)
		default:
			res.Offline = true
			res.OfflineErr = err
			blocked = true
		}
	}

	// 6. Drain the agent read spool to the hub (read heatmap telemetry).
	// Strictly best-effort: a failed report keeps the batch queued for the
	// next cycle and never fails — or even marks offline — this one.
	if rr, ok := s.Backend.(remote.ReadReporter); ok && !blocked {
		if evs, err := s.Store.PendingReads(); err == nil && len(evs) > 0 {
			reads := make([]remote.ReadEvent, len(evs))
			for i, e := range evs {
				reads[i] = remote.ReadEvent{Path: e.Path, Session: e.Session, Time: e.Time}
			}
			if rr.ReportReads(ctx, reads) == nil {
				s.Store.ClearPendingReads()
			}
		}
	}

	// st.Access is NOT recomputed here. Only the leg that actually asked the hub
	// knows how it answered, so each one records its own verdict above and a
	// cycle that never asked leaves the last one standing. Resetting it to OK on
	// every cycle meant the daemon's cheap local-only ticks — three of them
	// between remote passes — each declared "access restored; syncing normally",
	// so a device the hub was refusing alternated between the two log lines
	// forever and `bdrive status` reported healthy sync moments after the push
	// it had just refused.
	if err := s.finish(cache, st, sec); err != nil {
		return nil, err
	}
	return res, nil
}

// finish persists the two pieces of state a cycle mutates. Saving the cache
// matters even on a cut-short cycle: the scan already journaled local edits,
// and dropping the cache would make the next scan journal them all again.
func (s *Session) finish(cache map[string]store.CachedFile, st store.SyncState, sec *secretLog) error {
	if err := s.Store.SaveCache(s.mountID(), cache); err != nil {
		return err
	}
	// Written only when the cycle changed it, so a quiet daemon tick every 3s
	// still writes nothing — and an error is logged, never returned: this is
	// advisory telemetry about ops that are already journaled and pushed, and
	// giving it a veto over convergence is the one invariant this feature is
	// close enough to break.
	if sec != nil && sec.dirty {
		if err := s.Store.SaveSecrets(s.mountID(), sec.found); err != nil {
			log.Printf("beardrive: could not record credential findings: %v", err)
		}
	}
	return s.Store.SaveSync(st)
}

// scan diffs the working folder against the state cache and returns ops for
// every local change, storing new content in the blob store. Filtered paths
// are neither journaled nor deleted: a path that becomes ignored is dropped
// from the cache without a delete op, so opting out locally never removes
// the file from other devices.
func (s *Session) scan(cache map[string]store.CachedFile, st *store.SyncState, seqBase int64, filter *Filter, sec *secretLog) ([]journal.Op, error) {
	seen := make(map[string]bool, len(cache))
	var ops []journal.Op
	note := s.Note
	if note == "" {
		note = s.Store.LoadNote()
	}
	// One scan is one commit: every op it produces carries the same Time. Op
	// order inside the batch is already carried by Lamport and Seq, which
	// journal.Less consults first and second — so replay is unaffected — while
	// a shared Time is what lets `bdrive log` recognise a batch and order it by
	// the files' own write times (SortForDisplay). Stamping each op separately
	// made a rename's two halves two different instants, which is the bug.
	committed := time.Now().UTC()
	nextOp := func(kind, rel string) journal.Op {
		st.Lamport = tickLamport(st.Lamport)
		seqBase++
		return journal.Op{
			Seq: seqBase, Lamport: st.Lamport, Time: committed,
			Device: s.Device.ID, DeviceName: s.Device.Name, Author: s.Device.Author,
			User: s.Account.Email, UserName: s.Account.Name,
			Kind: kind, Path: rel, Note: note, Session: s.SessionID,
		}
	}

	err := walkFolder(s.Folder, filter, func(p, rel string, d fs.DirEntry, v verdict) error {
		if v != vSync {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		seen[rel] = true
		size, mt := info.Size(), info.ModTime().UnixNano()
		mode := uint32(info.Mode().Perm())
		c, ok := cache[rel]
		if ok && c.Size == size && c.MTimeNS == mt {
			return nil // unchanged (cheap path)
		}
		sum, n, err := s.Store.PutBlobFile(p)
		if err != nil {
			return nil // file vanished or unreadable; next cycle
		}
		// Below the cheap size+mtime return above, so only a file that actually
		// changed is ever read for credentials — and covering both branches
		// below means a file touched back to previously-flagged content still
		// reports. Warn only: nothing here stops the op.
		sec.scanBlob(s.Store, rel, sum)
		if ok && c.Blob == sum {
			// content unchanged, just touched
			c.Size, c.MTimeNS, c.Mode = n, mt, mode
			cache[rel] = c
			return nil
		}
		op := nextOp(journal.KindPut, rel)
		op.Blob, op.Size, op.Mode = sum, n, mode
		op.Mtime = info.ModTime().UTC()
		ops = append(ops, op)
		cache[rel] = store.CachedFile{Blob: sum, Size: n, Mode: mode, MTimeNS: mt}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for rel := range cache {
		if seen[rel] {
			continue
		}
		// The walk only ever produces clean in-mount paths, but this pass
		// mints an op from every cache key the walk did NOT see — and the
		// cache is a JSON file in $BDRIVE_HOME, not something the walk wrote.
		// Same rule the peer-op side applies, so this device never signs and
		// pushes a path it would refuse from anyone else.
		// Every arm below stops tracking the path, so every arm forgets its
		// findings too: a warning about a file this mount no longer syncs is
		// one nothing can clear.
		sec.drop(rel)
		if unsafeRel(rel) || neverSync(rel) {
			delete(cache, rel)
			continue
		}
		if filter.Skip(rel) {
			delete(cache, rel) // newly filtered, not deleted: stop tracking silently
			continue
		}
		ops = append(ops, nextOp(journal.KindDelete, rel))
		delete(cache, rel)
	}
	return ops, nil
}

// redefinesApplied reports whether have re-uses the (device, seq) of an op in
// applied for a DIFFERENT op. That slot is an op's identity within a journal —
// the one thing a peer cannot restate without contradicting itself — and
// contradicting it is how an already-applied op gets withdrawn while the op
// count, which is all the guard in pull can otherwise check, stays put.
//
// Sameness is exactly what journal.Less compares (the fields Replay reads), so
// two ops this calls equal fold to the same state; everything else on an Op is
// display only.
//
// A slot that is simply GONE is not a redefinition: a torn or corrupt line is
// the honest version of that, and pull's op-count guard is what covers it.
func redefinesApplied(have, applied []journal.Op) bool {
	if len(applied) == 0 {
		return false
	}
	ids := make(map[string]bool, len(have))
	slots := make(map[string]bool, len(have))
	for _, op := range have {
		ids[opID(op)] = true
		slots[opSlot(op)] = true
	}
	for _, op := range applied {
		if slots[opSlot(op)] && !ids[opID(op)] {
			return true
		}
	}
	return false
}

func opSlot(op journal.Op) string { return fmt.Sprintf("%s\x00%d", op.Device, op.Seq) }

func opID(op journal.Op) string {
	return fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d",
		opSlot(op), op.Lamport, op.Time.UTC().Format(time.RFC3339Nano),
		op.Kind, op.Path, op.Blob, op.Size, op.Mode)
}

// sizeGrowth is the headroom a bound gets over the size an object was
// declared at. Both bodies a device reads off the wire are bounded by what it
// was TOLD they were: the party serving the bytes must not also choose how
// many of them the daemon buffers (a journal body went straight into
// io.ReadAll, and a blob into an unbounded io.Copy in the volume's temp dir,
// with the hash check that would notice running after the copy — on a 3-second
// retry loop, forever). The slack is what makes the bound safe for the honest
// case: a journal legitimately grows between the LIST and the GET, and the
// next cycle picks up whatever did not fit.
const sizeGrowth = 1 << 20

func sizeBound(size int64) int64 {
	if size <= 0 || size > math.MaxInt64-sizeGrowth {
		return sizeGrowth
	}
	return size + sizeGrowth
}

// maxPullBytes is the absolute ceiling under sizeBound. sizeBound alone met its
// stated property only while the party serving the bytes was a PEER, whose
// numbers a separate hub had at least stored; when the peer IS the hub there is
// no second party at all, and one listing entry or one journal line sized the
// device's allocation and its disk. A journal is JSONL text (100 MiB is well
// over a million ops) and a blob this size is already far past what a synced
// project of notes and documents holds.
//
// It is a read CEILING, never an up-front refusal on the declared size: op.Size
// is a peer's integer with no relation to the object it names, so refusing on it
// let one peer line stop honest content from ever landing — the round-4 wedge
// class. An over-declared honest blob still reads to its real length and
// verifies; only bytes past the ceiling are refused.
//
// ponytail: an absolute constant, mirroring `bdrive import`'s maxImportBlob
// (256 << 20, raisable with --max-blob). A file larger than this does not
// materialize on receiving devices — if that becomes a real workload, this
// wants the same kind of knob, not a bigger constant. Raised from 32 MiB with
// delta sync: large files were the reason chunking shipped, and a ceiling
// below the files it was built for made it dead weight.
const maxPullBytes = 100 << 20

func pullBound(size int64) int64 { return min(sizeBound(size), maxPullBytes) }

// maxPeerJournals caps how many peer journal files one project's listing may
// mint on this disk. See pull.
const maxPeerJournals = 512

// reassertNote marks an op this device restated on a peer's behalf (step 3b).
// It is the only thing that distinguishes such an op from a local edit once it
// is in this device's journal, and conflictCopies depends on that distinction.
const reassertNote = "re-asserted: the device that published it withdrew it"

// adoptNote marks a local op demoted by the adoption step (1b): content this
// folder already held at a path the project it just joined also holds. Like
// reassertNote it is what tells conflictCopies the op is not a local edit — an
// adopted op is a loser by construction, so preserving it as a conflict copy is
// exactly the litter adoption exists to remove.
const adoptNote = "kept as history: the project's version of this path was adopted on join"

// errBlobContent marks "a blob's bytes are not its content address" — a
// statement about ONE object, never about whether the hub is reachable.
// Conflating the two let one peer integer (an understated Op.Size truncates an
// honest read and fails the sha) put the cycle in Offline, which is the flag
// step 5 checks before pushing anything: the victim's own journal and blobs
// stayed on the victim's disk, for one journal PUT per cycle.
var errBlobContent = errors.New("blob content mismatch")

// safeDevice reports whether a device id off a remote listing may become a
// path in this volume's journal dir. journal.SafePath is the repo's rule for a
// path a stranger named; a device id is one segment of one, so it also may not
// contain a separator and is bounded by NAME_MAX.
func safeDevice(dev string) bool {
	return dev != "" && len(dev) <= 200 && !strings.ContainsAny(dev, `/\`) && journal.SafePath(dev)
}

// sameJournalFile reports whether two journal paths name one file. os.SameFile
// is the only answer that survives a filesystem's own normalization (case on
// APFS/NTFS, unicode NFD on APFS) — string comparison is the check that let a
// hub address this device's own journal under another spelling.
func sameJournalFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// stillHold reports whether this folder's own materialization currently stands
// behind op — the premise step 2b rests on ("an op this device already applied
// is a file on this disk"). The mount's state cache is the record of what THIS
// device wrote into THIS folder, so it is the only evidence that the op was
// applied here; the replayed state is not, since the withdrawal is exactly what
// changed it.
//
// A withdrawn DELETE is never re-asserted. The cache records what is here, never
// what left, so "this device applied that delete" and "this path never existed
// in the project" are the same observation — and re-asserting on it let a peer
// mint deletes of paths that never existed (round 7's inert-op primitive),
// withdraw them, and grow every teammate's append-only journal by that many ops
// for one journal PUT each. Withdrawing a delete instead resurrects the file,
// which only the op's own author can trigger and which they can reach anyway by
// re-putting the content.
func (s *Session) stillHold(op journal.Op, cache map[string]store.CachedFile) bool {
	if op.Kind != journal.KindPut || op.Blob == "" {
		return false
	}
	c, ok := cache[op.Path]
	return ok && c.Blob == op.Blob && s.Store.HasBlob(op.Blob)
}

// withdrawn reports the ops in applied whose (device, seq) slot is no longer
// present in have at all — an op this device already put on disk that the
// peer's republished journal simply does not carry any more.
//
// It is not a redefinition (redefinesApplied) and not a shrink (the op count
// can grow at the same time), so neither guard sees it; a peer replaces one
// applied line with bytes Parse drops, appends two, and a file leaves every
// teammate's folder with no delete op and nothing in History. Refusing the
// update instead is not available: a device that synced before the rewrite
// would then hold a state a device syncing for the first time can never reach
// (TestSec_Pull_APeerCannotChooseWhichOpsEachDeviceSees), and the peer would
// be choosing the split again. So the receiver RE-ASSERTS what it applied, as
// its own op — the one journal it is allowed to write — which both keeps the
// file and republishes the fact to every other device.
func withdrawn(have, applied []journal.Op) []journal.Op {
	if len(applied) == 0 {
		return nil
	}
	slots := make(map[string]bool, len(have))
	for _, op := range have {
		slots[opSlot(op)] = true
	}
	var out []journal.Op
	for _, op := range applied {
		if !slots[opSlot(op)] {
			out = append(out, op)
		}
	}
	return out
}

// pull fetches journals that grew on the remote and any blobs we are missing
// for the new ops. It returns the ops we had not seen before, and any op a
// peer withdrew from a journal we had already applied (see withdrawn).
// cache is the mount's materialization state, read only for the delta basis:
// cache[path].Blob is the version of a file this device currently holds, and
// its chunks source a chunked pull locally (fetchChunked).
func (s *Session) pull(ctx context.Context, cache map[string]store.CachedFile) ([]journal.Op, []journal.Op, error) {
	objs, err := s.Backend.List(ctx, "journal/")
	if err != nil {
		return nil, nil, err
	}
	// The listing is the remote's answer, and the remote may be a hostile hub:
	// every key in it becomes a path in this volume's journal dir, and every
	// journal file there is re-read by AllOps on every later cycle. Round 7
	// capped the listing BODY; nothing capped the object COUNT, so one in-bounds
	// listing minted ~200k local files. New journals are admitted up to a
	// ceiling; existing ones always keep updating, so a real project's peers are
	// never starved by a flood.
	//
	// ponytail: 512 peer journals per project. A project with more devices than
	// that needs a paginated/authenticated peer list, not a bigger number.
	newJournals := maxPeerJournals
	if ents, err := os.ReadDir(filepath.Join(s.Store.Dir(), "journal")); err == nil {
		newJournals -= len(ents)
	}
	own := s.Store.JournalPath(s.Device.ID)
	var newOps, gone []journal.Op
	for _, o := range objs {
		name := strings.TrimPrefix(o.Key, "journal/")
		if !strings.HasSuffix(name, ".jsonl") || strings.Contains(name, "/") {
			continue
		}
		dev := strings.TrimSuffix(name, ".jsonl")
		if !safeDevice(dev) {
			// The hub picks these names and store.JournalPath validates
			// nothing. A name the OS cannot open (a NUL, a control byte, 300
			// bytes) is not a peer; it used to be a path this loop then failed
			// on, hiding every peer listed behind it.
			continue
		}
		lp := s.Store.JournalPath(dev)
		// Never this device's own journal — the one object it is the sole
		// writer of. An exact string compare is not the test: on a
		// case-insensitive filesystem (APFS and NTFS by default) "DEVA.jsonl"
		// is a different key and the SAME FILE, so the hub got to replace this
		// device's log and push it back as its own. The skip is on the FILE,
		// which is what the invariant is about.
		if strings.EqualFold(dev, s.Device.ID) || sameJournalFile(lp, own) {
			continue
		}
		var localSize int64
		isNew := true
		if fi, err := os.Stat(lp); err == nil {
			localSize, isNew = fi.Size(), false
		}
		if o.Size <= localSize && localSize > 0 {
			continue
		}
		if isNew {
			if newJournals <= 0 {
				continue
			}
			newJournals--
		}
		rc, err := s.Backend.Get(ctx, o.Key)
		if err != nil {
			return newOps, gone, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, pullBound(o.Size)))
		rc.Close()
		if err != nil {
			return newOps, gone, err
		}
		// Resume from a BYTE offset, never from an op count. A peer owns its
		// journal object and can rewrite it, and Parse drops a line it cannot
		// decode — so "skip the first len(prev) ops" let the peer choose how
		// far each device's cursor jumped: replace one already-counted line
		// with junk and every appended op shifts down by one, so a device that
		// synced earlier silently never sees it while a first-time device
		// does. Two devices, one journal, permanently different states.
		//
		// What we hold locally is the exact bytes we last accepted. If the
		// object still extends them, the new ops are what the extension parses
		// to; if it does not, the peer rewrote its log and every op in it is
		// treated as new. Re-applying ops is idempotent (Replay is a fold), so
		// the rewritten case is only ever slow, never divergent.
		local, err := os.ReadFile(lp)
		if err != nil && !os.IsNotExist(err) {
			// One unreadable local copy is one peer's problem, not every
			// peer's: returning here abandoned every journal this loop had not
			// reached yet, in an order the hub chooses, and reported it to the
			// user as "offline".
			log.Printf("beardrive: skipping journal %s this cycle: %v", dev, err)
			continue
		}
		if bytes.Equal(local, data) {
			continue
		}
		// Resume only at a COMPLETE LINE. "Append-only" is the peer's claim
		// about its own object: publish a stage cut mid-line and both the size
		// gate and HasPrefix still pass, but the offset lands inside an op's
		// JSON, Parse drops the fragment, and the local copy is then
		// overwritten with the full object — so the next resume starts past an
		// op this device never applied, while every other device applies it.
		// The peer picks the split by choosing where to cut.
		accepted := local
		if i := bytes.LastIndexByte(accepted, '\n'); i >= 0 {
			accepted = accepted[:i+1]
		} else {
			accepted = nil
		}
		// An op we already applied must not be REDEFINED: if the object still
		// carries the (device, seq) slot we applied, it has to still be the
		// same op. This is an IDENTITY check, and it is what the op-count
		// guard below cannot do — a peer replaces an applied op's line with a
		// decodable but inert one (a delete of a path that never existed), the
		// count is preserved, and a file leaves every teammate's folder with no
		// delete op, nothing in the journal and nothing in History.
		//
		// It runs BEFORE the resume switch because it is needed on both arms:
		// `accepted` is trimmed to the last newline, but Parse does not require
		// a trailing newline, so an op published unterminated is APPLIED while
		// sitting outside the prefix HasPrefix protects — and a rewrite of that
		// op then looks like an ordinary append.
		applied, _ := journal.Parse(local)
		all, aerr := journal.Parse(data)
		if aerr != nil || redefinesApplied(all, applied) {
			continue
		}
		tail := data
		switch {
		case len(accepted) > 0 && bytes.HasPrefix(data, accepted):
			tail = data[len(accepted):]
		case len(accepted) > 0:
			// The peer rewrote its log instead of appending to it. Re-reading
			// it whole is fine for ordering (Replay is a fold) but it must not
			// UNDO: replacing an already-applied op's line with a longer
			// undecodable one grows the object and drops the op.
			prev, perr := journal.Parse(accepted)
			if perr != nil || len(all) < len(prev) {
				continue
			}
		}
		fresh, err := journal.Parse(tail)
		if err != nil {
			continue // corrupt remote journal; ignore rather than break sync
		}
		if err := store.WriteFileAtomic(lp, data, 0o644); err != nil {
			return newOps, gone, err
		}
		gone = append(gone, withdrawn(all, applied)...)
		newOps = append(newOps, fresh...)
	}

	// Fetch content for new ops. Blobs are uploaded before journals on push,
	// so anything referenced should exist — but Op.Blob is a string a peer
	// chose, so "missing" is a case this loop has to survive rather than a
	// contradiction. A blob that cannot be fetched is left unfetched:
	// materializeFile skips a path whose content is not in the store yet and
	// the next cycle retries, which is this package's posture for everything
	// transient. Abandoning the loop instead meant one op naming a blob that
	// was never pushed stopped every complete op behind it from ever landing.
	var bad error
	for _, op := range newOps {
		if op.Kind != journal.KindPut || op.Blob == "" || s.Store.HasBlob(op.Blob) {
			continue
		}
		// Large files: try the manifest first, sourcing unchanged chunks from
		// the version of this path we already hold. EVERY failure falls
		// through to the whole-blob path, not just errNoManifest: the whole
		// blob is independently hash-verified, so there is nothing to lose by
		// trying it — and not falling through let one member-written manifest
		// (the only object in the key space that is neither content-addressed
		// nor hash-checked at ingest) permanently deny a file whose correct
		// whole blob was sitting right there. A hash contradiction is still
		// remembered as the one signal worth surfacing (errBlobContent).
		if op.Size > chunkThreshold {
			var basis string
			if c, ok := cache[op.Path]; ok {
				basis = c.Blob
			}
			cerr := s.fetchChunked(ctx, op, basis)
			if cerr == nil {
				continue
			}
			// Fall through to the whole blob only when one actually EXISTS
			// (Exists never triggers hub reassembly). When it does, the
			// fallthrough is unconditional — the whole blob is independently
			// hash-verified, so a poisoned manifest cannot deny it. When it
			// does not — the normal chunked-only case — a transient chunk
			// failure retries chunks next cycle instead of asking the hub to
			// reassemble and re-download the entire file every tick.
			//
			// A chunked-path hash contradiction is recorded ONLY when no
			// fallback can land the file: when the whole blob arrives fine,
			// "blob corrupt on remote" would page an operator about a file
			// that just converged.
			if ok, eerr := s.Backend.Exists(ctx, "blobs/"+op.Blob); eerr != nil || !ok {
				if errors.Is(cerr, errBlobContent) && bad == nil {
					bad = cerr
				}
				continue
			}
		}
		rc, err := s.Backend.Get(ctx, "blobs/"+op.Blob)
		if err != nil {
			continue
		}
		sum, _, err := s.Store.PutBlobReader(io.LimitReader(rc, pullBound(op.Size)))
		rc.Close()
		if err != nil {
			return newOps, gone, err
		}
		if sum != op.Blob {
			// Report it, but do NOT abandon the batch. Content addressing
			// already protects the disk (PutBlobReader files bytes under their
			// COMPUTED hash, so HasBlob(op.Blob) stays false and materializeFile
			// keeps skipping the path); the check earns its place as the only
			// SIGNAL that a hub is serving bytes that are not what they are
			// addressed as, since otherwise the case is indistinguishable from
			// "not uploaded yet".
			//
			// Returning here made that signal a weapon. op.Size bounds the read,
			// and op.Size is a field the PEER wrote with no relation to the
			// object it names — so declaring 1 byte for a real 3 MiB blob
			// truncated an honest read, failed the sha, and dropped every blob
			// queued BEHIND it in the same batch. The journal is on local disk
			// by then, so the next cycle yields no new ops and the loop is never
			// re-entered: one integer in one journal line decided which of a
			// peer's files each teammate never received. The mismatch is now
			// remembered and returned once the rest of the batch has been
			// fetched, so it still lands in res.Offline/OfflineErr.
			if bad == nil {
				bad = fmt.Errorf("%w: blob %s corrupt on remote (got %s)", errBlobContent, shortSha(op.Blob), shortSha(sum))
			}
			continue
		}
	}
	return newOps, gone, bad
}

// shortSha trims a blob string for a message. Op.Blob is arbitrary JSON off a
// peer's journal, not necessarily 64 hex characters, so slicing it directly
// panicked the daemon on every device that pulled the line.
func shortSha(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// conflictCopies detects paths edited concurrently — we hold a not-yet-pushed
// op and just pulled a competing op for the same path. Last-writer-wins
// resolves the path itself deterministically; here the device that observed
// the concurrency preserves the losing version (ours or the pulled one) as a
// conflict-copy file so no content is silently dropped.
// It runs BEFORE the re-assertion step on purpose: a re-asserted op is not this
// device's edit — it carries the withdrawn op's original (and therefore usually
// losing) lamport — so having it in myOps here made it a losing local unpushed
// op by construction, and a peer with no collision at all could make a victim
// author, sign and push a conflict copy at a path that never existed, holding
// content the peer chose.
func (s *Session) conflictCopies(myOps []journal.Op, pushed int64, pulled []journal.Op, st *store.SyncState) ([]journal.Op, error) {
	if pushed > int64(len(myOps)) {
		pushed = int64(len(myOps))
	}
	unpushed := map[string]journal.Op{}
	for _, op := range myOps[pushed:] {
		// adoptNote: the same reasoning one step removed — an adopted op was
		// demoted precisely because the project's version wins, so it is a
		// losing unpushed local op by construction and would conflict-copy
		// every path a joining folder shares with the project. That is the
		// litter step 1b exists to remove.
		if op.Note == reassertNote || op.Note == adoptNote {
			// A re-asserted op is not an edit this device made — it restates a
			// peer's op that the peer withdrew, carrying that op's original
			// (and therefore usually losing) clock. Round 9 kept it out of the
			// unpushed set by ORDERING (re-assert after this step), which holds
			// for exactly one cycle: `pushed` only advances on a successful
			// push, and a read-only member is the documented steady state where
			// local ops stay journaled and unpushed forever. So it came back
			// into this set on every later cycle, and two withdrawals made the
			// victim author and journal peer-chosen content at a new path.
			// Marked, not ordered.
			delete(unpushed, op.Path)
			continue
		}
		unpushed[op.Path] = op // latest local op per path
	}
	pulledLatest := map[string]journal.Op{}
	for _, op := range pulled {
		if _, ok := unpushed[op.Path]; !ok {
			continue
		}
		if prev, ok := pulledLatest[op.Path]; !ok || journal.Less(prev, op) {
			pulledLatest[op.Path] = op
		}
	}
	if len(pulledLatest) == 0 {
		return nil, nil
	}
	all, err := s.Store.AllOps()
	if err != nil {
		return nil, err
	}
	state := journal.Replay(all)
	seqBase := int64(len(myOps))
	var out []journal.Op
	for p, theirs := range pulledLatest {
		mine := unpushed[p]
		cur, exists := state[p]
		mineWon := (mine.Kind == journal.KindPut && exists && cur.Blob == mine.Blob) ||
			(mine.Kind == journal.KindDelete && !exists)
		loser := mine
		if mineWon {
			loser = theirs
		}
		if loser.Kind != journal.KindPut || loser.Blob == "" {
			continue // a lost delete needs no preservation
		}
		if exists && cur.Blob == loser.Blob {
			continue // identical content; nothing actually lost
		}
		if !s.Store.HasBlob(loser.Blob) {
			continue // content unavailable (partial pull); skip rather than fail
		}
		st.Lamport = tickLamport(st.Lamport)
		seqBase++
		out = append(out, journal.Op{
			Seq: seqBase, Lamport: st.Lamport, Time: time.Now().UTC(),
			Device: s.Device.ID, DeviceName: s.Device.Name, Author: s.Device.Author,
			User: s.Account.Email, UserName: s.Account.Name,
			Kind: journal.KindPut, Path: conflictName(p, loser.DeviceName, loser.Time),
			Blob: loser.Blob, Size: loser.Size, Mode: loser.Mode,
			Note: "conflict copy of " + p,
		})
	}
	return out, nil
}

// conflictName builds the copy's name. Both variable parts are bounded: the
// loser's DeviceName is an unvalidated string off a peer's journal, and the
// result has to be a name the filesystem accepts (NAME_MAX is 255 everywhere
// beardrive runs). An unwritable name is worse than an ugly one — the op is
// already in this device's own journal by the time the write is attempted, so
// it would replay and fail on every cycle from then on, triggered by one
// ordinary concurrent edit.
func conflictName(p, deviceName string, t time.Time) string {
	suffix := ".bdrive-conflict-" + clip(sanitize(deviceName), 32) + "-" + t.UTC().Format("20060102T150405Z")
	dir, base := path.Split(p)
	return dir + clip(base, 255-len(suffix)) + suffix
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}

// materialize applies the merged state to the working folder, never
// clobbering files that changed since the scan earlier in this cycle.
// Filtered paths are not written: other devices' files that match the local
// ignore/include rules simply don't appear here.
// A path that cannot be written is skipped, not fatal. Op.Path is a peer's
// string and the working folder is a real filesystem: a NUL byte, a 400-byte
// segment, a put for "docs/child.md" after a put for "docs", or a directory in
// the way are all ordinary refusals from the kernel. Failing the cycle on one
// of them wedged the device permanently — the op stays in the pulled journal,
// so every later cycle replayed it and died at the same line, and Cycle
// returns before finish() so the state cache was never saved either.
func (s *Session) materialize(target map[string]journal.FileState, cache map[string]store.CachedFile, filter *Filter) (int, error) {
	changed, skipped := 0, 0
	var firstErr error
	skip := func(err error) {
		skipped++
		if firstErr == nil {
			firstErr = err
		}
	}
	defer func() {
		if skipped > 0 {
			log.Printf("beardrive: %d path(s) could not be written this cycle (first: %v)", skipped, firstErr)
		}
	}()
	for rel, want := range target {
		// neverSync as well as the ignore filter: the builtin exclusions are
		// what keep .bdrive/ and .git/ off this device's disk, and an op
		// naming one arrives from a peer's journal — where the scan-side
		// check never ran. Writing it would let one device repoint another's
		// mount (or drop a git hook that runs on the next commit).
		if filter.Skip(rel) || neverSync(rel) {
			continue
		}
		wrote, err := s.materializeFile(rel, want, cache)
		if err != nil {
			skip(err)
			continue
		}
		if wrote {
			changed++
		}
	}

	for rel, c := range cache {
		if _, ok := target[rel]; ok {
			continue
		}
		// The write loop above got unsafeRel, neverSync and UnderRoot in round
		// 4; this loop got none of them, and it ends in os.Remove.
		if unsafeRel(rel) || neverSync(rel) {
			delete(cache, rel)
			continue
		}
		if filter.Skip(rel) {
			// The path left sync scope rather than being deleted — someone
			// ignored it, or `--prune` removed it from the hub. Stop tracking
			// it; the file itself is ours to keep. Without this guard a prune
			// (or any delete op for a now-filtered path) unlinks every peer's
			// local copy, which is the data loss the feature exists to avoid.
			delete(cache, rel)
			continue
		}
		abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
		if !store.UnderRoot(s.Folder, abs) {
			delete(cache, rel)
			continue
		}
		if fi, err := os.Stat(abs); err == nil {
			if fi.Size() != c.Size || fi.ModTime().UnixNano() != c.MTimeNS {
				continue // dirty; do not delete fresh local edits
			}
			if err := os.Remove(abs); err != nil {
				skip(err)
				continue
			}
			pruneEmptyDirs(s.Folder, filepath.Dir(abs))
			// Tell the next agent turn what vanished under it. Best-effort:
			// a spool failure must never fail a cycle. Only the branch that
			// actually unlinked a file logs — the paths below it were already
			// gone locally, so nothing changed under the agent.
			s.logInbound(rel, true)
		}
		delete(cache, rel)
		changed++
	}
	return changed, nil
}

// materializeFile writes one path of the merged state into the working
// folder, reporting whether it wrote. It never clobbers a file that changed
// since the scan earlier in this cycle. Split out of materialize so the cycle
// can land .bdriveignore on its own, before the rules are needed.
func (s *Session) materializeFile(rel string, want journal.FileState, cache map[string]store.CachedFile) (bool, error) {
	want.Mode = safeMode(want.Mode) // before the cache compare, or every cycle rewrites
	c, ok := cache[rel]
	if ok && c.Blob == want.Blob && c.Mode == want.Mode {
		return false, nil
	}
	abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
	if fi, err := os.Stat(abs); err == nil {
		if ok && (fi.Size() != c.Size || fi.ModTime().UnixNano() != c.MTimeNS) {
			return false, nil // dirty: changed mid-cycle, next scan commits it
		}
		if !ok {
			// Untracked file already at this path: adopt if identical,
			// otherwise leave it for the next scan to journal.
			sum, err := hashFile(abs)
			if err != nil || sum != want.Blob {
				return false, nil
			}
		}
	}
	if !s.Store.HasBlob(want.Blob) {
		return false, nil // content not fetched yet; retry next cycle
	}
	if err := s.writeFile(abs, want); err != nil {
		return false, fmt.Errorf("write %s: %w", rel, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return false, err
	}
	cache[rel] = store.CachedFile{Blob: want.Blob, Size: fi.Size(), Mode: want.Mode, MTimeNS: fi.ModTime().UnixNano()}
	// Spool it for the next agent turn ("changed since your last turn"). Only
	// peer content reaches here — a local edit is journaled by the scan and
	// lands in the cache, so the compare above short-circuits before any
	// write. Best-effort: a spool failure must never fail a cycle.
	//
	// Known trade: the first cycle on a fresh mount materializes the whole
	// project, so that one turn's list is "everything" — bounded by the
	// hook's render cap, and cheaper than special-casing an empty cache.
	s.logInbound(rel, false)
	return true, nil
}

// pruneOps journals a delete for every path the hub still holds that the
// shared rules now exclude, and drops it from target so this cycle does not
// write it back. Peers keep their copies: the delete arrives alongside the
// rules that explain it, and materialize's filter guard turns it into
// "stop tracking" rather than "unlink".
//
// It reconciles against the replayed remote state, not the local cache. A
// path filtered out in some earlier cycle was dropped from the cache back
// then and is invisible locally today — which is exactly the leak --prune
// exists to clean up.
//
// The rules are deliberately ignore-only. .bdriveignore syncs, so every
// device agrees on it; the include list lives in this device's own
// .bdrive/config.json and does not sync. Never reuse the cycle's main filter
// here: a device with a legacy include-list scope would delete files a
// whole-folder teammate legitimately syncs.
func (s *Session) pruneOps(target map[string]journal.FileState, st *store.SyncState, seqBase int64) ([]journal.Op, error) {
	shared, err := loadFilter(s.Folder, nil) // nil include: shared rules only
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", IgnoreFile, err)
	}
	// The `!` refusal has to be made against the rules the prune is about to
	// APPLY. The CLI's pruneSafe reads .bdriveignore before the cycle; the
	// pull then materializes whatever version a peer pushed, and this reads it
	// again — two reads of two different files, so a teammate running `bdrive
	// scope`/`--only` (which writes exactly these rules) turned a cleared
	// prune into a hub-wide delete of everything outside their scope. No
	// malice required. The CLI check stays, as a nicer early error.
	if shared.Negated() {
		return nil, nil
	}
	var paths []string
	for rel := range target {
		if shared.Skip(rel) || neverSync(rel) {
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths) // map order is random; keep the journal reproducible
	ops := make([]journal.Op, 0, len(paths))
	for _, rel := range paths {
		st.Lamport = tickLamport(st.Lamport)
		seqBase++
		ops = append(ops, journal.Op{
			Seq: seqBase, Lamport: st.Lamport, Time: time.Now().UTC(),
			Device: s.Device.ID, DeviceName: s.Device.Name, Author: s.Device.Author,
			User: s.Account.Email, UserName: s.Account.Name,
			Kind: journal.KindDelete, Path: rel,
			Note: "pruned: excluded by " + IgnoreFile,
		})
		delete(target, rel)
	}
	return ops, nil
}

// neverSync reports whether a path is one the scan walk never uploads at all
// — the builtin exclusions, which prune treats exactly like ignore rules — or
// one no journal may name at all (see unsafeRel). Every path that reaches the
// working folder routes through here.
//
// One spelling, config.ReservedPath, rather than a per-segment loop of its
// parts: the builtin exclusions grew a whole-PATH rule (an agent's
// project-level hook config, config.AgentHookConfig) that "a reserved dir plus
// a reserved base name" cannot express, and a second copy of the rule is a copy
// that will miss the next one.
func neverSync(rel string) bool {
	return unsafeRel(rel) || config.ReservedPath(rel)
}

// unsafeRel reports whether an op's Path escapes the mount root. scan only
// ever produces clean relative paths, but Op.Path is arbitrary JSON off a
// peer's journal and materialize resolves it with filepath.Join(s.Folder, …),
// which walks above the root without complaint — one pushed line would reach
// ~/.ssh/authorized_keys on every teammate's machine.
//
// The rule itself is journal.SafePath, shared with the hub's two ingest doors:
// this spelling used to be its own copy and was the one MISSING the
// control-character clause, so the /store/* journal door accepted paths
// /upload/commit answers 400 to.
func unsafeRel(rel string) bool { return !journal.SafePath(rel) }

// safeMode is the only mode materialize will apply. scan records
// info.Mode().Perm(), but Op.Mode is a raw uint32 off the wire and
// fs.ModeSetuid/ModeSetgid live in that same word — os.Chmod would turn a
// peer's op into a setuid binary in every teammate's folder. Group/other write
// goes too: a synced file is never a drop box for other users on the machine.
func safeMode(m uint32) uint32 { return m & 0o777 &^ 0o022 }

func (s *Session) writeFile(abs string, want journal.FileState) error {
	// unsafeRel judged the path's SPELLING; this is the same boundary on disk.
	// MkdirAll, CreateTemp and Rename all follow symlinks, so a directory
	// inside the mount that is a symlink makes "docs/x.md" a perfectly clean
	// relative path landing outside the mount — and walkFolder refuses to
	// descend into one, so such a directory is a one-way door: it takes peer
	// writes and never reports them. Checked before MkdirAll, so a refused op
	// does not even build the parent chain on the far side.
	if !store.UnderRoot(s.Folder, abs) {
		return fmt.Errorf("resolves outside the mount root")
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	src, err := s.Store.OpenBlob(want.Blob)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	mode := os.FileMode(want.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), abs)
}

// push uploads blobs referenced by unpushed ops, then the journal itself.
// Blob-before-journal ordering means peers never see an op whose content is
// missing.
func (s *Session) push(ctx context.Context, myOps []journal.Op, st *store.SyncState) error {
	if st.PushedOps > int64(len(myOps)) {
		st.PushedOps = int64(len(myOps))
	}
	// Collect the unique, not-yet-pushed blobs to upload (deduped by content
	// hash). The backend's Put is idempotent and already skips content that's
	// present remotely (the hub reports it during signing), so we don't pay a
	// separate existence round-trip per blob.
	seen := map[string]bool{}
	type blobJob struct {
		blob string
		size int64
	}
	var jobs []blobJob
	var totalBytes int64
	for _, op := range myOps[st.PushedOps:] {
		if op.Kind != journal.KindPut || op.Blob == "" || seen[op.Blob] {
			continue
		}
		seen[op.Blob] = true
		jobs = append(jobs, blobJob{op.Blob, op.Size})
		totalBytes += op.Size
	}

	var done, bytesDone int64
	report := func() {
		if s.OnProgress != nil {
			s.OnProgress(Progress{
				Done: int(atomic.LoadInt64(&done)), Total: len(jobs),
				Bytes: atomic.LoadInt64(&bytesDone), ToBytes: totalBytes,
			})
		}
	}
	report() // announce the total up front (0 / N)

	// Upload blobs in parallel — the initial import is bound on serial
	// round-trips, not bandwidth, so concurrency is the win.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(pushConcurrency)
	for _, j := range jobs {
		g.Go(func() error {
			// Large files move as chunks + a manifest (chunks.go); the whole
			// blob is never uploaded for them. Everything else is unchanged.
			if j.size > chunkThreshold {
				n, err := s.pushChunked(gctx, j.blob)
				if err != nil {
					return err
				}
				atomic.AddInt64(&done, 1)
				atomic.AddInt64(&bytesDone, n)
				report()
				return nil
			}
			f, err := s.Store.OpenBlob(j.blob)
			if err != nil {
				return err
			}
			fi, err := f.Stat()
			if err != nil {
				f.Close()
				return err
			}
			err = s.Backend.Put(gctx, "blobs/"+j.blob, f, fi.Size())
			f.Close()
			if err != nil {
				return err
			}
			atomic.AddInt64(&done, 1)
			atomic.AddInt64(&bytesDone, fi.Size())
			report()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	jp := s.Store.JournalPath(s.Device.ID)
	f, err := os.Open(jp)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := s.Backend.Put(ctx, "journal/"+s.Device.ID+".jsonl", f, fi.Size()); err != nil {
		return err
	}
	st.PushedOps = int64(len(myOps))
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func pruneEmptyDirs(root, dir string) {
	root = filepath.Clean(root)
	for {
		dir = filepath.Clean(dir)
		if dir == root || !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// DisplayTime is the timestamp to show a human for an op: when the file was
// written if we know it, otherwise when the op was committed. Ops written
// before Op.Mtime existed, and deletes (no file left to stat), fall back.
func DisplayTime(op journal.Op) time.Time {
	// Clamped to the moment the op was journaled. Mtime comes off the
	// filesystem, so a peer chooses it: an op stamped in the year 9999 sits
	// above every real entry in `bdrive log` forever, and a handful of them
	// pushes the genuine history off a screen that prints 50 rows. Lagging
	// Time is legitimate (an old file, journaled today); leading it is not a
	// write time, it is a sort key someone picked.
	//
	// Op.Time is no more verified than Op.Mtime — same JSON line, same peer —
	// so bounding one by the other only moves the value a field to the left.
	// The one clock a peer does not own is this machine's: a stamp later than
	// now is not a write time at all, and an op we cannot date does not get to
	// outrank the changes we can. It sorts last rather than first, which is
	// the direction that cannot be aimed.
	now := time.Now()
	if !op.Mtime.IsZero() && !op.Mtime.After(op.Time) && !op.Mtime.After(now) {
		return op.Mtime
	}
	if op.Time.After(now) {
		return time.Time{}
	}
	return op.Time
}

// CommitTime is when a change entered the project, which is the question
// `bdrive log` answers. It is not DisplayTime: `mv` preserves mtime, so the put
// half of a rename carries the original file's write time and sorts away from
// the delete half of the same rename — a file that appeared seconds ago lands
// below the fold.
//
// Same clamp as DisplayTime, for the same reason: Op.Time is a peer's JSON, and
// the one clock a peer does not own is this machine's. An op we cannot date
// sorts last rather than first, which is the direction that cannot be aimed.
func CommitTime(op journal.Op) time.Time {
	if op.Time.After(time.Now()) {
		return time.Time{}
	}
	return op.Time
}

// SortForDisplay orders ops newest-first by CommitTime — when the change
// entered the project — so the two halves of a rename sit together and an old
// file added today sorts by when it arrived. DisplayTime breaks ties and is
// load-bearing: everything from one scan shares a commit second, and the files'
// own write times are the only thing that orders it inside that second. Ties
// beyond that fall back to reversed journal.Less to stay deterministic. This is
// deliberately NOT the replay order: LogEntries keeps returning causal order
// because bdrive restore walks it to find a file's previous version.
func SortForDisplay(ops []journal.Op) {
	sort.SliceStable(ops, func(i, j int) bool {
		ti, tj := CommitTime(ops[i]), CommitTime(ops[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		di, dj := DisplayTime(ops[i]), DisplayTime(ops[j])
		if !di.Equal(dj) {
			return di.After(dj)
		}
		return journal.Less(ops[j], ops[i])
	})
}

// LogEntries returns the volume history, newest first.
func LogEntries(st *store.Store, pathFilter string, limit int) ([]journal.Op, error) {
	all, err := st.AllOps()
	if err != nil {
		return nil, err
	}
	journal.Sort(all)
	// reverse
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if pathFilter != "" {
		filtered := all[:0]
		for _, op := range all {
			if op.Path == pathFilter || strings.HasPrefix(op.Path, pathFilter+"/") || path.Dir(op.Path) == pathFilter {
				filtered = append(filtered, op)
			}
		}
		all = filtered
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
