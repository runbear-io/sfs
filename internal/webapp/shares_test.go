package webapp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/secrets"
)

func httptestNewRequestBody(method, url string, data []byte) *http.Request {
	return httptest.NewRequest(method, url, bytes.NewReader(data))
}

func doHTTP(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// shareHub returns an auth-enabled hub with sharing on and one synced file.
func shareHub(t *testing.T) (*Server, Project, string, *fakeRemote, http.Handler) {
	t.Helper()
	srv, p, root := newHub(t, true, nil)
	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Auth = auth
	sharesPath := filepath.Join(t.TempDir(), "shares.json")
	srv.Shares, err = OpenShareDB(sharesPath)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.put("dev1", "wiki/report.html", "<h1>Q3</h1><script>alert(1)</script>")
	f.put("dev1", "wiki/notes.md", "# Notes\n\nhello **team**")
	return srv, p, sharesPath, f, srv.Handler()
}

// authedShare creates a share as a signed-in user and returns its token+url.
func authedShare(t *testing.T, srv *Server, h http.Handler, project, path string) (string, string) {
	t.Helper()
	auth := srv.Auth.(*BuiltinAuth)
	u, err := auth.signup("s@x.io", "Sharer", "password1")
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatal(err)
	}
	var uid string
	if u != nil {
		uid = u.ID
	} else {
		auth.mu.Lock()
		uid = auth.findByEmail("s@x.io").ID
		auth.mu.Unlock()
	}
	tok, err := auth.issueToken(uid, "test")
	if err != nil {
		t.Fatal(err)
	}
	req := jsonReq(t, "POST", "/api/p/"+project+"/shares", map[string]string{"path": path})
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := doHTTP(h, req)
	if rec.Code != 200 {
		t.Fatalf("create share: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Token string `json:"token"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Token, out.URL
}

func TestShareLinks(t *testing.T) {
	srv, p, _, f, h := shareHub(t)

	// creating a share requires sign-in
	if rec := do(t, h, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "wiki/report.html"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated share create: %d, want 401", rec.Code)
	}

	token, url := authedShare(t, srv, h, p.ID, "wiki/report.html")
	if !strings.Contains(url, "/s/"+token) {
		t.Fatalf("url = %q", url)
	}

	// the public link needs NO auth and renders the HTML, sandboxed
	rec := do(t, h, "GET", "/s/"+token, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<h1>Q3</h1>") {
		t.Fatalf("public fetch: %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want html (rendered)", ct)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") {
		t.Fatalf("shared HTML must be sandboxed, CSP = %q", csp)
	}

	// sharing the same file again returns the SAME link
	token2, _ := authedShare(t, srv, h, p.ID, "wiki/report.html")
	if token2 != token {
		t.Fatalf("re-share minted a new token: %s vs %s", token2, token)
	}

	// the link serves the LATEST content after the file changes
	f.put("dev1", "wiki/report.html", "<h1>Q4 update</h1>")
	rec = do(t, h, "GET", "/s/"+token, nil)
	if !strings.Contains(rec.Body.String(), "Q4 update") {
		t.Fatalf("share must serve latest content, got %s", rec.Body)
	}

	// unknown tokens and unsynced paths
	if rec := do(t, h, "GET", "/s/ffffffffffffffffffffffffffffffff", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token: %d, want 404", rec.Code)
	}
	req := jsonReq(t, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "never-synced.md"})
	authAs(t, srv, req)
	if rec := doHTTP(h, req); rec.Code != http.StatusNotFound {
		t.Fatalf("share of unsynced file: %d, want 404", rec.Code)
	}

	// revoke kills the link
	req = jsonReq(t, "DELETE", "/api/shares/"+token, nil)
	authAs(t, srv, req)
	if rec := doHTTP(h, req); rec.Code != 200 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/s/"+token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("revoked link: %d, want 404", rec.Code)
	}
}

