package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// Uploads run in two modes, chosen by the server per request so the client
// never needs to know what the storage is:
//
//	direct: POST /api/upload/init returns a presigned, expiring URL; the
//	        client PUTs the content straight to the object store (blob first,
//	        keyed by its sha256), then POST /api/upload/commit journals it.
//	        Storage credentials never leave the server.
//	server: the backend can't presign (file://, plain folders); the client
//	        PUTs the content to /api/upload/content and the server stores it.
//
// The blobs-before-journal invariant holds in both modes: commit refuses to
// journal an op whose blob is not already in the store.

// Uploader is implemented by sources that accept writes through the server.
// who is the signed-in account the write should be attributed to (zero when
// auth is off), and note rides along on the journaled op — "" for an ordinary
// browser upload, a stated origin when the HUB itself authored the bytes
// (seedTemplate). Both are on the interface because the journal is the hub's
// only audit surface: a write with no human behind it must not be able to
// present as one by omission.
type Uploader interface {
	Upload(ctx context.Context, path string, r io.Reader, size int64, who User, note string) error
}

// DirectUploader is additionally implemented by sources whose storage can
// accept presigned direct uploads.
type DirectUploader interface {
	Uploader
	SignBlobPut(ctx context.Context, blob string, size int64, ttl time.Duration) (*remote.SignedPut, error)
	// BlobSize reports the stored size of a blob and whether it is there at
	// all. Size comes from storage, never from the caller: in direct mode the
	// server never sees the bytes, so this is the only true byte count it can
	// quota-check and journal.
	BlobSize(ctx context.Context, blob string) (int64, bool, error)
	// note rides along on the journaled op — "" for an ordinary upload,
	// "restore <path>@<sha8>" when the write is a restore.
	Commit(ctx context.Context, path, blob string, size int64, who User, note string) error
}

// ---- RemoteSource: writes go to the object store + our own journal ----

// SignBlobPut presigns a direct upload of the blob, if the backend can sign.
func (r *RemoteSource) SignBlobPut(ctx context.Context, blob string, size int64, ttl time.Duration) (*remote.SignedPut, error) {
	signer, ok := r.Backend.(remote.PutSigner)
	if !ok {
		return nil, fmt.Errorf("backend cannot presign uploads")
	}
	return signer.SignPut(ctx, "blobs/"+blob, size, ttl)
}

func (r *RemoteSource) BlobSize(ctx context.Context, blob string) (int64, bool, error) {
	o, ok, err := r.blobStat(ctx, blob)
	return o.Size, ok, err
}

// blobStat is the stored object behind a blob: its size, its last-modified
// time (zero on a backend that does not report one), and whether it is there.
// One metadata call, no egress.
func (r *RemoteSource) blobStat(ctx context.Context, blob string) (remote.Object, bool, error) {
	key := "blobs/" + blob
	objs, err := r.Backend.List(ctx, key) // full key as prefix: at most one hit
	if err != nil {
		return remote.Object{}, false, err
	}
	for _, o := range objs {
		if o.Key == key {
			return o, true, nil
		}
	}
	return remote.Object{}, false, nil
}

// presignTTL is how long a presigned upload URL to this source stays valid —
// the window in which a blob is writable by someone other than the hub.
func (r *RemoteSource) presignTTL() time.Duration {
	if r.PresignTTL > 0 {
		return r.PresignTTL
	}
	return DefaultUploadTTL
}

// spool reads src to a temp file, rewound and ready to re-read, and reports
// its real size and sha256. The server needs both before it stores anything:
// a client's declared size and a content address are claims, not facts. The
// caller closes and removes the file.
func spool(src io.Reader) (f *os.File, size int64, sum string, err error) {
	tmp, err := os.CreateTemp("", ".bdrive-tmp-upload-")
	if err != nil {
		return nil, 0, "", err
	}
	h := sha256.New()
	size, err = io.Copy(tmp, io.TeeReader(src, h))
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, "", err
	}
	return tmp, size, hex.EncodeToString(h.Sum(nil)), nil
}

// Upload stores content through the server: spool to disk while hashing,
// push the blob, then journal the op.
func (r *RemoteSource) Upload(ctx context.Context, p string, src io.Reader, _ int64, who User, note string) error {
	tmp, size, blob, err := spool(src)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	// Blob before journal, always.
	if err := r.Backend.Put(ctx, "blobs/"+blob, tmp, size); err != nil {
		return fmt.Errorf("push blob: %w", err)
	}
	return r.Commit(ctx, p, blob, size, who, note)
}

