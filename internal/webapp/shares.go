package webapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runbear-io/beardrive/internal/secrets"
)

// Share links make one file publicly readable at /s/<unguessable-token> —
// no sign-in needed, which is the whole point: "here's the report" is just a
// URL. A link always serves the file's LATEST synced content (living wiki
// pages, evolving reports) and lives until revoked, unless created with an
// expiry. Everything else on the hub stays behind auth.
//
// Shared content renders (HTML as a page, markdown Obsidian-style, PDFs
// inline) but sandboxed: /s/ responses carry a strict CSP sandbox and never
// see auth cookies, so a malicious shared file's scripts run in an opaque
// origin and can't touch hub sessions.

// Share is one public link.
type Share struct {
	Token   string    `json:"token"`
	Project string    `json:"project"`
	Path    string    `json:"path"`
	Creator string    `json:"creator,omitempty"` // account email
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires,omitzero"` // zero = permanent until revoked
}

func (s Share) expired() bool {
	return !s.Expires.IsZero() && time.Now().After(s.Expires)
}

// ShareDB is the in-memory share registry over a MetaStore ShareRepo.
type ShareDB struct {
	repo ShareRepo

	mu      sync.Mutex
	ver     versionGate // skips the re-read when the store has not moved
	warned  bool        // "re-read failed" logged once (see refresh)
	byToken map[string]Share
}

// refresh re-reads the share registry from the store. Callers hold mu.
//
// fileShareRepo.reload closed the WRITE side of this in round 12 and its own
// comment names the row it did not close: a revoked link is gone from the file
// and still served by any hub process that did not handle the revocation, to
// anonymous strangers, for the life of that process. Revocation is the whole
// emergency stop for a leaked public URL, so the read that decides whether a
// /s/<token> is live has to read the store.
//
// A store that cannot answer leaves the map in place — see ProjectDB.refresh
// for the trade.
//
// ponytail: one full Load per share resolution; see OrgDB.refresh for the
// upgrade path if it ever shows up in a profile.
func (db *ShareDB) refresh() {
	token, stale := db.ver.stale(db.repo)
	if !stale {
		return
	}
	list, err := db.repo.Load()
	if err != nil {
		if !db.warned {
			db.warned = true
			log.Printf("beardrive: share registry re-read failed, serving the last known links: %v", err)
		}
		return
	}
	db.warned = false
	db.ver.fresh(token)
	next := make(map[string]Share, len(list))
	for _, s := range list {
		next[s.Token] = s
	}
	db.byToken = next
}

// NewShareDB builds the registry over a repo, loading its contents.
func NewShareDB(repo ShareRepo) (*ShareDB, error) {
	db := &ShareDB{repo: repo, byToken: make(map[string]Share)}
	list, err := repo.Load()
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		db.byToken[s.Token] = s
	}
	return db, nil
}

// OpenShareDB loads the file-backed registry at path.
func OpenShareDB(path string) (*ShareDB, error) {
	return NewShareDB(newFileShareRepo(path))
}

// Create returns a share for (project, path), reusing an existing live one
// so repeated shares of the same file hand out the same URL.
func (db *ShareDB) Create(project, p, creator string, ttl time.Duration) (Share, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	for _, s := range db.byToken {
		if s.Project == project && s.Path == p && !s.expired() && s.Expires.IsZero() && ttl == 0 {
			return s, nil
		}
	}
	s := Share{
		Token: randHex(16), Project: project, Path: p,
		Creator: creator, Created: time.Now().UTC(),
	}
	if ttl > 0 {
		s.Expires = time.Now().UTC().Add(ttl)
	}
	db.byToken[s.Token] = s
	if err := db.repo.Put(s); err != nil {
		delete(db.byToken, s.Token)
		return Share{}, err
	}
	return s, nil
}

