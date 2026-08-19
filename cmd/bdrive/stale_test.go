package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

// staleRun drives the real cobra command and returns its combined output.
func staleRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := staleCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

// staleProject enrolls a folder and syncs it, so the journal — the only clock
// this command reads — actually has ops in it. The remote is unreachable on
// purpose: the cycle degrades offline and the local journal is still written.
// ages dates each file's mtime, which the scan carries into Op.Mtime.
func staleProject(t *testing.T, files map[string]string, ages map[string]time.Duration) string {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	if _, err := config.SaveProject(folder, config.Project{
		Volume: "wiki",
		Remote: "https://hub.example.com/p/p-12345678",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for rel, body := range files {
		abs := filepath.Join(folder, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if age, ok := ages[rel]; ok {
			when := now.Add(age)
			if err := os.Chtimes(abs, when, when); err != nil {
				t.Fatal(err)
			}
		}
	}
	sync := syncCmd()
	sync.SetOut(&bytes.Buffer{})
	sync.SetErr(&bytes.Buffer{})
	sync.SetArgs([]string{folder})
	if err := sync.Execute(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return folder
}

const day = 24 * time.Hour

// The core verdict: a doc is outgrown when something it references was written
// after it, and a doc whose references are all older is not.
func TestStaleReportsOutgrownDocsOnly(t *testing.T) {
	folder := staleProject(t, map[string]string{
		"docs/architecture.md":        "the syncer lives in [the syncer](../internal/syncer/syncer.go)\n",
		"docs/current.md":             "see `internal/journal/journal.go` for the model\n",
		"internal/syncer/syncer.go":   "package syncer\n",
		"internal/journal/journal.go": "package journal\n",
	}, map[string]time.Duration{
		"docs/architecture.md":        -40 * day,
		"internal/syncer/syncer.go":   -2 * day, // 38 days newer than the doc
		"docs/current.md":             -1 * day,
		"internal/journal/journal.go": -30 * day, // older than its doc
	})

	out, err := staleRun(t, folder)
	if err != nil {
		t.Fatalf("stale: %v\n%s", err, out)
	}
	for _, want := range []string{
		"docs/architecture.md",
		"internal/syncer/syncer.go",
		"38d newer",
		"1 outgrown doc, 1 stale reference",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "docs/current.md") {
		t.Errorf("a doc newer than everything it references was flagged:\n%s", out)
	}
}

// The acceptance criterion that fails on any mtime-based implementation: after
// a fresh materialize every file on disk carries this device's write time, so
// mtime comparison reports nothing on exactly the machine that most needs the
// answer. The journal still knows.
func TestStaleDatesFromTheJournalNotMtime(t *testing.T) {
	folder := staleProject(t, map[string]string{
		"guide.md":      "the loop is in [worker](src/worker.go)\n",
		"src/worker.go": "package src\n",
	}, map[string]time.Duration{
		"guide.md":      -20 * day,
		"src/worker.go": -3 * day,
	})

	// Simulate a fresh clone: materialize writes every blob now, so every
	// mtime on disk collapses onto one instant.
	same := time.Now()
	for _, rel := range []string{"guide.md", "src/worker.go"} {
		abs := filepath.Join(folder, filepath.FromSlash(rel))
		if err := os.Chtimes(abs, same, same); err != nil {
			t.Fatal(err)
		}
	}

	out, err := staleRun(t, folder)
	if err != nil {
		t.Fatalf("stale: %v\n%s", err, out)
	}
	if !strings.Contains(out, "guide.md") || !strings.Contains(out, "17d newer") {
		t.Errorf("identical mtimes must not erase the journal's answer:\n%s", out)
	}
}

// Resolution is the filter: a candidate that does not land on a synced file is
// not a reference, however path-shaped it looks.
func TestStaleReferenceResolution(t *testing.T) {
	synced := map[string]bool{
		"internal/syncer/syncer.go": true,
		"docs/hub-config.md":        true,
		"docs/nested/deep.go":       true,
		"README.md":                 true,
	}
	cases := []struct {
		name, docDir, cand, want string
	}{
		{"inline link, root-relative", ".", "internal/syncer/syncer.go", "internal/syncer/syncer.go"},
		{"relative to the doc's own dir", "docs/nested", "deep.go", "docs/nested/deep.go"},
		{"falls back to the root", "docs", "internal/syncer/syncer.go", "internal/syncer/syncer.go"},
		{"wikilink retried with .md", ".", "docs/hub-config.md", "docs/hub-config.md"},
		{"anchor stripped", ".", "README.md#install", "README.md"},
		{"query stripped", ".", "README.md?raw=1", "README.md"},
		{"trailing sentence period", ".", "README.md.", "README.md"},
		{"dot-slash prefix", ".", "./README.md", "README.md"},
		{"http url", ".", "https://example.com/README.md", ""},
		{"mailto", ".", "mailto:someone@example.com", ""},
		{"protocol-relative", ".", "//example.com/README.md", ""},
		{"absolute path", ".", "/etc/passwd", ""},
		{"escapes the mount", "docs", "../../../etc/passwd", ""},
		{"made up", ".", "internal/nope/missing.go", ""},
		{"empty", ".", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveRef(tc.docDir, tc.cand, synced)
			if tc.want == "" {
				if ok {
					t.Fatalf("resolved %q to %q, want dropped", tc.cand, got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("resolveRef(%q, %q) = %q,%v want %q", tc.docDir, tc.cand, got, ok, tc.want)
			}
		})
	}
}

// A URL, a ../ escape and a made-up path reach the command as real document
// text — and none of them may be counted.
func TestStaleIgnoresUnresolvableReferences(t *testing.T) {
	folder := staleProject(t, map[string]string{
		"notes.md": "see https://example.com/src/worker.go and ../../../etc/passwd and made/up/path.go\n" +
			"the real one is [worker](src/worker.go)\n",
		"src/worker.go": "package src\n",
	}, map[string]time.Duration{
		"notes.md":      -10 * day,
		"src/worker.go": -1 * day,
	})

	out, err := staleRun(t, folder)
	if err != nil {
		t.Fatalf("stale: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 outgrown doc, 1 stale reference") {
		t.Errorf("only the resolvable reference should count:\n%s", out)
	}
	for _, bad := range []string{"example.com", "etc/passwd", "made/up"} {
		if strings.Contains(out, bad) {
			t.Errorf("unresolvable %q was counted:\n%s", bad, out)
		}
	}
}

// A .bdriveignore rule excludes a file from this command exactly as it
// excludes it from sync — as a doc to scan and as a file that can age one.
func TestStaleHonorsBdriveignore(t *testing.T) {
	folder := staleProject(t, map[string]string{
		".bdriveignore":    "secret/\n",
		"guide.md":         "[a](secret/hidden.go) and [b](src/worker.go)\n",
		"secret/notes.md":  "[worker](../src/worker.go)\n",
		"secret/hidden.go": "package secret\n",
		"src/worker.go":    "package src\n",
	}, map[string]time.Duration{
		"guide.md":         -10 * day,
		"secret/notes.md":  -10 * day,
		"secret/hidden.go": -1 * day,
		"src/worker.go":    -2 * day,
	})

	out, err := staleRun(t, folder)
	if err != nil {
		t.Fatalf("stale: %v\n%s", err, out)
	}
	if strings.Contains(out, "secret/") {
		t.Errorf("an ignored path was scanned or counted:\n%s", out)
	}
	if !strings.Contains(out, "1 outgrown doc, 1 stale reference") {
		t.Errorf("the synced reference should still count:\n%s", out)
	}
}

// -l is paths only; -n caps the docs printed; both exit 0.
func TestStaleOutputAndFlags(t *testing.T) {
	folder := staleProject(t, map[string]string{
		"a.md":          "[w](src/worker.go)\n",
		"b.md":          "[c](src/cache.go)\n",
		"src/worker.go": "package src\n",
		"src/cache.go":  "package src\n",
	}, map[string]time.Duration{
		"a.md": -40 * day, "src/worker.go": -1 * day,
		"b.md": -10 * day, "src/cache.go": -5 * day,
	})

	out, err := staleRun(t, "-l", folder)
	if err != nil {
		t.Fatalf("stale -l: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if want := []string{"a.md", "b.md"}; len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Errorf("-l should print two bare paths worst-first, got %q", lines)
	}

	out, err = staleRun(t, "-l", "-n", "1", folder)
	if err != nil {
		t.Fatalf("stale -n 1: %v\n%s", err, out)
	}
	if !strings.Contains(out, "a.md") || strings.Contains(out, "b.md") {
		t.Errorf("-n 1 should print exactly the worst doc:\n%s", out)
	}
	if !strings.Contains(out, "output limited to 1 doc ") {
		t.Errorf("truncation should say so:\n%s", out)
	}

	// A clean project still exits 0 — this is advisory, not a gate.
	clean := staleProject(t, map[string]string{
		"a.md":          "[w](src/worker.go)\n",
		"src/worker.go": "package src\n",
	}, map[string]time.Duration{
		"a.md": -1 * day, "src/worker.go": -20 * day,
	})
	if out, err := staleRun(t, clean); err != nil {
		t.Fatalf("a clean project must exit 0: %v\n%s", err, out)
	} else if !strings.Contains(out, "no outgrown docs") {
		t.Errorf("a clean project should say so:\n%s", out)
	}
}

// A project that has never synced has no volume store, so nothing is datable.
// Same wording and same exit as `bdrive log`.
func TestStaleWithNoHistory(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	if _, err := config.SaveProject(folder, config.Project{
		Volume: "wiki",
		Remote: "https://hub.example.com/p/p-12345678",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "a.md"), []byte("[x](b.md)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := staleRun(t, folder)
	if err != nil {
		t.Fatalf("no history must exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no history yet") {
		t.Errorf("want `no history yet`, got:\n%s", out)
	}
}

// Two layers stand between a planted file name and the operator's terminal,
// and this asserts both: the scan refuses a path journal.SafePath rejects
// (walk.go:48), so a hostile name never becomes a synced path at all — and
// safeField still guards every string this command prints, because a filter
// that holds today is not a reason to print unfiltered tomorrow.
func TestStaleOutputCannotRewriteTheTerminal(t *testing.T) {
	hostile := "na‮me\x1b[31m\r.md"
	folder := staleProject(t, map[string]string{
		hostile:         "[w](src/worker.go)\n",
		"clean.md":      "[w](src/worker.go)\n",
		"src/worker.go": "package src\n",
	}, map[string]time.Duration{
		hostile: -10 * day, "clean.md": -10 * day, "src/worker.go": -1 * day,
	})

	for _, args := range [][]string{{folder}, {"-l", folder}} {
		out, err := staleRun(t, args...)
		if err != nil {
			t.Fatalf("stale %v: %v", args, err)
		}
		if !strings.Contains(out, "clean.md") {
			t.Fatalf("the hostile name must not suppress the rest of the report:\n%q", out)
		}
		for _, bad := range []string{"\x1b", "\r", "‮", "\x9b", "\x7f"} {
			if strings.Contains(out, bad) {
				t.Errorf("stale %v leaked %q into the terminal:\n%q", args, bad, out)
			}
		}
	}

	// Layer two, directly: the print path is safeField, so even if a hostile
	// path ever reached it, the row it draws is inert.
	if got := safeField(hostile, 160); strings.ContainsAny(got, "\x1b\r‮") {
		t.Errorf("safeField let a control character through: %q", got)
	}
}

// A read-only query must not enroll this device: LoadProject, never
// ResolveMount. Outside a project it says so, exits non-zero, and the registry
// is untouched.
func TestStaleOutsideAProjectWritesNothing(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "loose.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mounts := filepath.Join(os.Getenv("BDRIVE_HOME"), "mounts.json")
	before, beforeErr := os.ReadFile(mounts)

	out, err := staleRun(t, folder)
	if err == nil {
		t.Fatalf("should fail outside a project, got nil\n%s", out)
	}
	if !strings.Contains(err.Error(), "not a beardrive project") {
		t.Errorf("message should name the problem: %v", err)
	}
	after, afterErr := os.ReadFile(mounts)
	if (beforeErr == nil) != (afterErr == nil) || !bytes.Equal(before, after) {
		t.Errorf("the registry was written by a read-only query")
	}
}

// The write-time fold is a union across every device's journal, and it must
// survive a peer stamping an op in the year 9999: DisplayTime returns the zero
// time for that op, so max-by-DisplayTime drops it instead of dating the path
// to a future that makes every doc referencing it look stale.
func TestStaleFoldsAcrossDevicesAndClampsForgedStamps(t *testing.T) {
	folder := staleProject(t, map[string]string{
		"guide.md":      "[w](src/worker.go) and [c](src/cache.go)\n",
		"src/worker.go": "package src\n",
		"src/cache.go":  "package src\n",
	}, map[string]time.Duration{
		"guide.md":      -10 * day,
		"src/worker.go": -20 * day, // older than the doc: not stale on its own
		"src/cache.go":  -30 * day, // ditto
	})

	// Device B: a genuine later write to worker.go, and a forged year-9999
	// stamp on cache.go.
	proj, found, err := config.LoadProject(folder)
	if err != nil || !found {
		t.Fatalf("load project: %v %v", err, found)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.AppendOps("devb", []journal.Op{
		{
			Seq: 1, Lamport: 100, Time: now.Add(-2 * day), Device: "devb",
			Kind: journal.KindPut, Path: "src/worker.go", Blob: strings.Repeat("a", 64),
			Mtime: now.Add(-2 * day),
		},
		{
			Seq: 2, Lamport: 101, Time: now.Add(-time.Hour), Device: "devb",
			Kind: journal.KindPut, Path: "src/cache.go", Blob: strings.Repeat("b", 64),
			Mtime: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := staleRun(t, folder)
	if err != nil {
		t.Fatalf("stale: %v\n%s", err, out)
	}
	// Device B's real write is picked up across journals: 8 days newer.
	if !strings.Contains(out, "src/worker.go") || !strings.Contains(out, "8d newer") {
		t.Errorf("the newest write across devices should win:\n%s", out)
	}
	// The forged stamp is clamped to Op.Time (an hour ago), so cache.go is
	// stale by hours — not by eight thousand years.
	if strings.Contains(out, "2919") || strings.Contains(out, "9999") {
		t.Errorf("a forged year-9999 stamp reached the output:\n%s", out)
	}
	if !strings.Contains(out, "1 outgrown doc, 2 stale references") {
		t.Errorf("both references should count once, clamped:\n%s", out)
	}
}
