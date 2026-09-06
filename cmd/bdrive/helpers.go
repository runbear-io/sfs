package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mattn/go-isatty"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// repoURL is the project's public home — the one place the CLI points at
// GitHub, from `init`'s success block.
const repoURL = "https://github.com/runbear-io/beardrive"

// stdinIsTTY is the one answer to "is this an interactive shell?" — used both
// to decide whether init may prompt and whether login can drive a browser.
func stdinIsTTY() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
}

func absFolder(args []string) (string, error) {
	arg := "."
	if len(args) > 0 {
		arg = args[0]
	}
	return filepath.Abs(arg)
}

// findMountRoot walks up from folder to the nearest mount root. An agent
// session usually starts wherever the user opened their editor, which may be
// inside a synced subfolder rather than at its root.
func findMountRoot(folder string) (string, bool) {
	for cur := folder; ; cur = filepath.Dir(cur) {
		if config.IsMount(cur) {
			return cur, true
		}
		if filepath.Dir(cur) == cur {
			return "", false
		}
	}
}

// mountsUnder lists the registered mount roots at or below folder, sorted.
// The registry is the only thing that knows about mounts a walk up can't see.
func mountsUnder(folder string) []string {
	mounts, err := config.LoadMounts()
	if err != nil {
		return nil
	}
	root := resolvePath(folder) + string(filepath.Separator)
	var out []string
	for _, mi := range mounts {
		if mi.Path != "" && strings.HasPrefix(resolvePath(mi.Path)+string(filepath.Separator), root) {
			out = append(out, mi.Path)
		}
	}
	sort.Strings(out)
	return out
}

// resolvePath expands symlinks so paths that name the same directory compare
// equal: a mount registered as /private/tmp/x must still be found from a
// session in /tmp/x (macOS symlinks /tmp, /var, and /etc).
func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// syncTargets resolves which mount roots a sync at folder covers: the folder
// itself, else the mount enclosing it (session started inside a synced
// subfolder), else every mount below it (session started at a repo root whose
// wiki/ and docs/ are the mounts). Without this, turn hooks fire only when the
// session's directory happens to be exactly the mount root.
func syncTargets(folder string) []string {
	if config.IsMount(folder) {
		return []string{folder}
	}
	if root, ok := findMountRoot(folder); ok {
		return []string{root}
	}
	return mountsUnder(folder)
}

// workspaceRootUnder reports a workspace root strictly below folder, if this
// device syncs a project inside one. A root's projects are its immediate
// children and the registry knows where every mount is, so this is one stat
// per registered mount under folder — no walk down the user's disk.
//
// Registry only, and deliberately: it asks about paths this device already
// syncs, a small known set. Scanning the folder's children instead — tried,
// and it made `bdrive init` hang forever on one FIFO or one wedged network
// child, which is the same hazard that had to be removed from startSync.
// A guard that can hang the command it guards is worse than the gap it closes.
//
// The gap: a root whose projects this device never enrolled (a tree copied
// from another machine) is invisible here, so `bdrive init` above it is not
// refused. Stated in DESIGN.md.
func workspaceRootUnder(folder string) (string, bool) {
	self := resolvePath(folder)
	for _, m := range mountsUnder(folder) {
		// mountsUnder includes folder itself when folder IS a mount, and its
		// parent is then the root this project legitimately lives in —
		// `bdrive init` in a project under a root must keep working, which is
		// the whole shipping layout.
		if resolvePath(m) == self {
			continue
		}
		// Every level between the mount and folder, not just the mount's own
		// parent: a project need not sit directly under its root
		// (<root>/team/wiki is a mount whose root is two levels up), and
		// checking only the parent let `bdrive init` above such a root
		// through — which then synced the folders the root exists to hold
		// apart to the whole team.
		//
		// Bounded by the depth between a path this device syncs and a folder
		// the user just named, both of which the command is about to walk
		// anyway. That is why this is acceptable here and the same shape is
		// not acceptable in the desktop connect (config.CheckRootPlacement).
		// Walk the RESOLVED mount path: a mount registered by a symlinked
		// spelling walks up a different chain than the one `folder` names, so
		// the "stop at folder" test never fires and the walk continues past
		// it — reporting a root ABOVE the named folder as one beneath it.
		for cur := filepath.Dir(resolvePath(m)); resolvePath(cur) != self; cur = filepath.Dir(cur) {
			if config.HasManifest(cur) {
				return cur, true
			}
			if filepath.Dir(cur) == cur {
				break
			}
		}
	}
	return "", false
}

// notAProject is the "this folder has no project" error every command shares,
// including the one case where the usual advice is wrong: at a workspace root
// `bdrive init` refuses on purpose, so sending the user there is a dead end.
func notAProject(folder string) error {
	if config.HasManifest(folder) {
		return fmt.Errorf("%s is a BearDrive workspace root: it holds your projects and never syncs itself\n"+
			"run this in one of the project folders inside it", folder)
	}
	return fmt.Errorf("%s is not a beardrive project (run `bdrive init` there first)", folder)
}

