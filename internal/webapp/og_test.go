package webapp

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOGMetaOmitsEmptyAndEscapes(t *testing.T) {
	tests := []struct {
		name               string
		title, desc, image string
		ogType, url        string
		want, absent       []string
	}{
		{
			name: "minimal", title: "spec.md", ogType: "website", url: "https://h/x",
			want: []string{
				`<meta property="og:title" content="spec.md">`,
				`<meta property="og:site_name" content="BearDrive">`,
				`<meta property="og:type" content="website">`,
				`<meta property="og:url" content="https://h/x">`,
			},
			// An empty description or image renders a broken card: no tag at
			// all is the correct output, not an empty one.
			absent: []string{"og:description", "og:image"},
		},
		{
			name: "full", title: "a", desc: "hello", image: "https://cdn.example/i.png",
			ogType: "article", url: "https://h/s/tok",
			want: []string{
				`<meta property="og:description" content="hello">`,
				`<meta property="og:image" content="https://cdn.example/i.png">`,
			},
		},
		{
			name: "relative image dropped", title: "a", image: "diagram.png",
			ogType: "article", url: "https://h/s/tok", absent: []string{"og:image"},
		},
		{
			name: "scheme-relative image dropped", title: "a", image: "//cdn/x.png",
			ogType: "article", url: "https://h/s/tok", absent: []string{"og:image"},
		},
		{
			name: "javascript image dropped", title: "a", image: "javascript:alert(1)",
			ogType: "article", url: "https://h/s/tok", absent: []string{"og:image"},
		},
		{
			name:   `hostile`,
			title:  `"><script>alert(1)</script>`,
			desc:   `he said "hi" & <b>left</b>`,
			ogType: "website", url: "https://h/x",
			want: []string{`&#34;&gt;&lt;script&gt;`, `&amp;`, `&lt;b&gt;`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ogMeta(tc.title, tc.desc, tc.image, tc.ogType, tc.url)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %s", w, got)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("unexpected %q in %s", a, got)
				}
			}
			// Nothing hostile survives into attribute position. Checked per
			// tag: an unescaped ">" or quote in a value would truncate its
			// own tag, so a well-formed line is itself the assertion.
			for _, meta := range ogLines(got) {
				if !strings.HasSuffix(meta, `">`) {
					t.Errorf("malformed tag %q", meta)
				}
				for _, bad := range []string{"<script", "<b>", `content="">`} {
					if strings.Contains(meta, bad) {
						t.Errorf("unescaped %q in %s", bad, meta)
					}
				}
			}
		})
	}
}

func TestShellTitleNamesEveryRouteShape(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	for _, tc := range []struct{ upath, want string }{
		{"", ""},
		{"index.html", ""},
		{"join/abc123", ""},
		{"orgs/o1", ""},
		{"billing", ""},
		{"not-a-project-id/notes.md", ""},
		{"3f2b1c4d-0000-0000-0000-000000000000/notes.md", ""}, // well-shaped, unknown
		{p.ID, "proj"},
		{p.ID + "/dashboard", "Dashboard — proj"},
		{p.ID + "/history", "History — proj"},
		{p.ID + "/install", "Install — proj"},
		{p.ID + "/settings", "Settings — proj"},
		{p.ID + "/insights", "Dashboard — proj"}, // legacy alias
		{p.ID + "/notes/spec.md", "spec.md — proj"},
		{p.ID + "/notes", "notes — proj"},
		{p.ID + "/a/b/c/deep.md", "deep.md — proj"},
	} {
		if got := srv.shellTitle(tc.upath); got != tc.want {
			t.Errorf("shellTitle(%q) = %q, want %q", tc.upath, got, tc.want)
		}
	}

	// Volume mode has no project to name: the basename alone.
	vol := &Server{}
	for _, tc := range []struct{ upath, want string }{
		{"", ""},
		{"notes/spec.md", "spec.md"},
	} {
		if got := vol.shellTitle(tc.upath); got != tc.want {
			t.Errorf("volume shellTitle(%q) = %q, want %q", tc.upath, got, tc.want)
		}
	}
}