// Commit appends a put op for path→blob to this server's own journal. It
// refuses if the blob is not in the store yet (a peer must never see an op
// whose content is missing). Only this server writes this journal key, so
// the read-modify-write below has a single writer; upmu serializes it across
// concurrent requests.
func (r *RemoteSource) Commit(ctx context.Context, p, blob string, size int64, who User, note string) error {
	if r.Device.ID == "" {
		return fmt.Errorf("no device identity configured for uploads")
	}
	_, ok, err := r.BlobSize(ctx, blob)
	if err != nil {
		return fmt.Errorf("check blob: %w", err)
	}
	if !ok {
		return errBlobMissing
	}
	return r.appendOp(ctx, journal.Op{
		Kind: journal.KindPut, Path: p, Blob: blob, Size: size, Mode: 0o644,
		User: who.Email, UserName: who.Name, Note: note,
	})
}

// nextLamport is maxLamport+1 without the wrap.
func nextLamport(cur int64) int64 {
	if cur == math.MaxInt64 {
		return cur
	}
	return cur + 1
}

// appendOp stamps op with this server's identity and ordering and appends it
// to this server's own journal. Callers fill in Kind/Path and the content
// fields; Seq, Lamport, Time and the device fields belong to us.
func (r *RemoteSource) appendOp(ctx context.Context, op journal.Op) error {
	return r.appendOps(ctx, []journal.Op{op})
}

// appendOps is the same write for N ops at once, in ONE read-modify-write.
// Every journal write in this package goes through it — appendOp is the
// single-op call — so there is exactly one place that stamps the hub's
// identity and exactly one that rewrites the key.
//
// The batch is not an optimization detail: it is what makes a multi-path
// write atomic. One Put of one object either lands or it does not, so a
// run-wide undo (undorun.go) can never leave half a run reverted. A caller
// that loops appendOp instead gets N whole-journal round trips AND a
// partially-applied write with nothing to report it.
func (r *RemoteSource) appendOps(ctx context.Context, ops []journal.Op) error {
	// Never rewrite an unchanged journal: a Put of identical bytes still
	// bumps Modified, which invalidates every reader's journal cache.
	if len(ops) == 0 {
		return nil
	}
	r.upmu.Lock()
	defer r.upmu.Unlock()

	all, err := r.loadOps(ctx)
	if err != nil {
		return err
	}
	var maxLamport, mySeq int64
	for _, prev := range all {
		maxLamport = max(maxLamport, prev.Lamport)
		if prev.Device == r.Device.ID {
			mySeq = max(mySeq, prev.Seq)
		}
	}
	now := time.Now().UTC()
	lam := maxLamport
	for i := range ops {
		// Saturating, like the client's tickLamport. maxLamport is taken over
		// every journal the hub can see, members' included, and int64 addition
		// wraps: one pushed op carrying MaxInt64 made the hub's next lamport
		// MinInt64 — recomputed on every commit, so every later browser upload
		// in the project silently lost last-writer-wins while commit still
		// answered 200. At saturation the batch's lamports stop increasing and
		// journal.Less orders the rest through (time, device, seq), which is
		// still a total order — so replay stays deterministic.
		lam = nextLamport(lam)
		ops[i].Seq, ops[i].Lamport, ops[i].Time = mySeq+int64(i)+1, lam, now
		ops[i].Device, ops[i].DeviceName, ops[i].Author = r.Device.ID, r.Device.Name, r.Device.Author
	}

	// Read-modify-write of our own journal. A transient read error must fail
	// the commit — treating it as "no journal yet" would rewrite the key
	// without our earlier ops.
	key := "journal/" + r.Device.ID + ".jsonl"
	var existing []byte
	if exists, err := r.Backend.Exists(ctx, key); err != nil {
		return fmt.Errorf("check journal: %w", err)
	} else if exists {
		rc, err := r.Backend.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("fetch journal: %w", err)
		}
		existing, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	line, err := journal.Marshal(ops)
	if err != nil {
		return err
	}
	data := append(existing, line...)
	return r.Backend.Put(ctx, key, strings.NewReader(string(data)), int64(len(data)))
}

var errBlobMissing = fmt.Errorf("content not uploaded yet")

// emptyBlob is sha256(""), the only content whose size is legitimately zero.
const emptyBlob = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sizeFitsContentAddress is the one lie about a declared upload size that is
// provable before the bytes exist: zero, for content that is not the empty
// blob. In direct mode the bytes never pass through the hub, so the size is
// the caller's word — and it is the number the quota is charged against, which
// makes this exactly the lie that buys an unmetered presigned URL. Shared,
// because both doors take the number: /upload/init (browsers) and /store/sign
// (devices), and for a while only one of them checked.
func sizeFitsContentAddress(sha string, size int64) bool {
	return size > 0 || sha == emptyBlob
}

