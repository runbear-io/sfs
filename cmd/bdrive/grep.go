package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// errNoMatch is grep's "found nothing" exit, not a failure: the convention is
// status 1 with no message, so `bdrive grep x || echo none` composes. Real
// errors (bad pattern, not a project) still print — see grepCmd.
var errNoMatch = errors.New("no match")

// binarySniff is how much of a file is read to decide it is not text. A NUL
// byte in the first 8 KB is the rule: cheap, and the same one grep uses.
const binarySniff = 8 << 10

// maxLineScan bounds one line, so a minified bundle that syncs cannot make
// grep buffer it whole. Past this the rest of that file is skipped.
const maxLineScan = 1 << 20

func grepCmd() *cobra.Command {
	var (
		ignoreCase bool
		fixed      bool
		filesOnly  bool
		limit      int
	)
	c := &cobra.Command{
		Use:   "grep <pattern> [folder]",
		Short: "Search the text inside the files this project syncs",
		Long: `Search file contents in a synced folder.

pattern is a Go regular expression (RE2), or a literal string with -F. Only
the files this project actually syncs are searched: a .bdriveignore rule or a
narrowed ` + "`bdrive scope`" + ` excludes a file from search exactly as it excludes it
from sync, and binary files are skipped.

It searches the real files on disk, not the hub — a pure read with no daemon,
no lock, and no network, so it works offline and never blocks on a sync in
progress. Exit status is 0 when something matched and 1 when nothing did.`,
		Example: `  bdrive grep 'retention.*fold'   # regexp over the whole project
  bdrive grep -i -l TODO          # case-insensitive, paths only
  bdrive grep -F 'a[b]c' docs     # literal string, inside ./docs`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runGrep(cmd, args[0], args[1:], ignoreCase, fixed, filesOnly, limit)
			// SilenceErrors below is for errNoMatch alone, so anything else
			// has to print itself — cobra no longer will.
			if err != nil && !errors.Is(err, errNoMatch) {
				fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
			}
			return err
		},
	}
	// Silenced so a no-match exits 1 without printing anything, the way grep
	// does — errNoMatch is a status, not a failure, and cobra would otherwise
	// render it as an error and a usage block.
	c.SilenceErrors = true
	c.SilenceUsage = true
	c.Flags().BoolVarP(&ignoreCase, "ignore-case", "i", false, "match case-insensitively")
	c.Flags().BoolVarP(&fixed, "fixed-strings", "F", false, "treat the pattern as a literal string, not a regexp")
	c.Flags().BoolVarP(&filesOnly, "files-with-matches", "l", false, "print matching paths only, one per line")
	// -n means the same here as in `bdrive log`: max rows out.
	c.Flags().IntVarP(&limit, "limit", "n", 200, "max matching lines printed (0 = all)")
	return c
}

func runGrep(cmd *cobra.Command, pattern string, folderArg []string, ignoreCase, fixed, filesOnly bool, limit int) error {
	if fixed {
		pattern = regexp.QuoteMeta(pattern)
	}
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("bad pattern: %w", err)
	}
	folder, err := absFolder(folderArg)
	if err != nil {
		return err
	}
	// LoadProject, not ResolveMount: ResolveMount self-heals the registry
	// path, i.e. it enrolls this device. A read-only query must not have that
	// side effect — the rule logReads follows for the same reason.
	proj, found, err := config.LoadProject(folder)
	if err != nil {
		return err
	}
	if !found {
		return notAProject(folder)
	}

	// The rules this device has ACCEPTED, so results match what the cycle
	// actually uploads (syncer.Filter.SkipUp). Best-effort, exactly as
	// `bdrive scope --explain` does it: store.Open takes no volume flock, so
	// this cannot block behind a running daemon, and a store that will not
	// open degrades the answer to the live rules rather than failing a read.
	//
	// The Stat guard is this command's own: store.Open MkdirAlls the volume
	// directory, and a search must not create one for a project that has never
	// synced. No store means no accepted rules, which is what "" already says.
	var accepted string
	if vdir, verr := config.VolumeDir(proj.ID); verr == nil && dirExists(vdir) {
		if st, serr := store.Open(vdir); serr == nil {
			if sync, serr := st.LoadSync(); serr == nil {
				accepted = sync.IgnoreAccepted
			}
		}
	}

	paths, err := syncer.SyncedFiles(folder, proj.Include, accepted)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	var lines, files int
	truncated := false
	for _, rel := range paths {
		hits := grepFile(filepath.Join(folder, rel), re, filesOnly)
		if len(hits) == 0 {
			continue
		}
		files++
		for _, h := range hits {
			if limit > 0 && lines >= limit {
				truncated = true
				break
			}
			if filesOnly {
				fmt.Fprintln(out, safeField(rel, 160))
			} else {
				// Both the path and the matched text are content a teammate
				// wrote and synced — the same trust level `bdrive log` treats
				// journal strings with, over a wider surface. safeField, or a
				// lone CR repaints the row and U+202E reverses it.
				fmt.Fprintf(out, "%s:%d: %s\n", safeField(rel, 160), h.line, safeField(h.text, 400))
			}
			lines++
		}
		if truncated {
			break
		}
	}
	if lines == 0 {
		return errNoMatch
	}
	if !filesOnly {
		fmt.Fprintf(out, "%s, %s\n", plural(files, "file"), plural(lines, "matching line"))
	}
	if truncated {
		fmt.Fprintf(out, "output limited to %d lines — use -n 0 for all\n", limit)
	}
	return nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// hit is one matching line: its 1-based number and its text.
type hit struct {
	line int
	text string
}

// grepFile returns the matching lines in one file, or nothing when the file is
// binary or unreadable — an unreadable file is skipped, never fatal, the same
// posture the scan takes. With filesOnly it stops at the first match.
func grepFile(abs string, re *regexp.Regexp, filesOnly bool) []hit {
	f, err := os.Open(abs)
	if err != nil {
		return nil
	}
	defer f.Close()

	head := make([]byte, binarySniff)
	n, err := f.Read(head)
	if err != nil && err != io.EOF {
		return nil
	}
	head = head[:n]
	if bytes.IndexByte(head, 0) >= 0 {
		return nil // binary
	}

	var hits []hit
	sc := bufio.NewScanner(io.MultiReader(bytes.NewReader(head), f))
	sc.Buffer(make([]byte, 0, 64<<10), maxLineScan)
	for lineno := 1; sc.Scan(); lineno++ {
		if !re.Match(sc.Bytes()) {
			continue
		}
		hits = append(hits, hit{line: lineno, text: sc.Text()})
		if filesOnly {
			break
		}
	}
	return hits // sc.Err() ignored: an over-long line ends this file, not the run
}
