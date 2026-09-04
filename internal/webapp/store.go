package webapp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// The store API (/api/store/*) lets other devices sync through this server
// instead of talking to the object store themselves: the server is the only
// machine that knows where the storage is or holds credentials. It exposes
// the same key space every backend uses (blobs/<sha256>, journal/<dev>.jsonl)
// so the regular sync machinery works unchanged over it.
//
// Reads are always allowed (this is the same data the viewer serves). Writes
// follow the server's upload setting and go direct-to-storage via presigned
// URLs when the backend can sign, exactly like browser uploads.

var (
	blobKeyRe    = regexp.MustCompile(`^blobs/[0-9a-f]{64}$`)
	journalKeyRe = regexp.MustCompile(`^journal/` + deviceIDPattern + `\.jsonl$`)
	// Delta sync (docs/delta-sync-prd.md): a chunk is one content-defined
	// piece of a large file, keyed by its own sha256; a manifest is the chunk
	// list for the whole file with that sha256 — so Op.Blob alone locates it
	// and the journal format never changes. Both are immutable and never
	// deleted, exactly like blobs.
	chunkKeyRe    = regexp.MustCompile(`^chunks/[0-9a-f]{64}$`)
	manifestKeyRe = regexp.MustCompile(`^manifests/[0-9a-f]{64}$`)
)

func validStoreKey(key string) bool {
	return blobKeyRe.MatchString(key) || journalKeyRe.MatchString(key) ||
		chunkKeyRe.MatchString(key) || manifestKeyRe.MatchString(key)
}

// storeAcceptEncoding is what handleStoreSign tells a client this hub will
// inflate on a relayed PUT. A hub older than this answers without the field,
// and the client sends raw — which is the whole mixed-fleet story for the push
// leg (the pull leg needs no negotiation at all, see remote/compress.go).
var storeAcceptEncoding = []string{"gzip"}

// maxInflatedPut bounds what one compressed PUT may write to the hub's disk.
//
// spool() is unbounded, which is safe exactly as long as a byte on the wire
// costs a byte on disk. Content-Encoding breaks that: a 1 MB body can inflate
// to an arbitrary write, and it lands BEFORE CheckWrite can refuse it, because
// nothing knows the size until the inflate is done. The bound applies only when
// the body declares an encoding, so no honest raw push that works today can
// start failing.
//
// ponytail: 256 MiB, mirroring maxImportBlob on the archive path — a precedent,
// not a measurement. A real workload that hits it wants a server-side knob, not
// a bigger constant.
const maxInflatedPut = 256 << 20

// storeSource returns the volume's RemoteSource; only real beardrive
// remotes have a store to expose.
func storeSource(v *volume, w http.ResponseWriter) *RemoteSource {
	rs, ok := v.source.(*RemoteSource)
	if !ok {
		http.Error(w, "this server does not front a beardrive remote", http.StatusNotFound)
		return nil
	}
	return rs
}

func (s *Server) storeKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.URL.Query().Get("key")
	if !validStoreKey(key) {
		http.Error(w, fmt.Sprintf("invalid store key %q", key), http.StatusBadRequest)
		return "", false
	}
	return key, true
}