// Get resolves a live (non-expired) share.
func (db *ShareDB) Get(token string) (Share, bool) {
	if db == nil {
		return Share{}, false
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	s, ok := db.byToken[token]
	if !ok || s.expired() {
		return Share{}, false
	}
	return s, true
}

// lookup resolves a share regardless of expiry. Authorization must not depend
// on the clock: Get filters expired rows, so a handler that skips its
// permission check when the lookup misses lets anyone delete an expired
// share — and learn from the answer that it existed.
func (db *ShareDB) lookup(token string) (Share, bool) {
	if db == nil {
		return Share{}, false
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	s, ok := db.byToken[token]
	return s, ok
}

func (db *ShareDB) Revoke(token string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	sh, ok := db.byToken[token]
	if !ok {
		return false
	}
	delete(db.byToken, token)
	if err := db.repo.Delete(token); err != nil {
		// Revocation is the emergency stop for a leaked public URL: a delete
		// the store refused comes back at the next restart, so put the row
		// back and report the failure rather than reporting a revocation that
		// isn't one. Same shape as OrgDB.RevokeInvite.
		db.byToken[token] = sh
		return false
	}
	return true
}

// SetExpiry re-dates a live share in place: ttl > 0 sets the expiry, ttl == 0
// makes it permanent again. The token is untouched, so a URL already on
// someone's clipboard keeps working — which is the point, and why this is not
// revoke-and-remint.
func (db *ShareDB) SetExpiry(token string, ttl time.Duration) (Share, bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	prev, ok := db.byToken[token]
	if !ok || prev.expired() {
		return Share{}, false, nil
	}
	s := prev
	if ttl > 0 {
		s.Expires = time.Now().UTC().Add(ttl)
	} else {
		s.Expires = time.Time{}
	}
	db.byToken[token] = s
	if err := db.repo.Put(s); err != nil {
		// Restore, don't delete: unlike Create's rollback the row already
		// existed, and dropping it would revoke a live link over a disk hiccup.
		db.byToken[token] = prev
		return Share{}, false, err
	}
	return s, true, nil
}

// List returns a project's live shares, newest first. The order has to be a
// total one: byToken is a map, so without a sort every call reshuffles the
// rows — and these rows carry a Revoke button, so "the second one" must mean
// the same link on every load.
func (db *ShareDB) List(project string) []Share {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.refresh()
	var out []Share
	for _, s := range db.byToken {
		if s.Project == project && !s.expired() {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		// Equal, not !=: a time.Time carries a monotonic reading and a
		// location, so two logically-equal instants can compare unequal —
		// which would make this comparator non-transitive and leave the
		// order worse than the map iteration it replaces.
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Token < out[j].Token
	})
	return out
}

// firstFileUnder returns the lexicographically smallest file under the folder
// p, or "" if p is not a folder. The prefix is p+"/" and never bare p: "notes"
// is a prefix of "notes-archive/x.md", and that mistake turns a genuine
// "not synced" into a wrong "that's a folder". Smallest, not whatever map
// iteration hands back, so identical calls suggest the same file.
func firstFileUnder(files map[string]FileInfo, p string) string {
	prefix := p + "/"
	best := ""
	for k := range files {
		if strings.HasPrefix(k, prefix) && (best == "" || k < best) {
			best = k
		}
	}
	return best
}

// ---- HTTP ----

// handleShareCreate mints (or returns) the share link for a file. Any
// signed-in member can share; the file must already be synced.
func (s *Server) handleShareCreate(v *volume, w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	var req struct {
		Path      string `json:"path"`
		ExpiresIn string `json:"expires_in,omitempty"` // Go duration, e.g. "168h"
		Confirm   bool   `json:"confirm,omitempty"`    // share it anyway, secrets and all
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p, err := cleanUploadPath(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if _, ok := snap.files[p]; !ok {
		// snap.files maps FILES, so a fully synced folder misses here just like
		// a path that does not exist. Tell those apart before answering, or the
		// user goes off to fix a sync fault that isn't there.
		if inside := firstFileUnder(snap.files, p); inside != "" {
			http.Error(w, fmt.Sprintf("share links are per-file; %s is a folder - try a file inside it, e.g. %s", p, inside), http.StatusBadRequest)
			return
		}
		http.Error(w, fmt.Sprintf("%s is not synced to this project yet", p), http.StatusNotFound)
		return
	}
	var ttl time.Duration
	if req.ExpiresIn != "" {
		if ttl, err = time.ParseDuration(req.ExpiresIn); err != nil || ttl <= 0 {
			http.Error(w, "invalid expires_in", http.StatusBadRequest)
			return
		}
	}
	if !req.Confirm && !s.alreadyPublic(r.PathValue("project"), p) {
		rc, err := v.source.Open(r.Context(), p, snap.files[p])
		if err != nil {
			// Fails CLOSED. The repo's "degrade rather than fail" posture is for
			// sync cycles; minting is a rare interactive action, and a check
			// that skips itself on a storage hiccup is exactly the false
			// confidence this gate exists to remove.
			storageErr(w, http.StatusServiceUnavailable, "could not read the file to check it for credentials", err)
			return
		}
		// 1 MiB and close: source.Open streams from the object store, so this
		// aborts the rest of the transfer rather than pulling a 500 MB file
		// down to look at its first megabyte. Don't "fix" it into a ReadAll.
		buf, err := io.ReadAll(io.LimitReader(rc, secrets.ScanLimit))
		rc.Close()
		if err != nil {
			storageErr(w, http.StatusServiceUnavailable, "could not read the file to check it for credentials", err)
			return
		}
		if findings := secrets.Scan(buf); len(findings) > 0 {
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error":    "this file looks like it contains credentials",
				"findings": findings,
			})
			return
		}
	}
	sh, err := s.Shares.Create(r.PathValue("project"), p, s.requestUser(r).Email, ttl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// No opens: a freshly minted link has nothing to report.
	writeJSON(w, shareJSON(r, sh, nil))
}

// alreadyPublic reports whether this path is already served to anyone with a
// URL. If it is, minting skips the credential scan: the content is public
// already, so withholding the link protects nothing and would break the
// "clicking Share again gives me the same link" behaviour the dialog is built
// on.
//
// It cannot key off ShareDB.Create's reuse branch, which is narrower (that one
// also requires no expiry on either side), and it has to drop links whose
// creator left the org — those 404 at /s/ (shareCreatorStillBelongs), so a
// secrets file whose only link is already dead must not wave through.
func (s *Server) alreadyPublic(project, p string) bool {
	if s.Shares == nil {
		return false
	}
	for _, sh := range s.Shares.List(project) { // List already drops expired
		if sh.Path == p && s.shareCreatorStillBelongs(sh) {
			return true
		}
	}
	return false
}

func (s *Server) handleShareList(v *volume, w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	project := r.PathValue("project")
	shares := s.Shares.List(project)
	opens := s.Reads.ShareOpens(project) // once, outside the loop — never per share
	out := make([]map[string]any, 0, len(shares))
	for _, sh := range shares {
		out = append(out, shareJSON(r, sh, opens))
	}
	writeJSON(w, map[string]any{"shares": out})
}

func (s *Server) handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	// This route is /api/shares/{token} — outside the proj() wrapper — so the
	// level check lives here: minting and killing public links are the same
	// authority.
	sh, ok := s.Shares.lookup(r.PathValue("token"))
	if !ok {
		http.Error(w, "no such share", http.StatusNotFound)
		return
	}
	if !s.requirePerm(w, r, sh.Project, PermWrite) {
		return
	}
	if s.Shares.Revoke(r.PathValue("token")) {
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	http.Error(w, "no such share", http.StatusNotFound)
}

// handleShareExpiry sets or clears the expiry on an existing link. Same shape
// and same authority as revoke — the token stays valid, only its lifetime
// changes.
func (s *Server) handleShareExpiry(w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		http.Error(w, "sharing is not enabled on this server", http.StatusNotFound)
		return
	}
	var req struct {
		ExpiresIn string `json:"expires_in"` // Go duration, "" = permanent
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	sh, ok := s.Shares.Get(r.PathValue("token"))
	if !ok {
		http.Error(w, "no such share", http.StatusNotFound)
		return
	}
	if !s.requirePerm(w, r, sh.Project, PermWrite) {
		return
	}
	var ttl time.Duration
	if req.ExpiresIn != "" {
		var err error
		if ttl, err = time.ParseDuration(req.ExpiresIn); err != nil || ttl <= 0 {
			http.Error(w, "invalid expires_in", http.StatusBadRequest)
			return
		}
	}
	updated, ok, err := s.Shares.SetExpiry(sh.Token, ttl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no such share", http.StatusNotFound)
		return
	}
	// Same as create: an expiry edit is not the surface that reports opens.
	writeJSON(w, shareJSON(r, updated, nil))
}

// shareJSON renders one link. opens is the project's share-open map from
// ReadLedger.ShareOpens, built ONCE by the caller and indexed here — passing
// nil means "not measured" (reads are off, or this is a single-share reply
// that has nothing to report yet), and then neither receipt key appears.
// Absent is not zero: `0` would be a lie on a hub with reads disabled.
func shareJSON(r *http.Request, sh Share, opens map[string]ShareOpen) map[string]any {
	out := map[string]any{
		"token": sh.Token, "path": sh.Path, "project": sh.Project,
		"url": requestBaseURL(r) + "/s/" + sh.Token, "created": sh.Created,
	}
	if sh.Creator != "" {
		out["creator"] = sh.Creator
	}
	if !sh.Expires.IsZero() {
		out["expires"] = sh.Expires
	}
	if opens != nil {
		// Keyed by path, not token: heat has no token dimension, so two
		// links on one file report the same number. Documented, and asserted
		// in shares_test.go so it can't regress into a silent wrong answer.
		o := opens[sh.Path]
		out["opens"] = o.Count
		if o.Count > 0 {
			out["last_opened"] = o.Last
		}
	}
	return out
}

// shareCreatorStillBelongs reports whether the account that minted a link is
// still in the project's org. A share is the strongest grant on the hub — the
// org's live content, to anyone with the URL, forever — so offboarding has to
// reach it: the day someone leaves, every link they minted stops serving.
//
// Resolved at read time rather than by walking shares.json on RemoveMember,
// for the same reason projectPerm resolves membership instead of walking grant
// maps: one rule, no sweep to forget, and it self-heals if the account rejoins.
// Suspended, not deleted — an owner still sees the orphaned link in the
// project's share list and can revoke it for good. Demotion (write → read) is
// deliberately NOT covered: "a link lives until revoked" is the contract, and
// only leaving the org ends it.
func (s *Server) shareCreatorStillBelongs(sh Share) bool {
	if s.Dir == nil || sh.Creator == "" {
		return true // no membership model (single-volume), or a pre-accounts link
	}
	// No org on the project — cleared, never set, or the project is gone —
	// means membership cannot be established, and on a public route that is a
	// refusal, not a pass. Failing open here resurrected every link an
	// offboarded member ever minted, one layer below the same fix projectPerm
	// got in round 1.
	org := s.orgOf(sh.Project)
	if org == "" {
		return false
	}
	return s.Dir.Role(org, sh.Creator) != ""
}

// handleShared serves a share link: public, sandboxed, always the latest
// synced content.
// shareActor identifies one reader of a link well enough to debounce their
// own reloads without folding a whole office into a single visit: token+IP
// alone made every browser behind one NAT the same reader (BEA-151).
//
// The User-Agent is HASHED, never stored raw — Record persists the actor into
// the read buckets and out through ReadRepo, and a raw User-Agent there is a
// fingerprint sitting in storage indefinitely. Truncated because this only
// ever needs to GROUP, never to identify, and it must not leave the ledger
// either way. token+"/"+IP stays the prefix so the existing leak assertions
// keep covering the wider key.
func (s *Server) shareActor(r *http.Request, token string) string {
	sum := sha256.Sum256([]byte(r.UserAgent()))
	return token + "/" + s.clientIP(r) + "/" + hex.EncodeToString(sum[:8])
}

func (s *Server) handleShared(w http.ResponseWriter, r *http.Request) {
	// Sandbox everything under /s/ before anything can answer the request:
	// shared content executes in an opaque origin (scripts allowed — charts in
	// reports — but no cookies, no same-origin reach back into the hub), and
	// an error page is a /s/ response like any other. Set here, not at the
	// end, so the 429 and the 404s cannot go out bare.
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-popups")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")

	if !s.shareLimiter().allow(s.clientIP(r)) {
		http.Error(w, "too many requests — slow down", http.StatusTooManyRequests)
		return
	}
	sh, ok := s.Shares.Get(r.PathValue("token"))
	if !ok {
		http.Error(w, "this link does not exist or was revoked", http.StatusNotFound)
		return
	}
	if !s.shareCreatorStillBelongs(sh) {
		http.Error(w, "this link does not exist or was revoked", http.StatusNotFound)
		return
	}
	_, v, err := s.projectVolume(sh.Project)
	if err != nil {
		http.Error(w, "this link does not exist or was revoked", http.StatusNotFound)
		return
	}
	snap, err := v.snapshot(r.Context())
	if err != nil {
		http.Error(w, "content temporarily unavailable", http.StatusBadGateway)
		return
	}
	// A share token is a promise about ONE file, so it follows that file
	// when it moves — the opposite of a viewer URL, which is an address and
	// always serves whatever lives there now. That also closes a leak: a
	// share used to serve whatever unrelated file later occupied its path.
	sp, ok := resolveShare(snap.moves, snap.files, sh.Path, sh.Created)
	if !ok {
		http.Error(w, "the shared file no longer exists", http.StatusNotFound)
		return
	}
	fi := snap.files[sp]
	// A share hit is external consumption: one audience member reloading is
	// debounced to a visit, distinct visitors still count.
	s.Reads.Record(sh.Project, sp, ReadKindShare, s.shareActor(r, sh.Token))

	// Share links are the only unauthenticated door to stored bytes, so they
	// are the only egress a plan actually caps. The per-IP limiter above
	// bounds REQUESTS; this bounds BYTES, which is the number a pricing page
	// can promise and a scraper behind many IPs can otherwise ignore.
	org := s.orgOf(sh.Project)
	if err := s.quota().CheckRead(org, fi.Size); err != nil {
		// Not "forbidden": nothing is wrong with the link or the reader, the
		// owner is over their transfer allowance. Say so plainly — whoever
		// opened this has no relationship with us and no way to fix it.
		http.Error(w, "This link has exceeded its transfer limit for now. "+
			"Ask whoever shared it to get in touch with us.", http.StatusTooManyRequests)
		return
	}
	// Count what actually leaves, on every branch below, including a transfer
	// the reader abandons halfway.
	cw := &countingWriter{w: w}
	defer func() { s.quota().RecordEgress(org, cw.n) }()

	rc, err := v.source.Open(r.Context(), sp, fi)
	if err != nil {
		http.Error(w, "content temporarily unavailable", http.StatusBadGateway)
		return
	}
	defer rc.Close()

	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", contentType(sp))
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeFilename(path.Base(sp))))
		io.Copy(cw, rc)
		return
	}

	switch strings.ToLower(path.Ext(sp)) {
	case ".md", ".markdown":
		src, err := io.ReadAll(rc)
		if err != nil {
			http.Error(w, "content temporarily unavailable", http.StatusBadGateway)
			return
		}
		body, err := RenderMarkdown(src)
		if err != nil {
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(cw, sharedMarkdownShell, html.EscapeString(path.Base(sp)), mermaidTag(body), updatedStamp(fi.Time), body)
	case ".html", ".htm":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.Copy(cw, rc)
	default:
		w.Header().Set("Content-Type", contentType(sp))
		setContentLength(w, rc) // measured, never the journal's Size field
		io.Copy(cw, rc)
	}
}

