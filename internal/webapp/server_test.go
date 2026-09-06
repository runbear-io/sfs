package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// fakeRemote builds a beardrive remote layout (journal/<dev>.jsonl + blobs/<sha>)
// in a temp dir and returns a Server over it.
type fakeRemote struct {
	t   *testing.T
	dir string
	seq map[string]int64
	lam int64
}

func newFakeRemote(t *testing.T) *fakeRemote {
	t.Helper()
	return newFakeRemoteAt(t, t.TempDir())
}

// newFakeRemoteAt builds the remote layout at a specific directory (e.g. a
// project prefix inside a hub's storage root).
func newFakeRemoteAt(t *testing.T, dir string) *fakeRemote {
	t.Helper()
	for _, d := range []string{"journal", "blobs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &fakeRemote{t: t, dir: dir, seq: map[string]int64{}}
}

func (f *fakeRemote) put(dev, path, content string) {
	f.t.Helper()
	sum := sha256.Sum256([]byte(content))
	blob := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(f.dir, "blobs", blob), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.append(dev, journal.Op{
		Kind: journal.KindPut, Path: path,
		Blob: blob, Size: int64(len(content)), Mode: 0o644,
	})
}

func (f *fakeRemote) del(dev, path string) {
	f.t.Helper()
	f.append(dev, journal.Op{Kind: journal.KindDelete, Path: path})
}

func (f *fakeRemote) append(dev string, op journal.Op) {
	f.t.Helper()
	f.lam++
	f.seq[dev]++
	op.Seq, op.Lamport = f.seq[dev], f.lam
	// A caller that set Time keeps it: move pairing and share resolution are
	// both time-windowed, so those tests have to place ops in time.
	if op.Time.IsZero() {
		op.Time = time.Now().UTC()
	}
	op.Device, op.DeviceName, op.Author = dev, dev, dev+"@test"
	if err := journal.Append(filepath.Join(f.dir, "journal", dev+".jsonl"), []journal.Op{op}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeRemote) server() *Server {
	f.t.Helper()
	be, err := remote.Open(context.Background(), "file://"+f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { be.Close() })
	return &Server{Source: &RemoteSource{Backend: be}, Volume: "testvol", Refresh: 0}
}

func get(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	return rec
}

func TestTreeAndFile(t *testing.T) {
	f := newFakeRemote(t)
	f.put("deva", "readme.md", "# Hello")
	f.put("deva", "notes/plan.md", "- step one")
	f.put("devb", "notes/img.png", "not-really-a-png")
	h := f.server().Handler()

	rec := get(t, h, "/api/tree")
	if rec.Code != 200 {
		t.Fatalf("tree: %d %s", rec.Code, rec.Body)
	}
	var root Node
	if err := json.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Children) != 2 {
		t.Fatalf("root children = %d, want 2 (notes/, readme.md)", len(root.Children))
	}
	if !root.Children[0].Dir || root.Children[0].Name != "notes" {
		t.Fatalf("first child = %+v, want dir notes (folders first)", root.Children[0])
	}
	if got := len(root.Children[0].Children); got != 2 {
		t.Fatalf("notes/ children = %d, want 2", got)
	}

	rec = get(t, h, "/api/file?path=readme.md")
	if rec.Code != 200 || rec.Body.String() != "# Hello" {
		t.Fatalf("file: %d %q", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("content-type = %q", ct)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	req := httptest.NewRequest("GET", "/api/file?path=readme.md", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("etag revalidate: %d, want 304", rec.Code)
	}

	if rec := get(t, h, "/api/file?path=nope.md"); rec.Code != 404 {
		t.Fatalf("missing file: %d, want 404", rec.Code)
	}
}

func TestDeleteHidesFile(t *testing.T) {
	f := newFakeRemote(t)
	f.put("deva", "a.md", "a")
	f.put("deva", "b.md", "b")
	f.del("devb", "a.md")
	h := f.server().Handler()

	var root Node
	if err := json.Unmarshal(get(t, h, "/api/tree").Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Children) != 1 || root.Children[0].Name != "b.md" {
		t.Fatalf("tree after delete = %+v, want only b.md", root.Children)
	}
	if rec := get(t, h, "/api/file?path=a.md"); rec.Code != 404 {
		t.Fatalf("deleted file: %d, want 404", rec.Code)
	}
}

func TestRenderMarkdown(t *testing.T) {
	f := newFakeRemote(t)
	f.put("deva", "doc.md", "# Title\n\nsee [[plan]] and [[plan|the plan]]\n\n<script>x</script>")
	h := f.server().Handler()

	rec := get(t, h, "/api/render?path=doc.md")
	if rec.Code != 200 {
		t.Fatalf("render: %d %s", rec.Code, rec.Body)
	}
	var doc struct {
		HTML   string `json:"html"`
		Author string `json:"author"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h1", "Title", `href="wiki:plan"`, ">the plan</a>"} {
		if !strings.Contains(doc.HTML, want) {
			t.Errorf("html missing %q:\n%s", want, doc.HTML)
		}
	}
	if strings.Contains(doc.HTML, "<script>") {
		t.Errorf("raw HTML not escaped:\n%s", doc.HTML)
	}
	if doc.Author != "deva@test" {
		t.Errorf("author = %q", doc.Author)
	}
}

// The viewer used to credit only Op.Author (the git/OS fallback) while
// History credited the signed-in account, so the same op got two different
// answers to "who changed this?". /render and /tree must carry the account.
func TestViewerCreditsSignedInAccount(t *testing.T) {
	f := newFakeRemote(t)
	f.putAs("deva", "solo@example.com", "E2E Solo", "memory.md", "# notes")
	f.put("devb", "old.md", "# pre-accounts") // pre-accounts shape: Author only
	h := f.server().Handler()

	type who struct {
		User     string `json:"user"`
		UserName string `json:"user_name"`
		Author   string `json:"author"`
	}
	var doc who
	if err := json.Unmarshal(get(t, h, "/api/render?path=memory.md").Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.User != "solo@example.com" || doc.UserName != "E2E Solo" || doc.Author != "deva@test" {
		t.Errorf("render who = %+v, want all three fields", doc)
	}

	// An op written before accounts existed keeps its Author and sends no
	// empty user fields — whoChanged() would print "unknown" for those.
	raw := get(t, h, "/api/render?path=old.md").Body.String()
	if strings.Contains(raw, `"user"`) || strings.Contains(raw, `"user_name"`) {
		t.Errorf("no-account op leaked user fields: %s", raw)
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Author != "devb@test" {
		t.Errorf("no-account op author = %q, want devb@test", doc.Author)
	}

	var root Node
	if err := json.Unmarshal(get(t, h, "/api/tree").Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	for _, n := range root.Children {
		switch n.Name {
		case "memory.md":
			if n.User != "solo@example.com" || n.UserName != "E2E Solo" || n.Author != "deva@test" {
				t.Errorf("tree node = %+v, want all three fields", n)
			}
		case "old.md":
			if n.User != "" || n.UserName != "" || n.Author != "devb@test" {
				t.Errorf("no-account tree node = %+v, want author only", n)
			}
		}
	}
}

func TestDownload(t *testing.T) {
	f := newFakeRemote(t)
	f.put("deva", "notes/plan.md", "content")
	h := f.server().Handler()

	rec := get(t, h, "/api/download?path=notes/plan.md")
	if rec.Code != 200 || rec.Body.String() != "content" {
		t.Fatalf("download: %d %q", rec.Code, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="plan.md"` {
		t.Fatalf("content-disposition = %q", cd)
	}
}

func TestFrontendServed(t *testing.T) {
	f := newFakeRemote(t)
	h := f.server().Handler()
	rec := get(t, h, "/")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>BearDrive</title>") {
		t.Fatalf("index: %d", rec.Code)
	}
}

// The brand is the product/self-hoster name, never the storage basename:
// a hub whose bucket is "beardrive" must not report that as its brand.
func TestConfigBrandNeverLeaksVolume(t *testing.T) {
	srv, _, _ := newHub(t, true, nil)
	srv.Volume = "beardrive"

	var cfg struct {
		Volume string `json:"volume"`
		Brand  string `json:"brand"`
	}
	read := func() {
		t.Helper()
		rec := do(t, srv.Handler(), "GET", "/api/config", nil)
		if rec.Code != 200 {
			t.Fatalf("config: %d %s", rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
			t.Fatal(err)
		}
	}

	read()
	if cfg.Brand != "" {
		t.Errorf("unconfigured brand = %q, want empty (the frontend defaults it)", cfg.Brand)
	}
	if cfg.Volume != "beardrive" {
		t.Errorf("volume = %q, want beardrive (VolumeApp reads this key)", cfg.Volume)
	}

	auth, err := OpenBuiltinAuth(filepath.Join(t.TempDir(), "auth.json"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth.Brand = "Acme Docs"
	srv.Auth = auth

	read()
	if cfg.Brand != "Acme Docs" {
		t.Errorf("configured brand = %q, want Acme Docs", cfg.Brand)
	}
}

// TestWikilinkRendering drives the real renderer over one document holding
// every context a [[…]] can sit in. Prose must keep emitting exactly the
// anchor FileView.tsx resolves; code must come out byte-identical, which is
// the bug — a mermaid subroutine node B[[label]] used to reach mermaid as
// B[label](wiki:label) and refuse to parse.
func TestWikilinkRendering(t *testing.T) {
	const mermaid = "flowchart LR\n  A[input] --> B[[stored record]]"
	src := "a [[x y]] b [[u|v]] c [[no\n\n" +
		"span `[[target]]` end\n\n" +
		"```mermaid\n" + mermaid + "\n```\n"

	got, err := RenderMarkdown([]byte(src))
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}

	// Prose: unchanged from the regex this replaced.
	for _, want := range []string{
		`<a href="wiki:x%20y">x y</a>`,
		`<a href="wiki:u">v</a>`,
		`c [[no`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prose: %q missing from\n%s", want, got)
		}
	}

	// Code span: literal, no rewrite and no anchor.
	if !strings.Contains(got, "<code>[[target]]</code>") {
		t.Errorf("code span was rewritten:\n%s", got)
	}

	// Fenced block: the bytes mermaid receives are the bytes on disk.
	inner := html.EscapeString(mermaid)
	if !strings.Contains(got, inner) {
		t.Errorf("fenced block not byte-identical, want %q in\n%s", inner, got)
	}
	if strings.Contains(got, "wiki:stored") {
		t.Errorf("fenced block was rewritten:\n%s", got)
	}
}