// ownJournal binds a journal key to the calling device. "Each device writes
// only its own journal" is why no journal object ever has two writers and why
// the hub needs no locking service — write permission on the project is not
// permission to rewrite a peer's log (or the hub's own), where a forged op
// with a high lamport wins replay on every device and History blames the
// victim. Blob keys are content-addressed and immutable, so they carry no
// owner.
//
// A caller that names no device writes no journal either, but that is a 400:
// the request never said who is writing, which is a malformed sync request
// rather than a refused one. Only a caller claiming to be a device it is not
// gets the 403.
//
// The body's ops are deliberately NOT read here any more. They used to admit
// a first claim when every op named the device, which is a field the writer
// writes — see the default arm below.
func (s *Server) ownJournal(w http.ResponseWriter, r *http.Request, key string) bool {
	if !strings.HasPrefix(key, "journal/") {
		return true
	}
	// The canonical spelling on both sides. A journal key IS a storage key, and
	// the stores underneath disagree about case (APFS and NTFS fold, S3 does
	// not), so a device that may spell its id two ways is a device that owns two
	// keys and one file — see canonDeviceID. Requiring the canonical key means
	// one device is one object everywhere.
	dev := deviceID(r)
	if dev == "" {
		http.Error(w, "a journal write must identify its device (X-Bdrive-Device)", http.StatusBadRequest)
		return false
	}
	if key != "journal/"+dev+".jsonl" {
		http.Error(w, "a device may only write its own journal", http.StatusForbidden)
		return false
	}
	// Matching the key against the header binds nothing on its own: the same
	// request supplies both, so moving them together satisfies the check by
	// construction and any member could replace any peer's journal object —
	// their ops gone, every peer replaying the forged ones, History crediting
	// them to the victim. The device has to belong to the ACCOUNT as well.
	//
	// Ownership is DeviceRegistry.OwnerOf: hub-wide, first claim, ownerless
	// rows claiming nothing. Three things this deliberately does not do, each
	// because doing it was a hole:
	//
	//   - it does not consult the row this request would create. Every /store
	//     handler used to register the caller's header before asking who owns
	//     it, so an unclaimed id authorized whoever named it first. The
	//     callers observe AFTER this returns.
	//   - it does not treat "unclaimed" as permission. An id nothing has ever
	//     synced under is not this caller's to write.
	//   - it does not scope the claim to the project's org, so offboarding a
	//     teammate does not release her journal to the org she left.
	//
	// A hub with no registry cannot resolve ownership at all (single-volume,
	// auth-less, or a fixture): there is nobody to impersonate, and projectPerm
	// answers admin for exactly those configurations.
	if s.Devices != nil {
		me := normEmail(s.requestUser(r).Email)
		owner, _ := s.Devices.OwnerOf(dev)
		switch {
		case normEmail(owner) != "" && normEmail(owner) == me:
			// My device, my journal.
		case atLeast(s.projectPerm(r, r.PathValue("project")), PermAdmin):
			// Project admin is the recovery path — the answer to "a squatted id
			// is a permanent lockout". The device's own remedy is in the body.
		default:
			// There is no "first writer claims an unowned id" arm any more.
			// It used to admit `!known && journalNames(dev, ops)` — every op in
			// the body naming the device — which reads a field the WRITER
			// writes, so it cost one request to take any id that had not yet
			// pushed a journal. That included every device of every read-only
			// member, permanently, because a device that syncs with READ can
			// never reach this door to claim its own id in the first place.
			//
			// A device id is now bound to its account when the hub mints that
			// machine's token (DeviceRegistry.Bind), which is a moment the hub
			// authenticates and the machine cannot forge. So an unowned id is
			// simply not anybody's to write, and the remedy is to sign in.
			// A hub that has never bound ANY device is not refusing this
			// caller — it is refusing everyone, and no amount of signing in
			// will change it, because its auth provider is not calling the
			// binder (AuthProvider.UseDeviceBinder). The user-facing text
			// cannot say that: a brand-new hub also holds no bindings, and
			// telling its first user the server is broken would be wrong. The
			// operator is the one who can act, so this goes to the log, once.
			if s.Devices != nil && !s.Devices.AnyOwned() {
				s.unboundOnce.Do(func() {
					log.Printf("beardrive: refused a journal write and NO device is registered on this hub — " +
						"if this hub uses a custom AuthProvider, it must call the binder handed to " +
						"UseDeviceBinder at every point it mints a CLI token, or every push from every " +
						"device will be refused no matter how often anyone signs in")
				})
			}
			// "run `bdrive login`" alone sent one user in a circle for an
			// afternoon: the binding is made by the login request naming its
			// device, which a CLI older than this gate does not do, so signing in
			// again succeeded, changed nothing, and every push kept 403ing with
			// the same sentence. The hub and the CLI deploy separately, so the
			// skew is the expected state right after this ships — the refusal has
			// to name the upgrade, not just the command.
			http.Error(w, "this device is not registered to your account on this hub; "+
				"update bdrive, then run `bdrive login` on this machine (an older CLI signs in "+
				"without registering its device). If the id belongs to someone else, delete "+
				"device.json in your BearDrive home first, or ask a project admin",
				http.StatusForbidden)
			return false
		}
	}
	return true
}