// updatedStamp renders the "how old is this?" line a share page owes its
// reader — the link promises the latest version, so it has to say when latest
// was. Zero time (a source that doesn't know) prints nothing rather than 1970.
func updatedStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.UTC()
	return fmt.Sprintf(`<div class="updated" title="%s">Last updated %s</div>`,
		html.EscapeString(t.Format(time.RFC3339)), html.EscapeString(t.Format("2 Jan 2006")))
}

// mermaidTag is the share page's only script, and only when the document
// actually has a diagram in it — a share page without a mermaid fence must
// stay the byte-for-byte zero-JavaScript document it has always been.
//
// The tag is a module: under this page's sandbox CSP the origin is opaque, so
// the asset responses carry Access-Control-Allow-Origin (server.go) and
// mermaid keeps its code splitting instead of arriving as one file. The name
// is fixed and lives outside assets/ because this template cannot know Vite's
// content hash.
func mermaidTag(body string) string {
	if !strings.Contains(body, `class="language-mermaid"`) {
		return ""
	}
	return `<script type="module" src="/share-mermaid.js"></script>`
}

// sharedMarkdownShell wraps rendered markdown in a minimal readable page.
// Verbs, in order: title, mermaid script tag (usually empty), updated stamp,
// body.
const sharedMarkdownShell = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>%s</title>
<style>
body{font:16px/1.7 -apple-system,BlinkMacSystemFont,"SF Pro Text","Inter","Segoe UI",sans-serif;color:#24292f;
max-width:720px;margin:0 auto;padding:52px 24px 96px}
a{color:#b26a00}
h1,h2,h3{line-height:1.25;letter-spacing:-.018em}
pre{padding:12px;border-radius:8px;overflow-x:auto;background:#f6f8fa}
code{background:#f6f8fa;padding:2px 5px;border-radius:4px;font-size:.9em}
pre code{padding:0;background:none}
img{max-width:100%%}
blockquote{margin:0;padding-left:16px;border-left:3px solid #d0d7de;color:#57606a}
table{border-collapse:collapse;display:block;overflow-x:auto;max-width:100%%}td,th{border:1px solid #d0d7de;padding:5px 10px}
table.frontmatter{display:table;font-size:12px;color:#57606a;background:#f6f8fa;border-radius:8px;margin-bottom:24px}
table.frontmatter th,table.frontmatter td{border:none;border-bottom:1px solid #d8dee4;text-align:left;vertical-align:top}
table.frontmatter th{white-space:nowrap;color:#6e7781}
table.frontmatter tr:last-child th,table.frontmatter tr:last-child td{border-bottom:none}
table.frontmatter code{white-space:pre-wrap}
pre{max-width:100%%}
footer.bdrive{margin-top:64px;padding-top:14px;border-top:1px solid #d0d7de;font-size:12.5px;color:#57606a}
footer.bdrive a{color:inherit}
.updated{font-size:12.5px;color:#57606a;margin-bottom:28px}
.mermaid-diagram{margin:20px 0;overflow-x:auto}
.mermaid-diagram svg{max-width:100%%;height:auto}
.mermaid-err{font-size:12.5px;color:#57606a;margin:-8px 0 4px}
.mermaid-err-detail{font:11.5px/1.5 ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,monospace;color:#57606a;
margin:0 0 20px;white-space:pre;overflow:auto;max-height:12em}
/* Dark theme LAST: these rules sit at the same specificity as the light ones
   above, so source order is the whole fix — a dark block placed earlier loses
   to every light rule that follows it. Values are the hub's @theme tokens
   (frontend/src/tw.css), never hand-picked, so the two surfaces agree.
   Inline code carries its own tint and edge so a chip reads as code and not
   as prose; the edge is an inset shadow rather than a border because a border
   would change the chip's box metrics and light mode has to stay untouched. */
@media (prefers-color-scheme: dark){
body{background:#0a0b0d;color:#eef0f3}
a{color:#ffcf85}
h1,h2,h3{color:#eef0f3}
pre,code{background:#15171b}
code{color:#e4d9c4;box-shadow:inset 0 0 0 1px rgba(255,255,255,.07)}
pre code{color:inherit;box-shadow:none}
blockquote{border-left-color:rgba(255,255,255,.07);color:#9aa0a9}
td,th{border-color:rgba(255,255,255,.07)}
table.frontmatter{background:#15171b;color:#9aa0a9}
table.frontmatter th,table.frontmatter td{border-bottom-color:rgba(255,255,255,.07)}
table.frontmatter th{color:#868b93}
footer.bdrive{border-top-color:rgba(255,255,255,.07);color:#868b93}
.updated{color:#868b93}
.mermaid-err,.mermaid-err-detail{color:#868b93}}
</style>%s</head><body>%s%s
<footer class="bdrive">Shared with <a href="https://github.com/runbear-io/beardrive" rel="noopener">BearDrive</a> — synced files for AI agent teams</footer>
</body></html>`
