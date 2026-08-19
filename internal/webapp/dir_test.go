package webapp

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dirServer(t *testing.T, files map[string]string) http.Handler {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{Source: &DirSource{Root: root}, Volume: "local", Refresh: 0}
	return s.Handler()
}

func TestDirSourceServesFolder(t *testing.T) {
	h := dirServer(t, map[string]string{
		"README.md":      "# Local",
		"notes/plan.md":  "content",
		"notes/props.md": "---\ntitle: Plan\ntags: [a, b]\n---\n\n# Heading\n",
		".bdrive":        `{"volume":"x"}`, // settings file must be hidden
		".git/config":    "noise",          // .git must be skipped
	})

	var root Node
	if err := json.Unmarshal(get(t, h, "/api/tree").Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, n := range root.Children {
		names = append(names, n.Name)
	}
	if len(root.Children) != 2 || root.Children[0].Name != "notes" || root.Children[1].Name != "README.md" {
		t.Fatalf("tree children = %v, want [notes README.md]", names)
	}

	rec := get(t, h, "/api/file?path=notes/plan.md")
	if rec.Code != 200 || rec.Body.String() != "content" {
		t.Fatalf("file: %d %q", rec.Code, rec.Body)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("dir source should still produce ETags")
	}

	rec = get(t, h, "/api/render?path=README.md")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Local") {
		t.Fatalf("render: %d %s", rec.Code, rec.Body)
	}
	// A plain folder has no account behind a file. Sending empty user
	// fields would make the viewer's attribution helper print "unknown"
	// where this mode has always printed nothing.
	if strings.Contains(rec.Body.String(), `"user"`) || strings.Contains(rec.Body.String(), `"user_name"`) {
		t.Errorf("plain-folder render carries identity: %s", rec.Body)
	}

	// Frontmatter travels as ordered data, not as a table inside html —
	// the viewer puts it in a side panel, so the body starts with the body.
	rec = get(t, h, "/api/render?path=notes/props.md")
	var doc struct {
		HTML        string            `json:"html"`
		Frontmatter []FrontmatterPair `json:"frontmatter"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Frontmatter) != 2 || doc.Frontmatter[0].Key != "title" ||
		doc.Frontmatter[1].Value != "a, b" {
		t.Errorf("frontmatter = %+v", doc.Frontmatter)
	}
	if strings.Contains(doc.HTML, `class="frontmatter"`) {
		t.Errorf("render still bakes the table into html: %s", doc.HTML)
	}
	// A doc without any: the field is absent, so the viewer shows no panel.
	if rec := get(t, h, "/api/render?path=README.md"); strings.Contains(rec.Body.String(), `"frontmatter"`) {
		t.Errorf("render carries an empty frontmatter field: %s", rec.Body)
	}

	if rec := get(t, h, "/api/file?path=.git/config"); rec.Code != 404 {
		t.Fatalf(".git content must be hidden, got %d", rec.Code)
	}
	if rec := get(t, h, "/api/file?path=../escape"); rec.Code != 404 {
		t.Fatalf("path traversal must 404, got %d", rec.Code)
	}
}

// Synced HTML served inline must never run with the hub origin's session:
// the file endpoint sandboxes it (same posture as /s/* shares). Downloads
// are exempt — an attachment never executes in the hub's origin.
func TestInlineHTMLIsSandboxed(t *testing.T) {
	h := dirServer(t, map[string]string{
		"page.html": "<h1>hi</h1><script>1</script>",
		"pic.svg":   "<svg xmlns='http://www.w3.org/2000/svg'/>",
		"plan.md":   "# md",
	})
	for path, wantCSP := range map[string]bool{
		"/api/file?path=page.html":     true,
		"/api/file?path=pic.svg":       true,
		"/api/file?path=plan.md":       false,
		"/api/download?path=page.html": false, // attachment, not rendered
	} {
		rec := get(t, h, path)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if wantCSP && csp != "sandbox allow-scripts" {
			t.Errorf("%s: CSP = %q, want sandbox", path, csp)
		}
		if !wantCSP && csp != "" {
			t.Errorf("%s: unexpected CSP %q", path, csp)
		}
	}
}

// The frontend serves real assets directly but returns the app shell for any
// client-side route (a deep file path, /join/<token>), so a deep link or
// refresh doesn't 404. Reserved API/auth/share prefixes stay real 404s.
func TestFrontendSPAFallback(t *testing.T) {
	h := dirServer(t, map[string]string{"notes/plan.md": "content"})

	shell := func(url string) {
		t.Helper()
		rec := get(t, h, url)
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: want 200 html, got %d %s", url, rec.Code, rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(rec.Body.String(), `id="root"`) {
			t.Fatalf("%s: expected the app shell, got %.60q", url, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: the shell must revalidate, got Cache-Control %q", url, cc)
		}
	}
	// Client routes all resolve to the shell, not a 404 or file content.
	shell("/")
	shell("/notes/plan.md")            // a deep file route (not the raw file)
	shell("/p-deadbeef/notes/plan.md") // hub-style route
	shell("/join/abc123")              // invite route

	// Real assets are served as themselves, cacheable forever: Vite emits
	// content-hashed filenames, so find one instead of hardcoding a hash.
	assets, err := fs.Glob(staticFiles, "static/assets/*.js")
	if err != nil || len(assets) == 0 {
		t.Fatalf("no built js asset embedded (run npm run build in frontend/): %v", err)
	}
	rec := get(t, h, strings.TrimPrefix(assets[0], "static"))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("%s: %d %s", assets[0], rec.Code, rec.Header().Get("Content-Type"))
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("hashed assets must be immutable, got Cache-Control %q", cc)
	}

	// A mistyped API path is a genuine 404, not the shell.
	if rec := get(t, h, "/api/bogus"); rec.Code != 404 {
		t.Fatalf("/api/bogus: want 404, got %d", rec.Code)
	}
}

// Every file the frontend build wrote must actually be IN the binary. A bare
// `//go:embed static` skips names starting with `_` or `.`, which is how Vite's
// first `_commonjsHelpers-<hash>.js` chunk went missing: the build passes, the
// commit looks right, and the running app answers that chunk with index.html
// until the browser refuses the whole entry over its MIME type. Nothing about
// that is visible without loading the page, so it is asserted here instead.
func TestEveryBuiltAssetIsEmbedded(t *testing.T) {
	built := os.DirFS("static")
	embedded, err := fs.Sub(staticFiles, "static")
	if err != nil {
		t.Fatal(err)
	}
	err = fs.WalkDir(built, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if _, err := fs.Stat(embedded, p); err != nil {
			t.Errorf("static/%s is on disk but not in the binary — //go:embed needs the all: prefix for a name like this", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
