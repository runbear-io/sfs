package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

// The reported bug was read off `bdrive log`'s own output, so assert on that
// output: every printed timestamp descends, the file's edit time still reaches
// the reader (files written minutes apart don't collapse onto one), and -n
// keeps the newest by that stamp rather than the highest lamport.
//
// The first column is now when the change was journaled, so the edit time it
// used to carry is asserted where it is now printed — the appended
// `written ...` field. That is BEA-40's guarantee, moved, not dropped.
func TestLogPrintsNewestFirstByEditTime(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	if _, err := config.SaveProject(folder, config.Project{
		Volume: "wiki",
		Remote: "https://hub.example.com/p/p-12345678", // unreachable: the cycle degrades offline
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil { // enroll, as `bdrive init` would
		t.Fatal(err)
	}

	// Written in one scan, edited at three different times — and the newest
	// edit is deliberately not the one the walk sees last.
	now := time.Now()
	files := map[string]time.Duration{
		"a-oldest.md": -30 * time.Minute,
		"b-newest.md": -1 * time.Minute,
		"c-middle.md": -10 * time.Minute,
	}
	for rel, age := range files {
		abs := filepath.Join(folder, rel)
		if err := os.WriteFile(abs, []byte("content of "+rel), 0o644); err != nil {
			t.Fatal(err)
		}
		when := now.Add(age)
		if err := os.Chtimes(abs, when, when); err != nil {
			t.Fatal(err)
		}
	}

	sync := syncCmd()
	sync.SetOut(&bytes.Buffer{})
	sync.SetArgs([]string{folder})
	if err := sync.Execute(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rows := runLog(t, folder)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3:\n%v", len(rows), paths(rows))
	}
	if want := []string{"b-newest.md", "c-middle.md", "a-oldest.md"}; !equal(paths(rows), want) {
		t.Fatalf("row order = %v, want %v", paths(rows), want)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].stamp.After(rows[i-1].stamp) {
			t.Fatalf("printed stamps are not newest-first: %v then %v", rows[i-1].stamp, rows[i].stamp)
		}
	}
	// Three edits minutes apart must still reach the reader as three distinct
	// write times; before BEA-40 one scan stamped them all with its own time.
	for i, r := range rows {
		if r.written.IsZero() {
			t.Fatalf("row %d (%s) dropped the file's write time", i, r.path)
		}
	}
	if rows[0].written.Equal(rows[1].written) || rows[1].written.Equal(rows[2].written) {
		t.Fatalf("write times collapsed onto the sync time: %v %v %v",
			rows[0].written, rows[1].written, rows[2].written)
	}

	// -n truncates after the display sort.
	top := runLog(t, folder, "-n", "2")
	if want := []string{"b-newest.md", "c-middle.md"}; !equal(paths(top), want) {
		t.Fatalf("-n 2 = %v, want %v", paths(top), want)
	}

	// -p still filters to one path.
	only := runLog(t, folder, "-p", "c-middle.md")
	if want := []string{"c-middle.md"}; !equal(paths(only), want) {
		t.Fatalf("-p = %v, want %v", paths(only), want)
	}
}

// A rename is one change, and `mv` preserves mtime — so before this, the put
// half was stamped with the original file's write time and sorted away from
// its own delete, which is how a file that appeared seconds ago falls below
// the fold of "what changed since yesterday".
func TestLogKeepsARenameTogether(t *testing.T) {
	folder := logFixture(t)

	old := filepath.Join(folder, "architecture.md")
	if err := os.WriteFile(old, []byte("# architecture"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Written well before the rename: the gap is the bug.
	long := time.Now().Add(-90 * time.Minute)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}
	// Filler, so "adjacent at the top" is a real claim and not the only rows.
	for _, rel := range []string{"notes.md", "todo.md"} {
		if err := os.WriteFile(filepath.Join(folder, rel), []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runSync(t, folder)

	if err := os.Rename(old, filepath.Join(folder, "arch-v2.md")); err != nil {
		t.Fatal(err)
	}
	runSync(t, folder)

	rows := runLog(t, folder)
	if len(rows) < 2 {
		t.Fatalf("got %d rows, want the rename plus fillers: %v", len(rows), paths(rows))
	}
	// The two halves are one change and sit together at the top. The delete
	// leads: it has no file left to stat, so it sorts on the commit time while
	// the put still carries its 90-minute-old write time as the tie-break.
	got := []string{rows[0].kind + " " + rows[0].path, rows[1].kind + " " + rows[1].path}
	want := []string{"delete architecture.md", "put arch-v2.md"}
	if !equal(got, want) {
		t.Fatalf("top two rows = %v, want the rename's two halves %v\nall: %v", got, want, paths(rows))
	}
	if !rows[0].stamp.Equal(rows[1].stamp) {
		t.Fatalf("one rename printed two stamps: %v and %v", rows[0].stamp, rows[1].stamp)
	}
	// The write time is what makes the 90-minute gap legible instead of silent.
	if rows[1].written.IsZero() {
		t.Fatal("the put half dropped the file's write time")
	}
	if gap := rows[1].stamp.Sub(rows[1].written); gap < time.Hour {
		t.Fatalf("put half's write time = %v, stamp = %v: the original mtime was lost",
			rows[1].written, rows[1].stamp)
	}
	// A delete has no file left to stat, so it never carries the field.
	if !rows[0].written.IsZero() {
		t.Fatalf("delete row carried a write time: %v", rows[0].written)
	}
}

// An old document dropped into the project today is a change today. It used to
// sort by its own mtime, i.e. below everything journaled since it was written.
func TestLogSortsAnOldFileByWhenItArrived(t *testing.T) {
	folder := logFixture(t)

	recent := filepath.Join(folder, "recent.md")
	if err := os.WriteFile(recent, []byte("edited a minute ago"), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(recent, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	runSync(t, folder)

	// Journaled second, written days before the file above it.
	ancient := filepath.Join(folder, "ancient.md")
	if err := os.WriteFile(ancient, []byte("from the archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(ancient, old, old); err != nil {
		t.Fatal(err)
	}
	runSync(t, folder)

	rows := runLog(t, folder)
	if want := []string{"ancient.md", "recent.md"}; !equal(paths(rows), want) {
		t.Fatalf("row order = %v, want %v — the newly arrived file sorts first", paths(rows), want)
	}
	if rows[0].written.IsZero() || rows[0].stamp.Sub(rows[0].written) < 24*time.Hour {
		t.Fatalf("ancient.md lost its write time: stamp %v, written %v", rows[0].stamp, rows[0].written)
	}
}

// logFixture is an enrolled folder pointed at an unreachable hub, so the cycle
// degrades offline and the journal is all local.
func logFixture(t *testing.T) string {
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
	return folder
}

func runSync(t *testing.T, folder string) {
	t.Helper()
	c := syncCmd()
	c.SetOut(&bytes.Buffer{})
	c.SetArgs([]string{folder})
	if err := c.Execute(); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// logRow is one parsed `bdrive log` line: the leading stamp (when the change
// was journaled), the kind and path columns, and the appended write time when
// the row carries one.
type logRow struct {
	stamp   time.Time
	written time.Time
	kind    string
	path    string
}

// runLog runs `bdrive log` and parses its rows.
func runLog(t *testing.T, folder string, extra ...string) []logRow {
	t.Helper()
	c := logCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetArgs(append([]string{folder}, extra...))
	if err := c.Execute(); err != nil {
		t.Fatalf("log: %v", err)
	}
	const stampFmt = "2006-01-02 15:04:05"
	var rows []logRow
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		ts, err := time.ParseInLocation(stampFmt, line[:19], time.Local)
		if err != nil {
			t.Fatalf("unparsable log row %q: %v", line, err)
		}
		fields := strings.Fields(line[19:])
		row := logRow{stamp: ts, kind: fields[0], path: fields[1]}
		if _, rest, ok := strings.Cut(line, "(written "); ok {
			stamp, _, _ := strings.Cut(rest, ")")
			w, err := time.ParseInLocation(stampFmt, stamp, time.Local)
			if err != nil {
				t.Fatalf("unparsable write time in %q: %v", line, err)
			}
			row.written = w
		}
		rows = append(rows, row)
	}
	return rows
}

func paths(rows []logRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.path
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