// mustProject resolves a folder's project settings (from .bdrive/config.json,
// self-healing the registry when the folder moved).
func mustProject(folder string) (config.Project, error) {
	proj, found, err := config.ResolveMount(folder)
	if err != nil {
		return proj, err
	}
	if !found {
		return proj, notAProject(folder)
	}
	if proj.Volume == "" {
		proj.Volume = filepath.Base(folder)
	}
	return proj, nil
}

// syncBlocked reports why syncing must not run for a project on this device:
// "init" when the mount was never enrolled here (.bdrive/config.json travels
// with the folder — e.g. arrives in a git clone — so its presence alone is
// not consent to sync; only `bdrive init` enrolls a device), "paused" after
// `bdrive stop`, "" to proceed. Deliberately reads the registry without
// ResolveMount's self-heal, which would enroll as a side effect.
func syncBlocked(proj config.Project) string {
	mounts, err := config.LoadMounts()
	if err != nil {
		return "init"
	}
	if _, enrolled := mounts[proj.ID]; !enrolled {
		return "init"
	}
	if vdir, err := config.VolumeDir(proj.ID); err == nil && store.Paused(vdir) {
		return "paused"
	}
	return ""
}

// openSession builds a syncer session for a project folder. When withRemote
// is set and the remote is unreachable, it degrades to offline with a warning
// rather than failing.
func openSession(ctx context.Context, folder string, withRemote bool) (*syncer.Session, config.Project, error) {
	proj, err := mustProject(folder)
	if err != nil {
		return nil, proj, err
	}
	dev, err := config.LoadDevice()
	if err != nil {
		return nil, proj, err
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		return nil, proj, err
	}
	st, err := store.Open(vdir)
	if err != nil {
		return nil, proj, err
	}
	settings, _ := config.LoadSettings()
	sess := &syncer.Session{Folder: folder, MountID: proj.ID, Store: st, Device: dev, Account: settings}
	if withRemote && proj.Remote != "" {
		be, err := remote.Open(ctx, proj.Remote)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: remote unavailable, working offline: %v\n", err)
		} else {
			sess.Backend = be
		}
	}
	return sess, proj, nil
}

func closeSession(sess *syncer.Session) {
	if sess != nil && sess.Backend != nil {
		sess.Backend.Close()
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// printReason prints the hub's stated reason under a refusal line. "read-only"
// is a summary of the STATUS code; the sentence under it is the only thing that
// tells a device-registration 403 apart from a project the user really is a
// reader on — and only one of those is fixed by signing in again.
func printReason(reason string) {
	if reason != "" {
		fmt.Printf("                  %s\n", safeField(reason, 300))
	}
}

func printCycle(res *syncer.Result) {
	fmt.Printf("  local changes:  %d\n", res.LocalOps)
	fmt.Printf("  pulled changes: %d\n", res.PulledOps)
	if res.Conflicts > 0 {
		fmt.Printf("  conflicts:      %d (preserved as *.bdrive-conflict-* files)\n", res.Conflicts)
	}
	if res.Adopted > 0 {
		fmt.Printf("  adopted:        %d file(s) replaced by the project's version (yours: `bdrive restore --list <path>`)\n", res.Adopted)
	}
	if res.Pruned > 0 {
		fmt.Printf("  pruned:         %d path(s) removed from the hub (kept on disk)\n", res.Pruned)
	}
	// Named, not counted. A reverted edit is the one outcome where the user's
	// own change did not survive the cycle, so "which file?" is the first
	// thing they need — and the copy beside it is where their bytes went.
	if n := len(res.Reverted); n > 0 {
		fmt.Printf("  read-only:      %d file(s) reverted — you may read these folders but not change them\n", n)
		for i, rel := range res.Reverted {
			if i == 5 {
				fmt.Printf("                  ... and %d more\n", n-i)
				break
			}
			fmt.Printf("                  %s (your version kept beside it)\n", safeField(rel, 200))
		}
	}
	fmt.Printf("  files updated:  %d\n", res.Materialized)
	switch {
	case res.NoAccess:
		fmt.Printf("  remote:         no access — sync paused (ask a project admin for access)\n")
		printReason(res.Reason())
	case res.ReadOnly:
		fmt.Printf("  remote:         read-only (pull only) — local changes stay on this device\n")
		printReason(res.Reason())
	case res.Pushed && res.OfflineErr != nil:
		// Offline is a report, not a gate (see syncer.Result): a content-level
		// problem with one object no longer withholds this device's push, so
		// "pushed" and a remote warning are both true.
		fmt.Printf("  remote:         pushed, with a warning: %v\n", res.OfflineErr)
	case res.Pushed:
		fmt.Printf("  remote:         pushed\n")
	case res.Offline:
		fmt.Printf("  remote:         offline (%v)\n", res.OfflineErr)
	}
}