func TestShellOpenGraph(t *testing.T) {
	srv, p, _ := newHub(t, true, nil)
	h := srv.Handler()

	generic := do(t, h, "GET", "/", nil).Body.String()
	if !strings.Contains(generic, "<title>BearDrive</title>") {
		t.Fatal("root shell lost its generic title")
	}

	// Anonymous: no session anywhere in this test.
	rec := do(t, h, "GET", "/"+p.ID+"/notes/spec.md", nil)
	body := rec.Body.String()
	for _, want := range []string{
		"<title>spec.md — proj</title>",
		`<meta property="og:title" content="spec.md — proj">`,
		`<meta property="og:site_name" content="BearDrive">`,
		`<meta property="og:type" content="website">`,
		`<meta property="og:url" content="http://example.com/` + p.ID + `/notes/spec.md">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("file route missing %q", want)
		}
	}
	// The shell is unauthenticated, so nothing derived from file CONTENT
	// may appear on it.
	for _, bad := range []string{"og:description", "og:image", "<title>BearDrive</title>"} {
		if strings.Contains(body, bad) {
			t.Errorf("file route should not contain %q", bad)
		}
	}
	// Everything except the head is untouched.
	if strings.Count(body, "<script type=\"module\"") != strings.Count(generic, "<script type=\"module\"") {
		t.Error("shell body changed beyond the head")
	}
	for _, hdr := range [][2]string{
		{"Cache-Control", "no-cache"},
		{"X-Frame-Options", "DENY"},
		{"X-Content-Type-Options", "nosniff"},
		{"Content-Security-Policy", "frame-ancestors 'none'"},
	} {
		if got := rec.Header().Get(hdr[0]); got != hdr[1] {
			t.Errorf("%s = %q, want %q", hdr[0], got, hdr[1])
		}
	}

	if b := do(t, h, "GET", "/"+p.ID+"/dashboard", nil).Body.String(); !strings.Contains(b,
		`<meta property="og:title" content="Dashboard — proj">`) {
		t.Error("view route not named")
	}

	// Generic routes stay byte-identical to the root shell — including an id
	// that is shaped like a project but does not exist, which must not become
	// an existence oracle.
	for _, u := range []string{"/join/abc", "/3f2b1c4d-0000-0000-0000-000000000000/x.md"} {
		if b := do(t, h, "GET", u, nil).Body.String(); b != generic {
			t.Errorf("%s should serve the untouched shell", u)
		}
	}

	// assets/* never reaches the template and keeps its immutable caching.
	arec := do(t, h, "GET", "/assets/", nil)
	if strings.Contains(arec.Body.String(), "og:") {
		t.Error("assets response was templated")
	}
}

func TestShareDescriptionAndImage(t *testing.T) {
	render := func(t *testing.T, src string) ([]FrontmatterPair, string) {
		t.Helper()
		pairs, _ := frontmatterPairs([]byte(src))
		body, err := RenderMarkdown([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		return pairs, body
	}

	long := strings.Repeat("alpha beta ", 60) // ~660 chars of prose
	for _, tc := range []struct {
		name, src, wantDesc, wantImage string
	}{
		{
			name:     "frontmatter wins",
			src:      "---\ntitle: T\ndescription: from frontmatter\n---\n\nopening paragraph\n",
			wantDesc: "from frontmatter",
		},
		{
			name:     "case-insensitive key",
			src:      "---\nDescription: cased\n---\n\nbody\n",
			wantDesc: "cased",
		},
		{
			name:     "falls back to first paragraph",
			src:      "# Heading\n\nfirst **prose** here\n\nsecond\n",
			wantDesc: "first prose here",
		},
		{
			name:     "entities unescaped, whitespace collapsed",
			src:      "a &amp; b\nwrapped   line\n",
			wantDesc: "a & b wrapped line",
		},
		{
			name: "no prose at all",
			src:  "# Only a heading\n",
		},
		{
			name:      "absolute image",
			src:       "text\n\n![d](https://cdn.example/d.png)\n",
			wantDesc:  "text",
			wantImage: "https://cdn.example/d.png",
		},
		{
			name:     "relative image ignored",
			src:      "text\n\n![d](diagram.png)\n",
			wantDesc: "text",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pairs, body := render(t, tc.src)
			if got := shareDescription(pairs, body); got != tc.wantDesc {
				t.Errorf("description = %q, want %q", got, tc.wantDesc)
			}
			if got := shareImage(body); got != tc.wantImage {
				t.Errorf("image = %q, want %q", got, tc.wantImage)
			}
		})
	}

	t.Run("truncates on a word boundary", func(t *testing.T) {
		pairs, body := render(t, long)
		got := shareDescription(pairs, body)
		if n := len([]rune(got)); n > 200 {
			t.Fatalf("len = %d, want <= 200", n)
		}
		if !strings.HasSuffix(got, "…") || strings.Contains(got, "alph…") {
			t.Fatalf("not cut on a word boundary: %q", got)
		}
	})
}

func TestShareOpenGraph(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	f.put("dev1", "wiki/card.md", "---\ndescription: the card blurb\n---\n\n# Card\n\n![d](https://cdn.example/d.png)\n")
	f.put("dev1", "wiki/logo.png", "\x89PNG\r\n\x1a\nbinary")

	mdTok, _ := authedShare(t, srv, h, p.ID, "wiki/card.md")
	rec := doHTTP(h, httptest.NewRequest("GET", "/s/"+mdTok, nil))
	if rec.Code != 200 {
		t.Fatalf("md share: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<title>card.md</title>",
		`<meta property="og:title" content="card.md">`,
		`<meta property="og:site_name" content="BearDrive">`,
		`<meta property="og:type" content="article">`,
		`<meta property="og:url" content="http://example.com/s/` + mdTok + `">`,
		`<meta property="og:description" content="the card blurb">`,
		`<meta property="og:image" content="https://cdn.example/d.png">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("md share missing %q", want)
		}
	}
	for _, hdr := range [][2]string{
		{"Content-Security-Policy", "sandbox allow-scripts allow-popups"},
		{"X-Content-Type-Options", "nosniff"},
		{"Referrer-Policy", "no-referrer"},
	} {
		if got := rec.Header().Get(hdr[0]); got != hdr[1] {
			t.Errorf("%s = %q, want %q", hdr[0], got, hdr[1])
		}
	}

	// A markdown share with neither blurb nor image emits neither tag.
	f.put("dev1", "wiki/bare.md", "# Just a heading\n")
	bareTok, _ := authedShare(t, srv, h, p.ID, "wiki/bare.md")
	bare := doHTTP(h, httptest.NewRequest("GET", "/s/"+bareTok, nil)).Body.String()
	for _, bad := range []string{"og:description", "og:image"} {
		if strings.Contains(bare, bad) {
			t.Errorf("bare share should not carry %q", bad)
		}
	}

	// The hub never injects into raw HTML or binary bodies: those responses
	// are the stored bytes, unchanged.
	for _, tc := range []struct{ path, want string }{
		{"wiki/report.html", "<h1>Q3</h1><script>alert(1)</script>"},
		{"wiki/logo.png", "\x89PNG\r\n\x1a\nbinary"},
	} {
		tok, _ := authedShare(t, srv, h, p.ID, tc.path)
		got := doHTTP(h, httptest.NewRequest("GET", "/s/"+tok, nil)).Body.String()
		if got != tc.want {
			t.Errorf("%s body = %q, want the source bytes", tc.path, got)
		}
		if strings.Contains(got, "og:") {
			t.Errorf("%s was templated", tc.path)
		}
	}
}