// PATCH /api/shares/{token} re-dates a link the user already copied: same
// token, same URL, only the lifetime moves.
func TestShareSetExpiry(t *testing.T) {
	srv, p, sharesPath, _, h := shareHub(t)
	token, url := authedShare(t, srv, h, p.ID, "wiki/notes.md")

	patch := func(t *testing.T, tok string, body any) *httptest.ResponseRecorder {
		t.Helper()
		req := jsonReq(t, "PATCH", "/api/shares/"+tok, body)
		authAs(t, srv, req)
		return doHTTP(h, req)
	}
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body, err)
		}
		return out
	}

	// setting an expiry keeps the token and URL the caller already has
	rec := patch(t, token, map[string]string{"expires_in": "24h"})
	if rec.Code != 200 {
		t.Fatalf("set expiry: %d %s", rec.Code, rec.Body)
	}
	out := decode(t, rec)
	if out["token"] != token || out["url"] != url {
		t.Fatalf("expiry changed the link: %v", out)
	}
	exp, _ := out["expires"].(string)
	when, err := time.Parse(time.RFC3339Nano, exp)
	if err != nil {
		t.Fatalf("expires = %q: %v", exp, err)
	}
	if d := time.Until(when); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("expires in %v, want ~24h", d)
	}
	// and it survives a registry reload
	db2, err := OpenShareDB(sharesPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := db2.Get(token); !ok || got.Expires.IsZero() {
		t.Fatalf("expiry lost across reload: %+v", got)
	}

	// "" clears it back to permanent
	rec = patch(t, token, map[string]string{"expires_in": ""})
	if rec.Code != 200 {
		t.Fatalf("clear expiry: %d %s", rec.Code, rec.Body)
	}
	if got, _ := srv.Shares.Get(token); !got.Expires.IsZero() {
		t.Fatalf("cleared share still expires at %v", got.Expires)
	}
	if out := decode(t, rec); out["expires"] != nil {
		t.Fatalf("clear left an expiry: %v", out)
	}

	// junk durations are refused
	for _, bad := range []string{"nonsense", "0s", "-1h"} {
		if rec := patch(t, token, map[string]string{"expires_in": bad}); rec.Code != http.StatusBadRequest {
			t.Errorf("expires_in %q: %d, want 400", bad, rec.Code)
		}
	}
	// unknown token
	if rec := patch(t, strings.Repeat("f", 32), map[string]string{"expires_in": "24h"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown token: %d, want 404", rec.Code)
	}
	// (a read-only member gets 403: TestReadOnlyMemberRoutes, which has the
	// directory this harness deliberately does without.)

	// a PATCH-set expiry kills the link on time, and drops it from List
	if rec := patch(t, token, map[string]string{"expires_in": "1ms"}); rec.Code != 200 {
		t.Fatalf("short expiry: %d %s", rec.Code, rec.Body)
	}
	time.Sleep(5 * time.Millisecond)
	if rec := do(t, h, "GET", "/s/"+token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("expired link: %d, want 404", rec.Code)
	}
	req := jsonReq(t, "GET", "/api/p/"+p.ID+"/shares", nil)
	authAs(t, srv, req)
	if rec := doHTTP(h, req); strings.Contains(rec.Body.String(), token) {
		t.Fatalf("expired share still listed: %s", rec.Body)
	}
	// an already-expired token is dead, not adjustable
	if rec := patch(t, token, map[string]string{"expires_in": ""}); rec.Code != http.StatusNotFound {
		t.Errorf("patch of expired share: %d, want 404", rec.Code)
	}
}

func TestShareMarkdownRendersAndExpires(t *testing.T) {
	srv, p, sharesPath, _, h := shareHub(t)

	// markdown renders as a full page
	token, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")
	rec := do(t, h, "GET", "/s/"+token, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<strong>team</strong>") ||
		!strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Fatalf("markdown share: %d %s", rec.Code, rec.Body)
	}
	// download variant attaches raw
	rec = do(t, h, "GET", "/s/"+token+"?download=1", nil)
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "notes.md") ||
		!strings.Contains(rec.Body.String(), "**team**") {
		t.Fatalf("download variant: %v %s", rec.Header(), rec.Body)
	}

	// expiring shares die on time
	req := jsonReq(t, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "wiki/notes.md", "expires_in": "1ms"})
	authAs(t, srv, req)
	recC := doHTTP(h, req)
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(recC.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("expiring create: %d %s", recC.Code, recC.Body)
	}
	time.Sleep(5 * time.Millisecond)
	if rec := do(t, h, "GET", "/s/"+out.Token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("expired link: %d, want 404", rec.Code)
	}

	// list shows live links only, and persists across a registry reload
	req = jsonReq(t, "GET", "/api/p/"+p.ID+"/shares", nil)
	authAs(t, srv, req)
	rec = doHTTP(h, req)
	if !strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), out.Token) {
		t.Fatalf("list = %s", rec.Body)
	}
	db2, err := OpenShareDB(sharesPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := db2.Get(token); !ok {
		t.Fatal("share lost across reload")
	}
}