func (s *Server) handleStoreList(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	s.refreshDevice(r)
	prefix := r.URL.Query().Get("prefix")
	if prefix != "" &&
		!strings.HasPrefix(prefix, "journal/") && !strings.HasPrefix(prefix, "blobs/") &&
		!strings.HasPrefix(prefix, "chunks/") && !strings.HasPrefix(prefix, "manifests/") {
		http.Error(w, fmt.Sprintf("invalid prefix %q", prefix), http.StatusBadRequest)
		return
	}
	// A sync cycle starts here, which makes it the hub's regular opportunity to
	// confirm what its presigned grants actually delivered.
	s.reconcileGrants(r.Context(), r.PathValue("project"), rs.Backend)
	objs, err := rs.Backend.List(r.Context(), prefix)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "storage is temporarily unavailable", err)
		return
	}
	writeStoreJSON(w, r, map[string]any{"objects": objs})
}

// writeStoreJSON is writeJSON that compresses when the caller accepts it. The
// listing is the first call of every sync cycle on every device, it is JSON,
// and it is highly repetitive — one key per blob — so it is compressed without
// probing. Only this route uses it: writeJSON serves the whole browser API and
// is out of this change's scope, and handleStoreExists answers one boolean,
// which is smaller than a gzip header.
func writeStoreJSON(w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set("Content-Type", "application/json")
	if !remote.AcceptsGzip(r) {
		json.NewEncoder(w).Encode(v)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	gz := gzip.NewWriter(w)
	defer gz.Close()
	json.NewEncoder(gz).Encode(v)
}

func (s *Server) handleStoreGet(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	s.refreshDevice(r)
	key, ok := s.storeKey(w, r)
	if !ok {
		return
	}
	// Blobs go through OpenBlob so a presigned write cannot make this route
	// serve content that does not hash to the key it is stored under.
	var rc io.ReadCloser
	var err error
	if blob, isBlob := strings.CutPrefix(key, "blobs/"); isBlob {
		rc, err = rs.OpenBlob(r.Context(), blob)
	} else {
		rc, err = rs.Backend.Get(r.Context(), key)
	}
	if err != nil {
		// Fixed message: os.Open's error names the hub's absolute storage
		// path, and S3's names the bucket and key.
		storageErr(w, http.StatusNotFound, "no such object", err)
		return
	}
	defer rc.Close()
	// Compression is decided before a single header is written, because
	// Content-Encoding cannot be added after the first Write.
	var src io.Reader = rc
	gzipOK := false
	if remote.AcceptsGzip(r) {
		src, gzipOK, err = remote.Compressible(rc)
		if err != nil {
			storageErr(w, http.StatusBadGateway, "could not read the object", err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	// The sync proxy is a stored-bytes door like the other two: a
	// cookie-authenticated GET whose URL one member can hand another, answering
	// with content the attacker wrote under a Content-Type the hub chose.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Recorded, never checked. This is a device syncing: refusing it here
	// surfaces as ErrForbidden, which the syncer reads as "access is gone —
	// pause and touch nothing". Sync must not break over a bill.
	//
	// The counter wraps the SOCKET and gzip writes into it, never the other way
	// round: RecordEgress is a bandwidth meter, so it has to report what left
	// the machine. Inverted, it silently bills plaintext for every compressed
	// response and no test fails.
	cw := &countingWriter{w: w}
	if gzipOK {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(cw)
		io.Copy(gz, src)
		gz.Close()
	} else {
		io.Copy(cw, src)
	}
	s.quota().RecordEgress(s.orgOf(r.PathValue("project")), cw.n)
}

func (s *Server) handleStoreExists(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	s.refreshDevice(r)
	key, ok := s.storeKey(w, r)
	if !ok {
		return
	}
	exists, err := rs.Backend.Exists(r.Context(), key)
	if err != nil {
		storageErr(w, http.StatusBadGateway, "storage is temporarily unavailable", err)
		return
	}
	writeJSON(w, map[string]any{"exists": exists})
}

// handleStoreSign answers how a client should upload a key: a presigned
// direct-to-storage URL when the backend can sign, through the server
// otherwise — same contract as browser uploads.
func (s *Server) handleStoreSign(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	if !s.Upload.Enabled {
		http.Error(w, "uploads are disabled on this server", http.StatusForbidden)
		return
	}
	var req struct {
		Key  string `json:"key"`
		Size int64  `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !validStoreKey(req.Key) || req.Size < 0 {
		http.Error(w, fmt.Sprintf("invalid store key %q", req.Key), http.StatusBadRequest)
		return
	}
	// Ownership is deliberately NOT consulted here. Signing grants nothing for
	// a journal — journals are never presigned, the answer is always "come
	// through the server" — so the only thing asking could do is turn
	// OwnerOf's hub-wide answer into a status code on a route any org member
	// may call: a plain member of one org probed a device id belonging to a
	// separate tenant and the response told him whether it existed. The write
	// itself is where ownership is enforced, and that is the only place it
	// needs to be.
	s.refreshDevice(r)
	project := r.PathValue("project")
	s.reconcileGrants(r.Context(), project, rs.Backend)
	org := s.orgOf(project)
	// The cap is checked against this write PLUS everything already granted
	// and not yet accounted for, so concurrent grants cannot oversubscribe an
	// allowance that no single one of them exceeds.
	if err := s.quota().CheckWrite(org, req.Size+s.reservedBytes(org)); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	// Only blobs and chunks are presigned. They are content-addressed and
	// immutable, so a leaked URL can at worst re-upload identical bytes.
	// Journals are mutable state and always flow through the server — and so
	// do manifests: their key is the whole FILE's sha, not their own content
	// hash, so a presigned manifest write would be an unexamined claim about
	// bytes the hub never saw.
	blob, isBlob := strings.CutPrefix(req.Key, "blobs/")
	if !isBlob {
		blob, isBlob = strings.CutPrefix(req.Key, "chunks/")
	}
	if isBlob {
		if !sizeFitsContentAddress(blob, req.Size) {
			http.Error(w, "declared size does not match the content address", http.StatusForbidden)
			return
		}
		if exists, err := rs.Backend.Exists(r.Context(), req.Key); err == nil && exists {
			writeJSON(w, map[string]any{"mode": "direct", "exists": true, "accept_encoding": storeAcceptEncoding})
			return
		}
		if signer, ok := rs.Backend.(remote.PutSigner); ok {
			// Reserved, not charged: the bytes go straight to storage, so this
			// grant counts against the cap immediately and is billed when the
			// object is confirmed there (reconcileGrants), or released for
			// free when the URL expires unused. Booking it here outright
			// charged 20 GiB for 20 JSON posts. The check and the reservation
			// are one critical section, so concurrent callers cannot all read
			// the same zero and oversubscribe.
			if err := s.reserveIfFits(project, org, req.Key, req.Size, s.Upload.ttl()); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			if signed, err := signer.SignPut(r.Context(), req.Key, req.Size, s.Upload.ttl()); err == nil {
				writeJSON(w, map[string]any{
					"mode": "direct", "url": signed.URL, "method": signed.Method,
					"headers": signed.Headers, "expires": signed.Expires.UTC(),
					"accept_encoding": storeAcceptEncoding,
				})
				return
			}
			s.claimGrant(project, req.Key) // nothing was granted: give it back
		}
	}
	writeJSON(w, map[string]any{"mode": "server", "accept_encoding": storeAcceptEncoding})
}

// journalOps reads the operations a spooled journal body carries, exactly the
// way every device reads it (journal.Parse: a line that decodes to no
// operation is no operation). It leaves the file rewound for the store.
// A non-journal key carries no ops by definition.
//
// It is also the hub's ONLY path check on this door. /store/* is the second
// ingest into a project's tree and it used to validate nothing: the browser
// door (cleanUploadPath) refused control characters and this one journaled
// them, so "notes\x00.md" reached the tree, the metadata store and the Share
// button through the door round 6 said refusing at ingest had closed. The
// rule is journal.SafePath — the same one the device applies in unsafeRel and
// the same one cleanUploadPath is built on.
func journalOps(key string, tmp *os.File) ([]journal.Op, error) {
	if !strings.HasPrefix(key, "journal/") {
		return nil, nil
	}
	data, err := io.ReadAll(tmp)
	if err != nil {
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	ops, err := journal.Parse(data)
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		// The same two clauses the browser door applies (cleanUploadPath):
		// SafePath, plus the reserved dirs. Applying only the first here left
		// /store/* journaling ".git/hooks/pre-commit" with a 200 while /remove
		// and /shares answered 400 for the same path — so the entry was in the
		// tree, served to every device, and no request could take it back out.
		if !journal.SafePath(op.Path) || config.ReservedPath(op.Path) {
			return nil, fmt.Errorf("journal names an invalid path %q", op.Path)
		}
		// The note is the other peer-written free text History renders, right
		// next to the path (HistoryRow's NoteText, the run-card header) — and
		// `bdrive log` already scrubs the same characters out of it on the way
		// to a terminal, on the stated grounds that "the audit tool an operator
		// uses to catch a peer must not be renderable BY that peer". The web
		// History view is the audit tool everybody actually uses.
		// The note is not the OTHER peer-written free text History renders, it
		// is one of three: Op.Author and Op.UserName are rendered in the same
		// row by the same helper (the frontend's whoChanged), and were checked
		// by nothing. A right-to-left override in user_name reorders the whole
		// rendered row — the Trojan Source shape SafeText exists to refuse —
		// and a C0 run in author is the "renders as nothing" shape its own doc
		// comment names. DeviceName is absent on purpose: History serves the
		// device REGISTRY's name, not the op's.
		// Op.Session joins that list for the same reason: History serves it
		// beside the note and the frontend groups run cards on it, so it is
		// peer-written free text rendered in the audit surface.
		if !journal.SafeText(op.Note) || !journal.SafeText(op.Author) ||
			!journal.SafeText(op.UserName) || !journal.SafeText(op.Session) {
			return nil, fmt.Errorf("journal carries invalid text")
		}
	}
	return ops, nil
}

// opsNameTheirAuthor refuses a journal whose ops credit an account other than
// the one that owns the device writing them.
//
// Op.User/Op.UserName are what History serves and what the frontend's
// whoChanged() renders as THE answer to "who changed this file?" — the hub's
// only audit surface. They arrived as fields the pushing client typed, so bob
// pushed an op declaring alice and the audit log named Alice. The hub already
// holds the truth on this very request: ownJournal has just resolved the
// account the device id in the key belongs to.
//
// It refuses rather than overwrites: a journal object is the device's own log
// byte for byte, every peer replays exactly these bytes, and rewriting a body
// mid-push would make the hub a second author of a log the design says has
// exactly one.
//
// An op that names nobody at all is fine — journals from before accounts
// existed have no User and History falls back to Author. A NAME with no account
// is not: it is an attribution with nothing behind it, rendered the same way.
//
// "Names nobody" has to include Author, and that was the bypass: this checked
// User alone and explicitly waved through an op with neither User nor UserName,
// while whoChanged() falls back to Author — unchecked peer text — as THE answer
// to "who changed this file?". So bob pushed an op naming nobody, put
// "Alice <alice@x.io>" in author, and the audit surface credited Alice again,
// one field over from the fix. A device the hub has bound got its token from a
// login, so it has an account to name; if it names anything, it names that one.
func (s *Server) opsNameTheirAuthor(w http.ResponseWriter, r *http.Request, ops []journal.Op) bool {
	if s.Devices == nil || len(ops) == 0 {
		return true // nobody to impersonate (single-volume, auth-less, fixture)
	}
	owner, _ := s.Devices.OwnerOf(deviceID(r))
	if normEmail(owner) == "" {
		return true // no binding to check against; ownJournal already ruled on the write
	}
	for _, op := range ops {
		if op.User == "" && op.UserName == "" && op.Author == "" {
			continue
		}
		if normEmail(op.User) != normEmail(owner) {
			http.Error(w, "an op must name the account this device is registered to as its author",
				http.StatusForbidden)
			return false
		}
	}
	return true
}

// opsWithinScope refuses a journal whose ops name a folder this account may
// not write. Folder rules narrow the project level over a subtree (folders.go);
// proj() has already established the caller may write the PROJECT.
func (s *Server) opsWithinScope(w http.ResponseWriter, r *http.Request, ops []journal.Op) bool {
	p, ok := projectFromCtx(r)
	if !ok || len(p.Folders) == 0 {
		return true
	}
	base := s.projectPermOf(r, p)
	if base == PermAdmin {
		return true
	}
	email := normEmail(s.requestUser(r).Email)
	for _, op := range ops {
		if atLeast(folderLevel(p, email, op.Path, base), PermWrite) {
			continue
		}
		// The path is named back deliberately: the caller wrote this op, so it
		// is their own text, and a device whose push is refused with no way to
		// tell which file caused it cannot be debugged by the person holding
		// the laptop.
		http.Error(w, fmt.Sprintf("you have read-only access to %q in this project", op.Path),
			http.StatusForbidden)
		return false
	}
	return true
}

// journalKeepsItsOps reports whether an incoming journal body still carries
// every op the hub already holds under key, matched on the per-device sequence
// number — the append-only rule the data model states and that /store/object, a
// plain object PUT, enforced nowhere.
//
// "Every change is an Op in a per-device append-only JSONL log" is why History
// can answer "who changed this file?" at all. Without this, the writer of a
// journal can replace it with a SHORTER one: every op it held is gone from
// replay, from every peer, and from the hub's only audit surface. Two principals
// reach that — the device's own account erasing its own trail, and, once
// offboarding releases the id and the laptop is reassigned, whoever inherits it
// erasing the departed member's.
//
// Seq is the field to key on: it is the device's own monotone counter, an honest
// client never reuses one (store.AppendOps only appends), so "no stored Seq has
// vanished" is the log-only-grows statement in the model's own terms.
//
// ponytail: this refuses TRUNCATION, not rewriting-in-place — a body that keeps
// every Seq but changes what one of them says still edits the record. The
// stronger rule is a byte-prefix compare against the stored object, which is
// what an honest client always produces; it is not what ships here because two
// established fixtures drive this door by replacing a journal wholesale
// (sec_audit2, sec_path), and a security fix that rewrites the tests around it
// is a fix nobody can audit. Upgrade path: byte prefix, with those two fixtures
// switched to append.
//
// A key the hub does not hold yet is a first push and keeps everything. A stored
// journal the hub cannot parse protects nothing and is not a reason to refuse a
// device's sync — it is the hub's own corruption, and ingest (journalOps) has
// refused unparseable bodies since round 6. A backend that cannot answer fails
// the push closed: the client degrades to Offline and retries next cycle, which
// is the posture everywhere else on this path.
//
// storedMax is the highest Seq the hub already holds, returned because this is
// the only place that parses the stored journal and analytics needs it to tell
// this cycle's new ops from the whole history the body repeats (countOps). It
// is 0 for a first push and for a stored journal that would not parse — the
// latter over-counts one push, which is the right price for not reading the
// object twice.
func journalKeepsItsOps(ctx context.Context, be remote.Backend, key string, ops []journal.Op) (ok bool, storedMax int64, err error) {
	switch have, err := be.Exists(ctx, key); {
	case err != nil:
		return false, 0, err
	case !have:
		return true, 0, nil
	}
	rc, err := be.Get(ctx, key)
	if err != nil {
		return false, 0, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return false, 0, err
	}
	stored, err := journal.Parse(data)
	if err != nil {
		log.Printf("beardrive: %s is not parseable, its ops cannot be protected from a rewrite: %v", key, err)
		return true, 0, nil
	}
	seen := make(map[int64]bool, len(ops))
	for _, op := range ops {
		seen[op.Seq] = true
	}
	for _, op := range stored {
		if !seen[op.Seq] {
			return false, 0, nil
		}
		if op.Seq > storedMax {
			storedMax = op.Seq
		}
	}
	return true, storedMax, nil
}

// storePutBody hands back the plaintext of a PUT body, inflating it when the
// client declared an encoding, and reports whether it did. The reader it
// returns is capped one byte past maxInflatedPut so a bomb can never write more
// than that to disk before the caller measures it and refuses.
//
// It answers the client itself on the two ways this can be the client's fault:
// an encoding this hub does not implement, and a body that says gzip and is
// not one (gzip.NewReader reads the header eagerly, so that is caught here
// rather than halfway through a spool).
func storePutBody(w http.ResponseWriter, r *http.Request) (io.Reader, bool, bool) {
	enc := strings.TrimSpace(r.Header.Get("Content-Encoding"))
	if enc == "" {
		return r.Body, false, true
	}
	if !strings.EqualFold(enc, "gzip") {
		http.Error(w, "unsupported Content-Encoding "+enc, http.StatusUnsupportedMediaType)
		return nil, false, false
	}
	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		http.Error(w, "body declares Content-Encoding: gzip but is not gzip", http.StatusBadRequest)
		return nil, false, false
	}
	return io.LimitReader(gz, maxInflatedPut+1), true, true
}

func (s *Server) handleStorePut(v *volume, w http.ResponseWriter, r *http.Request) {
	rs := storeSource(v, w)
	if rs == nil {
		return
	}
	if !s.Upload.Enabled {
		http.Error(w, "uploads are disabled on this server", http.StatusForbidden)
		return
	}
	key, ok := s.storeKey(w, r)
	if !ok {
		return
	}
	// Spool the body before storing any of it. Everything this handler has to
	// be sure of is a property of the bytes, not of the headers the client
	// sent: what a blob key promises (its sha256), what the write costs
	// (Content-Length is -1 on any chunked request, which made every unsized
	// put free), and how many ops a journal write actually authors.
	// Cost: one temp file per put on the hub's busiest write path.
	//
	// Inflating sits ABOVE the spool, because every single thing this handler
	// goes on to be sure of is a property of the plaintext: the sha a blob key
	// promises, the ops journalOps counts, the size CheckWrite bills, and the
	// append-only check. Nothing below this line knows compression happened.
	body, inflated, ok := storePutBody(w, r)
	if !ok {
		return
	}
	tmp, size, sum, err := spool(body)
	if err != nil {
		if inflated {
			// A gzip stream that truncates or fails its CRC is the client's
			// body, not the hub's storage; 502 would blame the wrong machine.
			http.Error(w, "could not decompress the body", http.StatusBadRequest)
			return
		}
		storageErr(w, http.StatusBadGateway, "could not store the object", err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if inflated && size > maxInflatedPut {
		http.Error(w, "compressed body inflates past this hub's limit", http.StatusRequestEntityTooLarge)
		return
	}
	// Blobs and chunks are content-addressed: the key IS the content's hash.
	// Manifests are not — their key is the whole FILE's sha, which the hub
	// cannot check without reading every chunk; readers verify by reassembly.
	if blob, isBlob := strings.CutPrefix(key, "blobs/"); isBlob && blob != sum {
		http.Error(w, "content does not hash to its key", http.StatusBadRequest)
		return
	}
	if chunk, isChunk := strings.CutPrefix(key, "chunks/"); isChunk && chunk != sum {
		http.Error(w, "content does not hash to its key", http.StatusBadRequest)
		return
	}
	ops, err := journalOps(key, tmp)
	if err != nil {
		// The body is the client's, so everything journalOps can object to is
		// the client's fault: an undecodable journal or an op naming a path
		// this hub will not carry. 400, not 502 — and nothing is stored.
		http.Error(w, "invalid journal body", http.StatusBadRequest)
		return
	}
	if !s.ownJournal(w, r, key) {
		return
	}
	if !s.opsNameTheirAuthor(w, r, ops) {
		return
	}
	// The folder gate for the sync wire, and the only write door a device has:
	// a blob is content-addressed and inert until an op names it, and
	// handleStoreSign has no path in its request to gate on. So "may this
	// account write this folder?" is answered exactly here, over the ops the
	// handler has already parsed for two other checks.
	//
	// The whole PUT is refused, not the offending ops: a journal is
	// append-only and its writer's own record, so the hub may not edit one.
	// An up-to-date client never reaches this — it learns its scope from
	// /api/p/<id>/scope and never journals a path it cannot write — which is
	// what keeps a member who edits a read-only folder from wedging their own
	// sync forever. This is the check for the stale client and the hostile one.
	if !s.opsWithinScope(w, r, ops) {
		return
	}
	// Observed only after the write is authorized, and only into a row the
	// account ALREADY owns (refreshDevice checks OwnerOf first). Nothing here
	// claims: that happens once, when the hub mints this machine's token
	// (DeviceRegistry.Bind).
	//
	// The journal branch used to call observeDevice, which creates the row it
	// does not find — and ownJournal's admin arm lets a project admin write
	// somebody else's journal as the RECOVERY path. Anyone who can create a
	// project is admin of it, so one PUT into a project of the attacker's own
	// wrote a competing row {attacker, victim's device id}, and Bind refuses
	// any id another account holds a row for: the victim's `bdrive login` was
	// then 409 forever, across the org wall, and `bdrive login` is the
	// documented remedy for every other device problem. A recovery path may
	// not brick the recovery path.
	s.refreshDevice(r)
	project := r.PathValue("project")
	s.reconcileGrants(r.Context(), project, rs.Backend)
	org := s.orgOf(project)
	if err := s.quota().CheckWrite(org, size+s.reservedBytes(org)); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	var storedMax int64
	if strings.HasPrefix(key, "journal/") {
		var ok bool
		var err error
		switch ok, storedMax, err = journalKeepsItsOps(r.Context(), rs.Backend, key, ops); {
		case err != nil:
			storageErr(w, http.StatusBadGateway, "could not read the stored journal", err)
			return
		case !ok:
			http.Error(w, "a journal is append-only; this body drops ops the hub already holds",
				http.StatusConflict)
			return
		}
	}
	// A manifest is the one member-writable object that is neither
	// content-addressed nor hash-checkable at ingest, so it is WRITE-ONCE:
	// overwriting one was the only way a member could re-point an existing
	// file's chunk list after the fact. Re-putting identical bytes stays a
	// no-op — the retry after an interrupted push (chunks and manifest up,
	// journal not yet) must keep working.
	if strings.HasPrefix(key, "manifests/") {
		// Every chunk a manifest names must already be in the store. This is
		// what makes "a manifest exists ⟹ its chunks exist" an INVARIANT
		// rather than an honest-client convention: the manifest key is the
		// one member-writable object that is not content-addressed, and a
		// member who can read a file can publish its true chunk hashes
		// without uploading a byte — poisoning the slot under a whole-pushed
		// blob so a later honest push skips chunks that do not exist. The
		// honest client writes chunks before the manifest, so this always
		// passes for it; a refusal falls back to a whole-blob push on the
		// client (pushChunked), so even a race costs one full upload, never
		// a wedge.
		var man struct {
			Chunks []struct {
				H string `json:"h"`
			} `json:"chunks"`
		}
		if err := json.NewDecoder(io.LimitReader(tmp, 8<<20)).Decode(&man); err != nil {
			http.Error(w, "unreadable manifest body", http.StatusBadRequest)
			return
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			storageErr(w, http.StatusBadGateway, "could not store the object", err)
			return
		}
		for _, c := range man.Chunks {
			if !blobRe.MatchString(c.H) {
				http.Error(w, "manifest names an invalid chunk", http.StatusBadRequest)
				return
			}
			ok, err := rs.Backend.Exists(r.Context(), "chunks/"+c.H)
			if err != nil {
				storageErr(w, http.StatusBadGateway, "could not verify the manifest's chunks", err)
				return
			}
			if !ok {
				http.Error(w, "manifest names a chunk the store does not hold", http.StatusBadRequest)
				return
			}
		}
		// Exists first, then Get: `if Get succeeds, compare` fails OPEN on a
		// transient storage error — exactly the flakiness that must not
		// reopen the overwrite door this guard exists to close.
		exists, eerr := rs.Backend.Exists(r.Context(), key)
		if eerr != nil {
			storageErr(w, http.StatusBadGateway, "could not check the stored manifest", eerr)
			return
		}
		if exists {
			rc, gerr := rs.Backend.Get(r.Context(), key)
			if gerr != nil {
				storageErr(w, http.StatusBadGateway, "could not read the stored manifest", gerr)
				return
			}
			stored, rerr := io.ReadAll(io.LimitReader(rc, 8<<20))
			rc.Close()
			fresh, ferr := io.ReadAll(tmp)
			if _, serr := tmp.Seek(0, io.SeekStart); rerr != nil || ferr != nil || serr != nil {
				storageErr(w, http.StatusBadGateway, "could not compare the stored manifest", errors.Join(rerr, ferr, serr))
				return
			}
			if !bytes.Equal(stored, fresh) {
				http.Error(w, "a manifest is write-once; this key already holds a different one",
					http.StatusConflict)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
			return
		}
	}
	if err := rs.Backend.Put(r.Context(), key, tmp, size); err != nil {
		storageErr(w, http.StatusBadGateway, "could not store the object", err)
		return
	}
	// These bytes came through the hub, so they are charged here — drop any
	// reservation for the same key rather than charging it twice.
	s.claimGrant(project, key)
	s.quota().RecordUsage(org, size)
	if strings.HasPrefix(key, "journal/") {
		v.invalidate() // new ops should show in the viewer immediately
		puts, deletes := countOps(ops, storedMax)
		s.captureChange(r, "sync", puts, deletes)
	}
	writeJSON(w, map[string]any{"ok": true})
}