func TestOGEscapesHostileNames(t *testing.T) {
	srv, p, _, f, h := shareHub(t)

	// The shell needs no storage: shellTitle reads the URL and the project
	// name, so a hostile project name is enough to exercise it. projectLabel
	// strips ( and ), so assert against the name the registry actually
	// stored rather than the literal we asked for.
	hostile, _, err := srv.Projects.GetOrCreate(`<img src=x onerror=alert1>`, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hostile.Name, "<img") {
		t.Fatalf("name was sanitized away: %q", hostile.Name)
	}
	shell := do(t, h, "GET", "/"+hostile.ID+`/%22%3E%3Cscript%3E.md`, nil).Body.String()

	f.put("dev1", `"><script>.md`, "# hi\n\n\"><script>alert(1)</script> prose\n")
	tok, _ := authedShare(t, srv, h, p.ID, `"><script>.md`)
	share := doHTTP(h, httptest.NewRequest("GET", "/s/"+tok, nil)).Body.String()

	for name, page := range map[string]string{"shell": shell, "share": share} {
		if len(ogLines(page)) == 0 {
			t.Errorf("%s: no og tags emitted:\n%s", name, headOf(page))
			continue
		}
		for _, meta := range ogLines(page) {
			if !strings.HasSuffix(meta, `">`) {
				t.Errorf("%s: malformed tag %q", name, meta)
			}
			for _, bad := range []string{"<script", "<img "} {
				if strings.Contains(meta, bad) {
					t.Errorf("%s: unescaped %q in %s", name, bad, meta)
				}
			}
		}
	}
	if !strings.Contains(shell, "&lt;img") {
		t.Errorf("shell did not escape the project name:\n%s", headOf(shell))
	}
	if !strings.Contains(share, "&#34;&gt;&lt;script&gt;") {
		t.Errorf("share did not escape the file name:\n%s", headOf(share))
	}
}

// ogLines returns each og: meta tag on a page, for assertions that care about
// what ended up inside a content= attribute.
func ogLines(page string) []string {
	var out []string
	for rest := page; ; {
		i := strings.Index(rest, `<meta property="og:`)
		if i < 0 {
			return out
		}
		rest = rest[i:]
		j := strings.Index(rest, ">")
		if j < 0 {
			return append(out, rest)
		}
		out = append(out, rest[:j+1])
		rest = rest[j+1:]
	}
}

func headOf(page string) string {
	if i := strings.Index(page, "</head>"); i >= 0 {
		return page[:i]
	}
	if len(page) > 600 {
		return page[:600]
	}
	return page
}