// A Revoke button sits on every row of the public-links table, so the row
// order must not move between loads. byToken is a map, so List has to sort.
func TestShareListIsDeterministic(t *testing.T) {
	db, err := OpenShareDB(filepath.Join(t.TempDir(), "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Hand-set Created — Create stamps time.Now(), which never collides, so
	// going through it would leave the tie-break path untested.
	tick := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	for _, s := range []Share{
		{Token: "t3", Project: "p", Path: "z.md", Created: tick},
		{Token: "t2", Project: "p", Path: "a.md", Created: tick}, // same instant as t3
		{Token: "t1", Project: "p", Path: "newest.md", Created: tick.Add(time.Hour)},
		{Token: "t0", Project: "p", Path: "oldest.md", Created: tick.Add(-time.Hour)},
		{Token: "x", Project: "other", Path: "a.md", Created: tick},
		{Token: "dead", Project: "p", Path: "gone.md", Created: tick, Expires: tick},
	} {
		db.byToken[s.Token] = s
		if err := db.repo.Put(s); err != nil {
			t.Fatal(err)
		}
	}

	first := db.List("p")
	want := []string{"t1", "t2", "t3", "t0"} // created desc, then path asc (a.md < z.md)
	got := make([]string, len(first))
	for i, s := range first {
		got[i] = s.Token
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v (newest first, path tie-break)", got, want)
	}
	for i := 0; i < 10; i++ {
		if !reflect.DeepEqual(db.List("p"), first) {
			t.Fatalf("call %d reordered: %v vs %v", i, db.List("p"), first)
		}
	}
	// A project with no shares still returns nil, not an empty slice.
	if db.List("nobody") != nil {
		t.Fatal("empty project should list as nil")
	}
}

// Both share-listing APIs — the project settings table and the org-wide audit
// — must hand back the same bytes on consecutive reads.
func TestShareListAPIsAreStable(t *testing.T) {
	h, srv, c, p := permHub(t)
	// Mint directly: the HTTP route requires a synced file, and this test is
	// about ordering, not about the mint path.
	for _, path := range []string{"b.md", "a.md", "c.md"} {
		if _, err := srv.Shares.Create(p.ID, path, "alice@x.io", 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, url := range []string{"/api/p/" + p.ID + "/shares", "/api/orgs/" + p.Org + "/shares"} {
		first := doAs(t, h, "GET", url, nil, c["alice"])
		if first.Code != 200 {
			t.Fatalf("GET %s: %d %s", url, first.Code, first.Body)
		}
		if !strings.Contains(first.Body.String(), "a.md") {
			t.Fatalf("GET %s listed no shares: %s", url, first.Body)
		}
		for i := 0; i < 5; i++ {
			again := doAs(t, h, "GET", url, nil, c["alice"])
			if again.Body.String() != first.Body.String() {
				t.Fatalf("GET %s moved between loads:\n%s\n%s", url, first.Body, again.Body)
			}
		}
	}
	// The newest link is the first row — what you just minted is what you are
	// most likely to want to undo.
	rec := doAs(t, h, "GET", "/api/p/"+p.ID+"/shares", nil, c["alice"])
	var out struct {
		Shares []struct {
			Path string `json:"path"`
		} `json:"shares"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Shares) != 3 || out.Shares[0].Path != "c.md" {
		t.Fatalf("newest share is not first: %s", rec.Body)
	}
}

// helpers

// TestShareLastUpdatedStamp: a share page promises the latest version, so a
// markdown page says when latest was — and nothing else gets injected into.
func TestShareLastUpdatedStamp(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	const rawHTML = "<h1>Q3</h1><script>alert(1)</script>"
	const pdf = "%PDF-1.4 fake\n"
	f.put("dev1", "wiki/deck.pdf", pdf)

	// markdown: the stamp is the FILE's time, human date + precise title=
	when := time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)
	f.putAt("dev1", "wiki/notes.md", "# Notes\n\nhello **team**", when)
	token, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")
	rec := do(t, h, "GET", "/s/"+token, nil)
	body := rec.Body.String()
	if !strings.Contains(body, "Last updated 14 Mar 2026") {
		t.Fatalf("no last-updated stamp: %s", body)
	}
	if !strings.Contains(body, `title="2026-03-14T09:26:53Z"`) {
		t.Fatalf("no precise timestamp: %s", body)
	}
	// ...and it sits above the content, not after it
	if strings.Index(body, "Last updated") > strings.Index(body, "<strong>team</strong>") {
		t.Fatal("stamp must render before the content")
	}
	// the sandbox + footer survive
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox allow-scripts allow-popups" {
		t.Fatalf("CSP = %q", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("share headers = %v", rec.Header())
	}
	if !strings.Contains(body, "Shared with <a") {
		t.Fatal("footer went missing")
	}

	// re-syncing the file moves the stamp; the token does not change
	f.putAt("dev1", "wiki/notes.md", "# Notes\n\nhello **team**", when.AddDate(0, 0, 40))
	rec = do(t, h, "GET", "/s/"+token, nil)
	if !strings.Contains(rec.Body.String(), "Last updated 23 Apr 2026") {
		t.Fatalf("stamp must track the file, got %s", rec.Body)
	}

	// zero time: no stamp at all, not a 1970 date
	f.putAt("dev1", "wiki/notes.md", "# Notes\n\nhello **team**", time.Time{})
	rec = do(t, h, "GET", "/s/"+token, nil)
	if strings.Contains(rec.Body.String(), "Last updated") || strings.Contains(rec.Body.String(), `class="updated"`) {
		t.Fatalf("zero time must print no stamp, got %s", rec.Body)
	}

	// HTML shares are served byte-for-byte — never injected into
	htmlTok, _ := authedShare(t, srv, h, p.ID, "wiki/report.html")
	rec = do(t, h, "GET", "/s/"+htmlTok, nil)
	if !bytes.Equal(rec.Body.Bytes(), []byte(rawHTML)) {
		t.Fatalf("html share must be byte-identical, got %q", rec.Body)
	}

	// so are binaries, Content-Length included
	pdfTok, _ := authedShare(t, srv, h, p.ID, "wiki/deck.pdf")
	rec = do(t, h, "GET", "/s/"+pdfTok, nil)
	if !bytes.Equal(rec.Body.Bytes(), []byte(pdf)) {
		t.Fatalf("binary share must be unchanged, got %q", rec.Body)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(len(pdf)) {
		t.Fatalf("Content-Length = %q, want %d", cl, len(pdf))
	}
}

// The share page keeps the frontmatter TABLE at the top of the document.
// The viewer moved its frontmatter into a side panel (BEA-154) by calling
// RenderMarkdownPairs; shares.go stayed on RenderMarkdown deliberately, and
// nothing else would fail if someone later "tidied" it onto the pairs path —
// which would silently change every public link ever minted.
func TestShareKeepsFrontmatterTable(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	f.put("dev1", "wiki/props.md", "---\ntitle: Q3\nowner: snow\n---\n\n# Q3\n")
	token, _ := authedShare(t, srv, h, p.ID, "wiki/props.md")
	body := do(t, h, "GET", "/s/"+token, nil).Body.String()
	for _, want := range []string{
		`<table class="frontmatter"><tbody>`,
		`<th scope="row">title</th><td>Q3</td>`,
		`<th scope="row">owner</th><td>snow</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("share page lost %q:\n%s", want, body)
		}
	}
	// ...and above the document, where it has always been.
	if strings.Index(body, `class="frontmatter"`) > strings.Index(body, "<h1") {
		t.Error("frontmatter table no longer precedes the body")
	}
}

// TestShareDarkThemeIsLast: the share page is the surface strangers see first,
// so in dark mode it must not show white slabs. Every dark rule sits at the
// same specificity as the light one it overrides, which makes SOURCE ORDER the
// whole feature — a dark block placed before the light rules (as it was) loses
// silently and the page still renders light. Assert placement, not presence.
func TestShareDarkThemeIsLast(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	f.put("dev1", "wiki/themed.md", "---\ntitle: Q3\n---\n\n# Q3\n\n> quote\n\n| a | b |\n| - | - |\n| 1 | 2 |\n\n```go\nx := 1\n```\n")
	token, _ := authedShare(t, srv, h, p.ID, "wiki/themed.md")
	body := do(t, h, "GET", "/s/"+token, nil).Body.String()

	if strings.Contains(body, "%!") {
		t.Fatalf("format verb leaked into the page (a %% needs doubling in the const): %s", body)
	}
	dark := strings.LastIndex(body, "prefers-color-scheme")
	if dark < 0 {
		t.Fatal("no dark block at all")
	}
	// Every light surface colour must be settled before the dark block opens.
	for _, light := range []string{"#f6f8fa", "#d0d7de", "#d8dee4", "#6e7781", "#57606a"} {
		if i := strings.LastIndex(body, light); i > dark {
			t.Errorf("light literal %s at %d comes after the dark block at %d — it wins in dark mode", light, i, dark)
		}
	}
	// ...and the dark block has to actually cover the surfaces that were light.
	block := body[dark:]
	for _, sel := range []string{"pre,code{background:#15171b}", "blockquote{", "td,th{", "table.frontmatter{", "footer.bdrive{"} {
		if !strings.Contains(block, sel) {
			t.Errorf("dark block has no rule for %q", sel)
		}
	}
	// Hub tokens, not a hand-picked palette (tw.css: --color-text, --color-bg).
	if !strings.Contains(block, "background:#0a0b0d;color:#eef0f3") {
		t.Error("dark body must use the hub's bg/text tokens")
	}
	if strings.Contains(body, "#c6cbd3") || strings.Contains(body, "#3a3a44") {
		t.Error("ad-hoc dark greys survived; use the tw.css tokens")
	}
	// A code chip has to read as code: its own colour, not the body's, plus an
	// edge to give it shape against a background only a shade off the page.
	if !strings.Contains(block, "code{color:#e4d9c4;box-shadow:inset 0 0 0 1px") {
		t.Error("dark inline code must have its own colour and edge, else a chip reads as prose")
	}
	// ...and a fenced block stays one slab rather than a row of bordered chips.
	if !strings.Contains(block, "pre code{color:inherit;box-shadow:none}") {
		t.Error("dark pre code must reset the inline chip's colour and edge")
	}
}

// A share page is a zero-JavaScript document, and it stays one unless the
// document it renders actually has a diagram in it. The tag is the whole cost
// of the feature for every other share page on the hub, so it is worth a test
// that it isn't paid — and that the CSP sandbox that makes the tag work at all
// is unchanged.
func TestShareMermaidScriptOnlyWhenNeeded(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	f.put("dev1", "wiki/diagram.md", "# D\n\n```mermaid\ngraph TD\n  A --> B\n```\n")

	tag := `<script type="module" src="/share-mermaid.js"></script>`

	token, _ := authedShare(t, srv, h, p.ID, "wiki/diagram.md")
	rec := do(t, h, "GET", "/s/"+token, nil)
	body := rec.Body.String()
	if !strings.Contains(body, tag) {
		t.Errorf("a document with a mermaid fence must load the script: %s", body)
	}
	if strings.Contains(body, "%!") {
		t.Errorf("format verb leaked into the page: %s", body)
	}
	// The tag only works because the page is sandboxed with scripts allowed
	// and its origin is opaque; adding allow-same-origin to make loading
	// easier would hand shared content the hub's origin.
	if csp := rec.Header().Get("Content-Security-Policy"); csp != "sandbox allow-scripts allow-popups" {
		t.Errorf("share CSP = %q, want the unchanged sandbox", csp)
	}

	// wiki/notes.md has no fence: not one byte of mermaid.
	plain, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")
	if b := do(t, h, "GET", "/s/"+plain, nil).Body.String(); strings.Contains(b, "share-mermaid") ||
		strings.Contains(b, "<script") {
		t.Errorf("a share page without a diagram must ship no script: %s", b)
	}
}

// listShares reads the project's share list as the signed-in sharer.
func listShares(t *testing.T, srv *Server, h http.Handler, project string) []map[string]any {
	t.Helper()
	req := jsonReq(t, "GET", "/api/p/"+project+"/shares", nil)
	authAs(t, srv, req)
	rec := doHTTP(h, req)
	if rec.Code != 200 {
		t.Fatalf("list shares: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Shares []map[string]any `json:"shares"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Shares
}

// TestShareOpensOnTheWire: the number was always recorded and always thrown
// away at the UI layer (BEA-76). It now rides the shares list — as a count,
// never as an opener.
func TestShareOpensOnTheWire(t *testing.T) {
	srv, p, _, _, h := shareHub(t)
	var err error
	if srv.Reads, err = OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0); err != nil {
		t.Fatal(err)
	}
	token, _ := authedShare(t, srv, h, p.ID, "wiki/report.html")
	unopened, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")

	// A freshly minted link reports zero — not absent. "Minted, never
	// opened" is a real answer and the UI words it as "not opened yet".
	byToken := map[string]map[string]any{}
	for _, s := range listShares(t, srv, h, p.ID) {
		byToken[s["token"].(string)] = s
	}
	if got, ok := byToken[unopened]["opens"]; !ok || got.(float64) != 0 {
		t.Fatalf("unopened link opens = %v (present %v), want 0", got, ok)
	}
	if _, ok := byToken[unopened]["last_opened"]; ok {
		t.Fatal("a never-opened link must carry no last_opened")
	}

	// Two hits from one client inside the debounce window are one visit.
	for i := 0; i < 2; i++ {
		if rec := do(t, h, "GET", "/s/"+token, nil); rec.Code != 200 {
			t.Fatalf("public fetch %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	byToken = map[string]map[string]any{}
	for _, s := range listShares(t, srv, h, p.ID) {
		byToken[s["token"].(string)] = s
	}
	if got := byToken[token]["opens"].(float64); got != 1 {
		t.Fatalf("opens = %v after two hits in the debounce window, want 1", got)
	}
	if _, ok := byToken[token]["last_opened"]; !ok {
		t.Fatal("an opened link must carry last_opened")
	}

	// opens reads one link's count and last_opened back off the WIRE rather
	// than out of the ledger, because the wire is what the panel shows.
	opens := func(tok string) (float64, time.Time) {
		t.Helper()
		for _, sh := range listShares(t, srv, h, p.ID) {
			if sh["token"] != tok {
				continue
			}
			last, _ := sh["last_opened"].(string)
			if last == "" {
				return sh["opens"].(float64), time.Time{}
			}
			ts, err := time.Parse(time.RFC3339, last)
			if err != nil {
				t.Fatalf("last_opened %q: %v", last, err)
			}
			return sh["opens"].(float64), ts
		}
		t.Fatalf("link %s missing from the list", tok)
		return 0, time.Time{}
	}
	// The debounce collapses reloads; it does not cap a link. Only the clock
	// is under test, so it is moved rather than waited on.
	ageDebounce := func() {
		srv.Reads.mu.Lock()
		defer srv.Reads.mu.Unlock()
		for k, v := range srv.Reads.seen {
			srv.Reads.seen[k] = v.Add(-readDebounce - time.Minute)
		}
	}
	ageDebounce()
	if rec := do(t, h, "GET", "/s/"+token, nil); rec.Code != 200 {
		t.Fatalf("public fetch past the window: %d %s", rec.Code, rec.Body)
	}
	if got, _ := opens(token); got != 2 {
		t.Fatalf("opens = %v after a reload past the debounce window, want 2", got)
	}

	// Three readers, ONE network, seconds apart: distinct browsers are
	// distinct actors, so the count moves and last_opened follows the newest
	// hit. token+"/"+IP alone made a whole office one reader (BEA-151) —
	// httptest hands every request the same 192.0.2.1, which is exactly the
	// NAT the personas were sitting behind.
	uas := []string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) Safari/605.1",
		"Mozilla/5.0 (X11; Linux x86_64) Firefox/128.0",
		"Mozilla/5.0 (Windows NT 10.0; Win64) Chrome/126.0.0.0",
	}
	var afterFirst time.Time
	for i, ua := range uas {
		req := jsonReq(t, "GET", "/s/"+unopened, nil)
		req.Header.Set("User-Agent", ua)
		if rec := doHTTP(h, req); rec.Code != 200 {
			t.Fatalf("reader %d: %d %s", i, rec.Code, rec.Body)
		}
		if i == 0 {
			_, afterFirst = opens(unopened)
		}
	}
	got, afterThird := opens(unopened)
	if got != 3 {
		t.Fatalf("three readers on one network reported %v opens, want 3 — distinct browsers are distinct readers", got)
	}
	if !afterThird.After(afterFirst) {
		t.Fatalf("last_opened stuck at %s after two later readers; it must follow the newest open", afterFirst)
	}

	// The actor is token+"/"+IP+"/"+UA hash — a public credential joined to a
	// network and a browser. No part of it may appear anywhere in the
	// response, in any shape; a hash is not an exemption.
	req := jsonReq(t, "GET", "/api/p/"+p.ID+"/shares", nil)
	authAs(t, srv, req)
	body := doHTTP(h, req).Body.String()
	leaks := []string{token + "/", unopened + "/", "192.0.2.1", "actor", "openers"}
	for _, ua := range append(uas, "") {
		sum := sha256.Sum256([]byte(ua))
		leaks = append(leaks, hex.EncodeToString(sum[:8]))
	}
	for _, leak := range leaks {
		if strings.Contains(body, leak) {
			t.Fatalf("shares response leaks %q: %s", leak, body)
		}
	}

	// Two tokens on one path report the SAME count: heat is keyed by path,
	// not by token. Documented behavior — asserted so it cannot regress into
	// a silently wrong per-link number.
	second := Share{Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Project: p.ID,
		Path: "wiki/report.html", Creator: "s@x.io", Created: time.Now().UTC()}
	srv.Shares.mu.Lock()
	srv.Shares.byToken[second.Token] = second
	srv.Shares.mu.Unlock()
	if err := srv.Shares.repo.Put(second); err != nil {
		t.Fatal(err)
	}
	byToken = map[string]map[string]any{}
	for _, s := range listShares(t, srv, h, p.ID) {
		byToken[s["token"].(string)] = s
	}
	if a, b := byToken[token]["opens"], byToken[second.Token]["opens"]; a != b {
		t.Fatalf("two links on one path report %v and %v; heat is keyed by path, so they must match", a, b)
	}

	// One byKey scan per project per render — never one per share. Three
	// links are listed below; a per-share implementation would scan 3×.
	before := srv.Reads.scans.Load()
	listShares(t, srv, h, p.ID)
	if got := srv.Reads.scans.Load() - before; got != 1 {
		t.Fatalf("listing 3 shares performed %d ShareOpens scans, want exactly 1", got)
	}

	// Reads off: neither key, and no panic on the nil ledger. Absent means
	// "not measured"; a 0 here would claim nobody has opened the link.
	srv.Reads = nil
	for _, s := range listShares(t, srv, h, p.ID) {
		if _, ok := s["opens"]; ok {
			t.Fatalf("reads disabled must omit opens entirely: %v", s)
		}
		if _, ok := s["last_opened"]; ok {
			t.Fatalf("reads disabled must omit last_opened entirely: %v", s)
		}
	}
}

// The org-wide audit table is the second caller of shareJSON, and the place
// the one-scan-per-project rule is easiest to break (it loops projects).
func TestOrgSharesCarryOpens(t *testing.T) {
	h, srv, c, p := permHub(t)
	var err error
	if srv.Reads, err = OpenReadLedger(filepath.Join(t.TempDir(), "reads.json"), 0); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"a.md", "b.md", "c.md"} {
		if _, err := srv.Shares.Create(p.ID, path, "alice@x.io", 0); err != nil {
			t.Fatal(err)
		}
	}
	srv.Reads.Record(p.ID, "a.md", ReadKindShare, "tok/203.0.113.7")
	srv.Reads.Record(p.ID, "b.md", ReadKindHuman, "alice@x.io") // not an open

	before := srv.Reads.scans.Load()
	rec := doAs(t, h, "GET", "/api/orgs/"+p.Org+"/shares", nil, c["alice"])
	if rec.Code != 200 {
		t.Fatalf("org shares: %d %s", rec.Code, rec.Body)
	}
	if got := srv.Reads.scans.Load() - before; got != 1 {
		t.Fatalf("org audit over 1 project with 3 shares scanned %d times, want 1", got)
	}
	var out struct {
		Shares []struct {
			Path  string `json:"path"`
			Opens *int64 `json:"opens"`
		} `json:"shares"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Shares) != 3 {
		t.Fatalf("want 3 org share rows, got %d", len(out.Shares))
	}
	for _, s := range out.Shares {
		if s.Opens == nil {
			t.Fatalf("%s carries no opens on a reads-enabled hub", s.Path)
		}
		want := int64(0)
		if s.Path == "a.md" {
			want = 1
		}
		if *s.Opens != want {
			t.Fatalf("%s opens = %d, want %d (a human read is not an open)", s.Path, *s.Opens, want)
		}
	}
}

func jsonReq(t *testing.T, method, url string, body any) *http.Request {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptestNewRequestBody(method, url, data)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func authAs(t *testing.T, srv *Server, req *http.Request) {
	t.Helper()
	auth := srv.Auth.(*BuiltinAuth)
	auth.mu.Lock()
	u := auth.findByEmail("s@x.io")
	auth.mu.Unlock()
	if u == nil {
		var err error
		u, err = auth.signup("s@x.io", "Sharer", "password1")
		if err != nil {
			t.Fatal(err)
		}
	}
	tok, err := auth.issueToken(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
}

// planted is a fabricated AWS-shaped string. No real credential is involved,
// here or in the fixtures below.
const planted = "AKIAIOSFODNN7EXAMPLE"

// postShare mints as a signed-in member and hands back the raw recorder, so a
// test can look at a non-200.
func postShare(t *testing.T, srv *Server, h http.Handler, project string, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := jsonReq(t, "POST", "/api/p/"+project+"/shares", body)
	authAs(t, srv, req)
	return doHTTP(h, req)
}

func decodeFindings(t *testing.T, rec *httptest.ResponseRecorder) []secrets.Finding {
	t.Helper()
	var out struct {
		Error    string            `json:"error"`
		Findings []secrets.Finding `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode 409 body %q: %v", rec.Body, err)
	}
	if out.Error == "" {
		t.Fatalf("409 body carries no error message: %s", rec.Body)
	}
	return out.Findings
}

// TestShareSecretScan is the gate: a credential-shaped file does not become a
// public URL by accident, and does become one on purpose.
func TestShareSecretScan(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	f.put("dev1", "deploy.md", "# Deploy\n\nrun it\n\nAWS_ACCESS_KEY_ID="+planted+"\n")

	// 1. blocked, and nothing minted
	rec := postShare(t, srv, h, p.ID, map[string]string{"path": "deploy.md"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("share of a file holding a key: %d %s, want 409", rec.Code, rec.Body)
	}
	if got := decodeFindings(t, rec); !reflect.DeepEqual(got, []secrets.Finding{{Rule: "aws_access_key_id", Line: 5}}) {
		t.Fatalf("findings = %v, want aws_access_key_id on line 5", got)
	}
	if n := len(srv.Shares.List(p.ID)); n != 0 {
		t.Fatalf("409 minted %d shares, want 0", n)
	}

	// 2. the same request with confirm mints a working link
	rec = postShare(t, srv, h, p.ID, map[string]any{"path": "deploy.md", "confirm": true})
	if rec.Code != 200 {
		t.Fatalf("confirmed share: %d %s", rec.Code, rec.Body)
	}
	var out struct{ Token string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if pub := do(t, h, "GET", "/s/"+out.Token, nil); pub.Code != 200 || !strings.Contains(pub.Body.String(), "Deploy") {
		t.Fatalf("confirmed link does not serve: %d %s", pub.Code, pub.Body)
	}

	// 3. a path that is already public skips the scan — the content is out
	//    there, so a second Share click still hands back the same URL.
	rec = postShare(t, srv, h, p.ID, map[string]string{"path": "deploy.md"})
	if rec.Code != 200 {
		t.Fatalf("re-share of an already-public file: %d %s, want 200", rec.Code, rec.Body)
	}

	// 4. a clean file is unaffected
	if rec := postShare(t, srv, h, p.ID, map[string]string{"path": "wiki/notes.md"}); rec.Code != 200 {
		t.Fatalf("clean file: %d %s, want 200", rec.Code, rec.Body)
	}

	// 5. the boundary is a decision: a key past the first MiB mints silently.
	f.put("dev1", "big.md", strings.Repeat("filler line\n", 100_000)+planted+"\n")
	if rec := postShare(t, srv, h, p.ID, map[string]string{"path": "big.md"}); rec.Code != 200 {
		t.Fatalf("key past the 1 MiB limit: %d %s, want 200 (documented boundary)", rec.Code, rec.Body)
	}
}

// TestShareSecretNeverEchoed is the one rule that cannot bend: the matched
// bytes never leave secrets.Scan — not in the body, not in the log.
func TestShareSecretNeverEchoed(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	f.put("dev1", "creds.md", "key = "+planted+"\n")

	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	rec := postShare(t, srv, h, p.ID, map[string]string{"path": "creds.md"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("share: %d %s, want 409", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), planted) {
		t.Errorf("the 409 body echoed the secret: %s", rec.Body)
	}
	if strings.Contains(logs.String(), planted) {
		t.Errorf("the secret reached the log: %s", logs.String())
	}
}

// TestShareSecretScanFailsClosed: a blob the hub cannot read mints nothing.
// A check that skips itself on a storage hiccup is the false confidence this
// gate exists to remove.
func TestShareSecretScanFailsClosed(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	f.put("dev1", "gone.md", "harmless")
	// Drop the blob but keep the journal op: the file is "synced" and unreadable.
	blobs, err := os.ReadDir(filepath.Join(f.dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("harmless"))
	want := hex.EncodeToString(sum[:])
	for _, b := range blobs {
		if b.Name() == want {
			if err := os.Remove(filepath.Join(f.dir, "blobs", b.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	rec := postShare(t, srv, h, p.ID, map[string]string{"path": "gone.md"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreadable blob: %d %s, want 503", rec.Code, rec.Body)
	}
	if n := len(srv.Shares.List(p.ID)); n != 0 {
		t.Fatalf("failed scan minted %d shares, want 0", n)
	}
}

// A share token is a promise about ONE file. These three pin the direction
// it goes when a path changes hands — the opposite of the viewer's, which is
// an address and always serves whatever lives there now.

func TestShareFollowsMovedFile(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	token, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")

	at := time.Now().Add(time.Minute)
	f.putAt("dev1", "docs/notes.md", "# Notes\n\nhello **team**", at)
	f.delAt("dev1", "wiki/notes.md", at.Add(time.Second))

	rec := do(t, h, "GET", "/s/"+token, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("share after move: %d %s", rec.Code, rec.Body)
	}

	// ...and keeps following it once an UNRELATED file takes the old address.
	f.putAt("dev1", "wiki/notes.md", "# Someone else's file", at.Add(time.Hour))
	rec = do(t, h, "GET", "/s/"+token, nil)
	if rec.Code != 200 {
		t.Fatalf("share with a new file at the old path: %d %s", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); strings.Contains(body, "Someone else") {
		t.Fatalf("share served the file that took its old address:\n%s", body)
	}
}

func TestShareOfDeletedFileNeverServesItsSuccessor(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	token, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")

	at := time.Now().Add(time.Minute)
	f.delAt("dev1", "wiki/notes.md", at)
	if rec := do(t, h, "GET", "/s/"+token, nil); rec.Code != 404 {
		t.Fatalf("share of a deleted file: %d, want 404", rec.Code)
	}
	// The leak this closes: something new lands on the address later, and
	// the public link used to start serving it with no revoke and no signal.
	f.putAt("dev1", "wiki/notes.md", "# Payroll", at.Add(time.Hour))
	rec := do(t, h, "GET", "/s/"+token, nil)
	if rec.Code != 404 {
		t.Fatalf("share resurrected by an unrelated file: %d %s", rec.Code, rec.Body)
	}
}

func TestShareUnaffectedByAnUnmovedFile(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	_ = f
	token, _ := authedShare(t, srv, h, p.ID, "wiki/notes.md")
	if rec := do(t, h, "GET", "/s/"+token, nil); rec.Code != 200 {
		t.Fatalf("unmoved share: %d %s", rec.Code, rec.Body)
	}
}

// A folder is never a key in the files map, so a fully synced folder used to
// 404 as "not synced yet" and send the user off to fix a sync fault that
// wasn't there.
func TestShareFolderIsNotASyncProblem(t *testing.T) {
	srv, p, _, _, h := shareHub(t)

	// wiki/ is fully synced; sharing it is a per-file problem, not a timing one
	req := jsonReq(t, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "wiki"})
	authAs(t, srv, req)
	rec := doHTTP(h, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("share of a folder: %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "per-file") {
		t.Fatalf("folder error must say share links are per-file, got %q", body)
	}
	// names a file inside, and the SMALLEST one so repeat calls agree
	if !strings.Contains(body, "wiki/notes.md") {
		t.Fatalf("folder error must name a file inside it, got %q", body)
	}
	if strings.Contains(body, "not synced") {
		t.Fatalf("folder error must not blame sync, got %q", body)
	}
	if n := len(srv.Shares.List(p.ID)); n != 0 {
		t.Fatalf("folder share minted %d links, want 0", n)
	}

	// a path that really is unknown keeps today's answer
	req = jsonReq(t, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "never-synced.md"})
	authAs(t, srv, req)
	if rec := doHTTP(h, req); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not synced to this project yet") {
		t.Fatalf("unknown path: %d %s, want 404 not-synced", rec.Code, rec.Body)
	}

	// "wik" is a PREFIX of wiki/report.html but is not a folder: still unknown.
	// A bare HasPrefix(k, p) would call it a folder — the same wrong-diagnosis
	// class of bug this fix is about.
	req = jsonReq(t, "POST", "/api/p/"+p.ID+"/shares", map[string]string{"path": "wik"})
	authAs(t, srv, req)
	if rec := doHTTP(h, req); rec.Code != http.StatusNotFound {
		t.Fatalf("prefix-of-a-file path: %d, want 404", rec.Code)
	}
}
