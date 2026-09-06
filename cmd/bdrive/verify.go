package main

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// errVerifyProblems is verify's "something is wrong" exit, not a failure of
// the command: the convention is status 1 with the findings already printed,
// so `bdrive verify || ...` composes in a script or a pre-flight check. Real
// errors (not a project, bad remote) still print — see verifyCmd.
var errVerifyProblems = errors.New("verify found problems")

// verifyMaxList caps how many paths are listed per category. The counts are
// always exact; only the listing is bounded, so a project that drifted whole
// does not scroll the finding off the screen.
const verifyMaxList = 20

func verifyCmd() *cobra.Command {
	var checkRemote bool
	c := &cobra.Command{
		Use:   "verify [folder]",
		Short: "Prove this folder matches the content the journal records",
		Long: `Hash every file this project syncs and compare it to the journal.

"Your folder is the same everywhere" is the claim; this is the receipt.
` + "`bdrive status`" + ` counts pending ops and unscanned changes but never reads a
byte of content — so a file whose bytes changed while its size and mtime
stayed put is invisible to it. verify hashes, which is the whole point: there
is deliberately no size/mtime fast path, and on a large project it is
noticeably slower than status.

It reports five things:

  drifted          on disk, but not the content the journal records
  never-pushed     committed here, never reached the hub
  missing-locally  the journal has it, this folder does not
  not-yet-scanned  on disk, with no op anywhere yet
  missing-on-hub   synced here, absent from the hub (--remote only)

It is a pure read: no daemon, no lock, no ops, no journal writes, and without
--remote no network at all — so it works offline and never blocks on a sync in
progress. It never repairs anything; run ` + "`bdrive sync`" + ` for that.

Exit status is 0 when every category is empty and 1 when any is not.

One caveat it prints for itself: the journals it replays are this device's
local copies, so without a pull it proves "this folder matches what I last
pulled" — a teammate's newer op is invisible until the next cycle.`,
		Example: `  bdrive verify            # hash the folder, compare to the journal
  bdrive verify --remote   # also ask the hub whether it still holds the content
  bdrive verify ~/wiki     # a project other than the current folder`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runVerify(cmd, args, checkRemote)
			// SilenceErrors below is for errVerifyProblems alone, so anything
			// else has to print itself — cobra no longer will.
			if err != nil && !errors.Is(err, errVerifyProblems) {
				fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
			}
			return err
		},
	}
	// Silenced so a findings exit is status 1 with the findings and nothing
	// else — errVerifyProblems is a status, not a failure, and cobra would
	// otherwise render it as an error and a usage block. Same mechanism
	// `bdrive grep` uses for its no-match exit.
	c.SilenceErrors = true
	c.SilenceUsage = true
	c.Flags().BoolVar(&checkRemote, "remote", false, "also ask the hub whether it still holds every blob (one check per blob)")
	return c
}

func runVerify(cmd *cobra.Command, folderArg []string, checkRemote bool) error {
	folder, err := absFolder(folderArg)
	if err != nil {
		return err
	}
	// LoadProject, not ResolveMount: ResolveMount self-heals the registry
	// path, i.e. it enrolls this device. A read-only query must not have that
	// side effect — the same rule grep and stale follow.
	proj, found, err := config.LoadProject(folder)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s is not a beardrive project (run `bdrive init` there first)", folder)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "  project:  %s (%s)\n", safeField(proj.Volume, 120), proj.ID)

	// Stat-guarded, because store.Open MkdirAlls and a read must not create a
	// volume for a project that has never synced; unlocked, because store.Open
	// takes no volume flock — a running daemon never blocks this.
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		return err
	}
	if !dirExists(vdir) {
		fmt.Fprintln(out, "  nothing synced yet — no local history to verify against")
		return nil
	}
	st, err := store.Open(vdir)
	if err != nil {
		return err
	}

	var be remote.Backend
	if checkRemote {
		if proj.Remote == "" {
			return errors.New("--remote needs a hub: this project is local only")
		}
		// remote.Open directly, not openSession: openSession goes through
		// mustProject, which is the enrolling call this command must not make.
		// It picks up the device token itself, so there is nothing to wire.
		b, oerr := remote.Open(cmd.Context(), proj.Remote)
		if oerr != nil {
			// Unreachable hub is a warning, not a failure: the local verdict
			// is still the answer, and this is the repo's "never break on the
			// remote" posture.
			fmt.Fprintf(out, "  warning:  hub unreachable, local check only (%s)\n", safeField(oerr.Error(), 200))
		} else {
			be = b
			defer be.Close()
		}
	}

	// Best-effort: without an identity the never-pushed count is simply
	// empty, which is a worse answer than the others but not a reason to fail
	// a read.
	dev, _ := config.LoadDevice()

	rep, err := syncer.Verify(cmd.Context(), folder, proj.Include, st, dev.ID, be)
	if err != nil {
		return err
	}
	return printVerify(out, rep)
}

func printVerify(out io.Writer, rep syncer.VerifyReport) error {
	fmt.Fprintf(out, "  checked:  %s, %s hashed in %s\n",
		plural(rep.Files, "file"), humanBytes(rep.Bytes), rep.Elapsed.Round(time.Millisecond))
	if rep.RemoteErr != nil {
		fmt.Fprintf(out, "  warning:  hub check incomplete, local result stands (%s)\n", safeField(rep.RemoteErr.Error(), 200))
	}

	missingNote := "the journal has it, this folder does not"
	if rep.NotFetched > 0 {
		missingNote = fmt.Sprintf("%d not fetched yet — run `bdrive sync`", rep.NotFetched)
	}
	cats := []struct {
		name, note string
		paths      []string
	}{
		{"drifted", "on disk, but not the content the journal records", rep.Drifted},
		{"never-pushed", "committed here, never reached the hub", rep.NeverPushed},
		{"missing-locally", missingNote, rep.MissingLocally},
		{"not-yet-scanned", "on disk, with no op anywhere yet", rep.NotYetScanned},
		{"missing-on-hub", "synced here, absent from the hub", rep.MissingOnHub},
	}
	for _, c := range cats {
		if len(c.paths) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n  %s (%d) - %s\n", c.name, len(c.paths), c.note)
		shown := c.paths
		if len(shown) > verifyMaxList {
			shown = shown[:verifyMaxList]
		}
		for _, p := range shown {
			// Every path in missing-locally and missing-on-hub comes out of a
			// PEER's journal — a string someone else chose. safeField, the
			// same treatment `bdrive log` and `bdrive grep` give journal
			// strings, or a lone CR repaints the row.
			fmt.Fprintf(out, "    %s\n", safeField(p, 160))
		}
		if len(c.paths) > len(shown) {
			fmt.Fprintf(out, "    ... and %d more\n", len(c.paths)-len(shown))
		}
	}

	if n := rep.Problems(); n > 0 {
		fmt.Fprintf(out, "\n  %s. Run `bdrive sync` to reconcile, or `bdrive log <path>` for history.\n", plural(n, "problem"))
		return errVerifyProblems
	}
	fmt.Fprintln(out, "  OK - the folder matches the journal")
	// Said out loud, not just documented: the journals replayed here are this
	// device's local copies, so a clean verdict is about what was last pulled.
	// Without this line the command overclaims exactly the thing it exists to
	// prove.
	fmt.Fprintln(out, "  (this compares against what this device last pulled — run `bdrive sync` first for the hub's latest)")
	return nil
}