// ---- DirSource: writes land straight in the folder ----

// Upload writes the file atomically under Root. There is no journal here;
// on a mounted folder the daemon scans, journals, and syncs it like any
// local edit.
func (d *DirSource) Upload(_ context.Context, p string, src io.Reader, _ int64, _ User, _ string) error {
	dst := filepath.Join(d.Root, filepath.FromSlash(p))
	// cleanUploadPath rules out "..", but a path with no ".." in it still
	// leaves the folder by walking through a symlinked directory that was
	// already there. The viewer promises "this folder": resolve where the
	// write would actually land and refuse anything outside Root.
	//
	// Judged BEFORE anything is created. underRoot needs a directory that
	// exists (EvalSymlinks), so asking after MkdirAll meant a refused upload
	// had already built the whole parent chain on the other side of the
	// symlink. The deepest existing ancestor answers the same question: every
	// directory MkdirAll then creates is a real directory beneath it.
	if err := underRoot(d.Root, existingDir(filepath.Dir(dst))); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".bdrive-tmp-")
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
	return os.Rename(tmp.Name(), dst)
}

// existingDir walks up from dir to the deepest ancestor that exists on disk.
func existingDir(dir string) string {
	for {
		if _, err := os.Lstat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// underRoot reports whether dir, with every symlink resolved, is root or
// inside it.
func underRoot(root, dir string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if realDir != realRoot && !strings.HasPrefix(realDir, realRoot+string(filepath.Separator)) {
		return fmt.Errorf("path leaves the served folder")
	}
	return nil
}

// ---- HTTP handlers ----

var blobRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// cleanUploadPath validates a client-supplied destination path and returns
// its normalized form.
func cleanUploadPath(p string) (string, error) {
	// journal.SafePath is the rule — the same one the /store/* journal door
	// and the device's unsafeRel apply. This door used to carry its own copy
	// of it; the copies disagreed, which is how a control character that this
	// door answered 400 to got journaled through the other one.
	if !journal.SafePath(p) {
		return "", fmt.Errorf("invalid path %q", p)
	}
	cl := p
	// The same set the scan walk would never have uploaded, at any depth:
	// .bdrive/ is the mount's own identity (an upload of it repoints every
	// device that pulls) and .git/ materializes hook scripts that run on a
	// teammate's next commit. materialize re-checks this too — an op can also
	// arrive from a peer's journal — but a hub that journals one has already
	// handed it to every device.
	if config.ReservedPath(cl) {
		return "", fmt.Errorf("reserved name %q", cl)
	}
	return cl, nil
}

// gateUpload enforces the server's upload setting and returns the volume's
// writable source, failing the request if uploads are off or the source is
// read-only.
func (s *Server) gateUpload(v *volume, w http.ResponseWriter) Uploader {
	if !s.Upload.Enabled {
		http.Error(w, "uploads are disabled on this server", http.StatusForbidden)
		return nil
	}
	up := v.uploader()
	if up == nil {
		http.Error(w, "this source is read-only", http.StatusForbidden)
	}
	return up
}

type uploadReq struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func (s *Server) decodeUpload(w http.ResponseWriter, r *http.Request, needBlob bool) (uploadReq, bool) {
	var req uploadReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return req, false
	}
	p, err := cleanUploadPath(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return req, false
	}
	req.Path = p
	if req.Size < 0 {
		http.Error(w, "invalid size", http.StatusBadRequest)
		return req, false
	}
	if needBlob && !blobRe.MatchString(req.SHA256) {
		http.Error(w, "sha256 must be 64 lowercase hex chars", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// handleUploadInit tells the client how to upload this content: a presigned
// direct URL when the storage supports it, otherwise through the server.
func (s *Server) handleUploadInit(v *volume, w http.ResponseWriter, r *http.Request) {
	up := s.gateUpload(v, w)
	if up == nil {
		return
	}
	req, ok := s.decodeUpload(w, r, true)
	if !ok {
		return
	}
	if !sizeFitsContentAddress(req.SHA256, req.Size) {
		http.Error(w, "declared size does not match the content address", http.StatusForbidden)
		return
	}
	project := r.PathValue("project")
	if rs, ok := v.source.(*RemoteSource); ok {
		s.reconcileGrants(r.Context(), project, rs.Backend)
	}
	org := s.orgOf(project)
	if err := s.quota().CheckWrite(org, req.Size+s.reservedBytes(org)); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if direct, isDirect := up.(DirectUploader); isDirect {
		if _, exists, err := direct.BlobSize(r.Context(), req.SHA256); err == nil && exists {
			// Content already in the store (same file elsewhere, or a retry):
			// skip the upload, go straight to commit.
			writeJSON(w, map[string]any{"mode": "direct", "exists": true})
			return
		}
		// Reserved, exactly like the device door: counted against the cap now,
		// charged when the object is confirmed in storage, released for free
		// if the caller never uploads. A browser upload that never comes back
		// to commit is therefore still billed. Check and reservation are one
		// critical section (see reserveIfFits).
		if err := s.reserveIfFits(project, org, "blobs/"+req.SHA256, req.Size, s.Upload.ttl()); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		signed, err := direct.SignBlobPut(r.Context(), req.SHA256, req.Size, s.Upload.ttl())
		if err == nil {
			writeJSON(w, map[string]any{
				"mode":    "direct",
				"url":     signed.URL,
				"method":  signed.Method,
				"headers": signed.Headers,
				"expires": signed.Expires.UTC(),
			})
			return
		}
		// Backend can't presign right now (e.g. credentials that can't
		// sign): degrade to uploading through the server.
		s.claimGrant(project, "blobs/"+req.SHA256) // nothing was granted
	}
	writeJSON(w, map[string]any{"mode": "server"})
}

// handleUploadContent receives content through the server (server mode).
func (s *Server) handleUploadContent(v *volume, w http.ResponseWriter, r *http.Request) {
	up := s.gateUpload(v, w)
	if up == nil {
		return
	}
	p, err := cleanUploadPath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Spool first, then charge what actually arrived. Content-Length is -1 on
	// any chunked request, so max(r.ContentLength, 0) admitted an upload of any
	// size against a quota of zero bytes and billed it at zero — the hole round
	// 1 closed on the device door, still open on this one.
	tmp, size, _, err := spool(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("store: %v", err), http.StatusBadGateway)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	project := r.PathValue("project")
	if rs, ok := v.source.(*RemoteSource); ok {
		s.reconcileGrants(r.Context(), project, rs.Backend)
	}
	org := s.orgOf(project)
	if err := s.quota().CheckWrite(org, size+s.reservedBytes(org)); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := up.Upload(r.Context(), p, tmp, size, s.requestUser(r), ""); err != nil {
		http.Error(w, fmt.Sprintf("store: %v", err), http.StatusBadGateway)
		return
	}
	s.quota().RecordUsage(org, size)
	v.invalidate()
	s.captureChange(r, "browser", 1, 0)
	writeJSON(w, map[string]any{"ok": true, "path": p})
}

// handleUploadCommit journals a direct upload after the blob is in the store.
func (s *Server) handleUploadCommit(v *volume, w http.ResponseWriter, r *http.Request) {
	up := s.gateUpload(v, w)
	if up == nil {
		return
	}
	direct, isDirect := up.(DirectUploader)
	if !isDirect {
		http.Error(w, "this source has no direct-upload commit", http.StatusBadRequest)
		return
	}
	req, ok := s.decodeUpload(w, r, true)
	if !ok {
		return
	}
	// The blob may already sit in storage (direct upload), but commit is what
	// makes it part of the volume — so it is the accounting point, and the
	// only one in direct mode. Size comes from storage: req.Size is the
	// caller's claim, and it would otherwise be both the quota charge and the
	// journaled Op.Size.
	// Bytes are charged once, where they land. If this commit finalizes a
	// grant nothing has confirmed yet, it is the one charging them, from the
	// size storage reports; if the reconciler (or a relayed put) already did,
	// there is nothing left to claim and commit charges nothing — committing
	// a path is not a second copy of the content.
	claimed := s.claimGrant(r.PathValue("project"), "blobs/"+req.SHA256)
	size, exists, err := direct.BlobSize(r.Context(), req.SHA256)
	if err != nil {
		http.Error(w, fmt.Sprintf("commit: %v", err), http.StatusBadGateway)
		return
	}
	if !exists {
		http.Error(w, fmt.Sprintf("commit: %v", errBlobMissing), http.StatusConflict)
		return
	}
	org := s.orgOf(r.PathValue("project"))
	if err := s.quota().CheckWrite(org, size); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := direct.Commit(r.Context(), req.Path, req.SHA256, size, s.requestUser(r), ""); err != nil {
		code := http.StatusBadGateway
		if err == errBlobMissing {
			code = http.StatusConflict
		}
		http.Error(w, fmt.Sprintf("commit: %v", err), code)
		return
	}
	if claimed {
		s.quota().RecordUsage(org, size)
	}
	v.invalidate()
	s.captureChange(r, "browser", 1, 0)
	writeJSON(w, map[string]any{"ok": true, "path": req.Path})
}
