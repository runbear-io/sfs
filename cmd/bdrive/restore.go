package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// restoreNoteTTL keeps the restore note short-lived: it also stamps whatever
// the daemon happens to commit next, so it must not outlive the restore by
// much.
const restoreNoteTTL = 2 * time.Minute

func restoreCmd() *cobra.Command {
	var list bool
	c := &cobra.Command{
		Use:   "restore <file> [version]",
		Short: "Put an earlier version of a file back",
		Long: `Restore an earlier version of a file: bdrive writes those bytes back as a
NEW change. Nothing is erased — the versions in between stay in the history,
the restore itself shows up in "bdrive log", and it syncs to every device and
teammate like any other edit (so it can be restored away from too).

With no version, restores the one immediately before the current content. A
version is a short content hash from "bdrive log" or --list; any unambiguous
prefix works.

Restore puts content back; it does not delete. To un-create a file a run
created, use the undo button on that row in the hub's History view — or just
delete the file here and let the next sync carry it.`,
		Example: `  bdrive restore docs/spec.md              # the previous version
  bdrive restore docs/spec.md --list       # what versions exist
  bdrive restore docs/spec.md a3f9c1e2     # a specific one`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			root, _, err := findProject(filepath.Dir(abs))
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, abs)
			if err != nil || strings.HasPrefix(rel, "..") {
				return fmt.Errorf("%s is outside the project at %s", abs, root)
			}
			rel = filepath.ToSlash(rel)

			// Restoring ends in a sync cycle, so it answers to the same gate
			// `bdrive sync` does: it must not enroll this device or resume a
			// project someone paused. Only `bdrive init` does that.
			proj, ok, err := config.LoadProject(root)
			if err != nil {
				return err
			}
			if !ok {
				return notAProject(root)
			}
			switch syncBlocked(proj) {
			case "init":
				return fmt.Errorf("%s is not synced on this device yet (run `bdrive init` there to connect it)", root)
			case "paused":
				return fmt.Errorf("syncing is paused for %s (run `bdrive init` there to resume)", root)
			}

			sess, _, err := openSession(cmd.Context(), root, true)
			if err != nil {
				return err
			}
			defer closeSession(sess)
			all, err := syncer.LogEntries(sess.Store, "", 0) // newest first
			if err != nil {
				return err
			}
			versions := versionsOf(all, rel)
			if len(versions) == 0 {
				return fmt.Errorf("no history for %s", rel)
			}
			if list {
				printVersions(cmd.OutOrStdout(), versions, currentBlob(all, rel))
				return nil
			}
			want := ""
			if len(args) == 2 {
				want = args[1]
			}
			op, err := pickVersion(versions, currentBlob(all, rel), want)
			if err != nil {
				return err
			}

			note := fmt.Sprintf("restore %s@%s", rel, shortSHA(op.Blob))
			// Persist the note so a daemon that wins the race to scan the file
			// stamps it too — otherwise the restore lands in history unlabeled.
			if err := sess.Store.SaveNote(note, restoreNoteTTL); err != nil {
				return err
			}
			sess.Note = note
			if err := sess.Restore(cmd.Context(), rel, op.Blob); err != nil {
				return err
			}
			if _, err := sess.Cycle(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restored %s to the version from %s (%s, %s)\n",
				rel, op.Time.Local().Format("2006-01-02 15:04:05"), shortSHA(op.Blob), humanBytes(op.Size))
			return nil
		},
	}
	c.Flags().BoolVar(&list, "list", false, "list this file's versions instead of restoring one")
	return c
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// versionsOf returns the puts for exactly this path, newest first.
// LogEntries' own path filter also matches directories and prefixes, so a
// caller that wants one file must re-filter — restoring the wrong file would
// be the worst possible bug in this command.
func versionsOf(all []journal.Op, path string) []journal.Op {
	var out []journal.Op
	for _, op := range all {
		if op.Kind == journal.KindPut && op.Path == path {
			out = append(out, op)
		}
	}
	return out
}

// currentBlob is the content the file holds right now, "" when it is deleted
// (or never existed).
func currentBlob(all []journal.Op, path string) string {
	return journal.Replay(all)[path].Blob
}

// pickVersion resolves the version to restore. want is a (short) content
// hash, or "" for "the one before what the file says now" — which, when the
// file is currently deleted, is simply its last content: that is what makes
// restoring a deleted file work.
func pickVersion(versions []journal.Op, current, want string) (journal.Op, error) {
	if want == "" {
		for _, op := range versions {
			if op.Blob != current {
				return op, nil
			}
		}
		return journal.Op{}, fmt.Errorf("no earlier version to restore — this is the only content this file has had")
	}
	want = strings.ToLower(want)
	var match journal.Op
	blobs := map[string]bool{}
	for _, op := range versions {
		if strings.HasPrefix(op.Blob, want) && !blobs[op.Blob] {
			blobs[op.Blob] = true
			if len(blobs) == 1 {
				match = op
			}
		}
	}
	switch len(blobs) {
	case 0:
		return journal.Op{}, fmt.Errorf("no version of this file starts with %q (try --list)", want)
	case 1:
		return match, nil
	default:
		return journal.Op{}, fmt.Errorf("%q matches %d versions — use more characters", want, len(blobs))
	}
}

func printVersions(w io.Writer, versions []journal.Op, current string) {
	for _, op := range versions {
		who := op.UserName
		if who == "" {
			who = op.User
		}
		if who == "" {
			who = op.Author
		}
		mark := "  "
		if op.Blob == current {
			mark = "* "
		}
		// Same treatment as `bdrive log`: these strings are a peer's.
		fmt.Fprintf(w, "%s%s  %s  %8s  %s on %s\n", mark, shortSHA(op.Blob),
			op.Time.Local().Format("2006-01-02 15:04:05"), humanBytes(op.Size),
			safeField(who, 64), safeField(op.DeviceName, 64))
	}
}
