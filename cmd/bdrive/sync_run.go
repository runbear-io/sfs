package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/daemon"
	"github.com/runbear-io/beardrive/internal/store"
)

// startSync brings a project folder live: register the mount, open the
// volume store, run the initial cycle, start the background daemon. Called
// by `bdrive init` (and by anything that needs to resume a stopped project).
// errDaemonStart marks the ONE startSync failure that leaves a working mount
// behind: the folder is enrolled, the project exists and the first cycle ran
// — only the background daemon did not come up, which the next bdrive command
// in the folder heals. A caller that already created files (the desktop
// connect step) must report it, never unwind over it.
var errDaemonStart = errors.New("start sync daemon")

func startSync(ctx context.Context, folder string, proj config.Project, foreground bool, scanInterval, remoteInterval time.Duration) error {
	// EnrollMount, not ResolveMount: creating the registry row is the
	// enrollment gesture and startSync is `bdrive init`'s path, the one place
	// that may do it. Every other command resolves without enrolling.
	if _, _, err := config.EnrollMount(folder); err != nil {
		return err
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		return err
	}
	if _, err := store.Open(vdir); err != nil {
		return err
	}
	// init is the one gesture that (re)consents to syncing: clear any pause
	// left by `bdrive stop` so the daemon and the agent hooks run again.
	if err := store.SetPaused(vdir, false); err != nil {
		return err
	}

	// Initial cycle: import existing files, pull remote state.
	sess, _, err := openSession(ctx, folder, true)
	if err != nil {
		return err
	}
	sess.OnProgress = progressReporter() // the initial import is the slow one
	res, err := sess.Cycle(ctx)
	closeSession(sess)
	if err != nil {
		return err
	}
	printCycle(res)

	if foreground {
		return daemon.Run(folder, scanInterval, remoteInterval)
	}
	pid, err := daemon.Start(folder, vdir, scanInterval, remoteInterval)
	if err != nil {
		return fmt.Errorf("%w: %w", errDaemonStart, err)
	}
	fmt.Printf("  daemon:  running (pid %d, scan %s, remote sync %s)\n", pid, scanInterval, remoteInterval)
	return nil
}

// stopCmd pauses syncing for a project folder (files stay on disk; run
// `bdrive init` again to resume).
func stopCmd() *cobra.Command {
	var forget bool
	c := &cobra.Command{
		Use:     "stop [folder]",
		Aliases: []string{"pause"},
		Short:   "Stop syncing a project folder",
		Long: `Stop the sync daemon for a project folder. Files stay on disk and the
project's local history is kept; run "bdrive init" in the folder to resume.

With --forget the mount is also removed from this device's registry (the
folder's .bdrive settings and the local volume data are kept).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			proj, err := mustProject(folder)
			if err != nil {
				return err
			}
			vdir, err := config.VolumeDir(proj.ID)
			if err != nil {
				return err
			}
			stopped, err := daemon.Stop(vdir)
			if err != nil {
				return err
			}
			// The pause must outlive the daemon: agent turn hooks run
			// `bdrive sync` in this folder on every turn and would silently
			// resume without it. Cleared by `bdrive init`.
			if err := store.SetPaused(vdir, true); err != nil {
				return err
			}
			if stopped {
				fmt.Printf("stopped syncing %s (run `bdrive init` to resume)\n", folder)
			} else {
				fmt.Printf("no sync daemon running for %s; syncing paused (run `bdrive init` to resume)\n", folder)
			}
			if forget {
				mounts, err := config.LoadMounts()
				if err != nil {
					return err
				}
				delete(mounts, proj.ID)
				if err := config.SaveMounts(mounts); err != nil {
					return err
				}
				fmt.Printf("forgot mount %s (local volume data kept under ~/.bdrive/volumes/%s)\n", proj.ID, proj.ID)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&forget, "forget", false, "also remove the mount from this device's registry")
	return c
}
