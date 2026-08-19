package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/daemon"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/secrets"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

func syncCmd() *cobra.Command {
	var note string
	var noteTTL time.Duration
	var hookLabel string
	var prune bool
	c := &cobra.Command{
		Use:   "sync [folder]",
		Short: "Sync a mounted folder with its remote now",
		Long: `Run one sync cycle now: journal local changes, pull teammates' changes,
and push.

--prune additionally reconciles the hub against .bdriveignore: anything the
hub still holds that the ignore rules now exclude is removed from the hub
while staying on disk, here and on every teammate's device. That is the
cleanup path for files that synced before the rule was added.

It refuses outright when .bdriveignore narrows the sync scope with "!"
rules (what bdrive scope and init --only write): there, pruning would mean
removing everything outside the scope from the hub, for the whole team.
Drop specific paths with bdrive forget instead. A legacy per-device include
list in .bdrive/config.json is never pruned against either.`,
		Example: `  bdrive sync
  bdrive sync --prune    # also drop hub files that .bdriveignore now excludes`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			// A folder resolves to the mount it is, the mount above it, or the
			// mounts below it — a repo root with wiki/ and docs/ mounted syncs
			// both, and a session inside a mount syncs its root.
			targets := syncTargets(folder)

			syncOne := func(target string) error {
				// Gate before openSession: hooks fire in every folder on every
				// turn, and must never enroll this device or resume a paused
				// project — that is `bdrive init`'s job alone.
				proj, ok, err := config.LoadProject(target)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("%s is not a beardrive project (run `bdrive init` there first)", target)
				}
				switch syncBlocked(proj) {
				case "init":
					return fmt.Errorf("%s is not synced on this device yet (run `bdrive init` there to connect it)", target)
				case "paused":
					return fmt.Errorf("syncing is paused for %s (run `bdrive init` there to resume)", target)
				}
				sess, proj, err := openSession(cmd.Context(), target, true)
				if err != nil {
					return err
				}
				defer closeSession(sess)
				// Persist the note so the daemon's own scans stamp it too —
				// history then links every change from this working session
				// to its context, not just the ones this invocation catches.
				// Expires after --note-ttl. Unconditional because an explicit
				// `bdrive sync` is a human act: whatever note the last agent
				// session left stops applying here, and empty text clears
				// (SaveNote -> ClearNote), so one call both sets and clears.
				// Clearing the *store* is what leaves this cycle unstamped —
				// scan falls back to LoadNote whenever Session.Note is empty.
				// The --hook branch below has its own SaveNote and keeps the
				// TTL, which is what the TTL is for: the daemon's own scans
				// inside an agent session.
				if err := sess.Store.SaveNote(note, noteTTL); err != nil {
					return err
				}
				sess.Note = note
				sess.Prune = prune
				sess.OnProgress = progressReporter()
				res, err := sess.Cycle(cmd.Context())
				if err != nil {
					return err
				}
				fmt.Printf("synced %s (project %q)\n", target, proj.Volume)
				printCycle(res)
				return nil
			}

			if hookLabel != "" {
				// Agent-hook mode: event JSON on stdin, silent best-effort
				// sync, link-formula context on stdout. Never fails. Every
				// mount contributes its own prefix→URL pair; the JSON
				// contract is one object, so they are emitted together after
				// the loop.
				sessionID := hookSessionID(cmd)
				var links []hookLink
				for _, target := range targets {
					proj, ok, err := config.LoadProject(target)
					if err != nil || !ok || syncBlocked(proj) != "" {
						continue
					}
					if h, ok := runHookSync(cmd, target, sessionID, hookLabel); ok {
						link := hookLinkFor(folder, target, h.base)
						link.paths = h.paths
						link.secrets = h.secrets
						links = append(links, link)
					}
				}
				emitHookContext(cmd, links)
				return nil
			}
			if len(targets) == 0 {
				return fmt.Errorf("%s is not a beardrive project (run `bdrive init` there first)", folder)
			}
			if prune {
				for _, target := range targets {
					if err := pruneSafe(target); err != nil {
						return err
					}
				}
			}
			for _, target := range targets {
				if err := syncOne(target); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&prune, "prune", false, "also remove from the hub what .bdriveignore now excludes (files stay on disk everywhere)")
	c.Flags().StringVar(&note, "note", "", "session context stamped onto changes (e.g. an agent session id); shown in history; empty clears")
	c.Flags().DurationVar(&noteTTL, "note-ttl", 30*time.Minute, "how long the note keeps applying to daemon-committed changes")
	c.Flags().StringVar(&hookLabel, "hook", "", "agent-hook mode: read the platform's hook event JSON from stdin, sync with a session note labeled by this value, and emit the project's link-formula context (Claude Code hook JSON) on stdout")
	return c
}

// pruneSafe refuses --prune on a mount whose rules narrow the scope. Prune
// removes from the hub everything the shared rules exclude — with "only
// these folders" rules that is everything else the project holds, deleted
// for every teammate on their next sync. Excluding one path is what
// `bdrive forget` is for.
func pruneSafe(folder string) error {
	filter, err := syncer.LoadFilter(folder, nil)
	if err != nil {
		return err
	}
	if !filter.Negated() {
		return nil
	}
	return fmt.Errorf("%s/.bdriveignore narrows the scope with `!` rules, so --prune would remove\n"+
		"everything outside that scope from the hub — for every teammate, not just this device.\n"+
		"drop specific paths with `bdrive forget <path>`, or widen the rules first", folder)
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [folder]",
		Short: "Show mount, sync, and daemon status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mounts, err := config.LoadMounts()
			if err != nil {
				return err
			}
			if len(args) > 0 {
				folder, err := absFolder(args)
				if err != nil {
					return err
				}
				proj, err := mustProject(folder) // also self-heals the registry
				if err != nil {
					return err
				}
				mounts = map[string]config.MountInfo{proj.ID: {Path: folder, Volume: proj.Volume, Remote: proj.Remote}}
			}
			if len(mounts) == 0 {
				fmt.Println("no beardrive projects on this device (run `bdrive init` in a folder)")
				return nil
			}
			dev, err := config.LoadDevice()
			if err != nil {
				return err
			}
			if settings, _ := config.LoadSettings(); settings.Email != "" {
				who := settings.Email
				if settings.Name != "" {
					who = settings.Name + " <" + settings.Email + ">"
				}
				// The account name/email come from the hub, like the rows below.
				fmt.Printf("device: %s (%s) signed in as %s\n\n", dev.Name, dev.ID, safeField(who, 160))
			} else {
				fmt.Printf("device: %s (%s) as %s\n\n", dev.Name, dev.ID, dev.Author)
			}
			first := true
			for id, mi := range mounts {
				if !first {
					fmt.Println()
				}
				first = false
				folder := mi.Path
				var include []string
				if proj, ok, err := config.LoadProject(folder); err == nil && ok {
					mi.Volume, mi.Remote = proj.Volume, proj.Remote // folder config wins
					include = proj.Include
				} else {
					fmt.Printf("%s\n  (folder missing — moved or deleted; run `bdrive init` at its new location)\n", folder)
					continue
				}
				fmt.Printf("%s\n", folder)
				// Volume and Remote come out of .bdrive/config.json, and
				// Volume comes originally from the hub's project name — any
				// org member's string, reaching a terminal. Same treatment as
				// `bdrive log`'s rows.
				fmt.Printf("  project:  %s (%s)\n", safeField(mi.Volume, 120), id)
				if mi.Remote != "" {
					fmt.Printf("  remote:   %s\n", safeField(mi.Remote, 200))
				} else {
					fmt.Printf("  remote:   (none — local only)\n")
				}
				vdir, err := config.VolumeDir(id)
				if err != nil {
					return err
				}
				if pid, ok := daemon.Running(vdir); ok {
					fmt.Printf("  daemon:   running (pid %d)\n", pid)
				} else {
					fmt.Printf("  daemon:   stopped\n")
				}
				sess, _, err := openSession(cmd.Context(), folder, false)
				if err != nil {
					continue
				}
				cache, cacheErr := sess.Store.LoadCache(id)
				if cacheErr == nil {
					var total int64
					for _, c := range cache {
						total += c.Size
					}
					fmt.Printf("  files:    %d (%s)\n", len(cache), humanBytes(total))
				}
				// Read, never scanned: `status` runs no cycle, so what it
				// reports is what the last cycle that read those bytes found.
				if found, err := sess.Store.LoadSecrets(id); err == nil {
					printSecrets(found)
				}
				st, err := sess.Store.LoadSync()
				myOps, err2 := sess.Store.DeviceOps(dev.ID)
				if err == nil && err2 == nil {
					pending := int64(len(myOps)) - st.PushedOps
					if pending < 0 {
						pending = 0
					}
					fmt.Printf("  pending:  %d local change(s) not yet pushed\n", pending)
					// `pending` counts what the journal holds; it says nothing
					// about the folder, so with the daemon stopped an edit
					// nobody has scanned yet is in neither. Drift is that
					// second, separate state — a read-only walk, no ops, no
					// journal, no hub. It degrades to no line rather than
					// failing the command.
					if cacheErr == nil {
						if added, modified, gone, dErr := syncer.Drift(folder, include, st.IgnoreAccepted, cache); dErr == nil {
							fmt.Printf("  local:    %d change(s) not yet scanned (%d new, %d edited, %d removed)\n",
								added+modified+gone, added, modified, gone)
						}
					}
					switch st.Access {
					case store.AccessReadOnly:
						fmt.Printf("  access:   read-only (pull only) — %d local change(s) stay on this device\n", pending)
					case store.AccessNone:
						fmt.Printf("  access:   no access to this project — sync paused\n")
					}
					// `status` is the command someone runs when sync is stuck, and
					// it never talks to the hub — so the refusal it reports is only
					// as useful as the reason the last cycle recorded with it.
					if st.AccessReason != "" {
						fmt.Printf("  reason:   %s\n", safeField(st.AccessReason, 300))
					}
				}
			}
			return nil
		},
	}
}

// writeGap is how far a file's write time must lag the moment it was journaled
// before `bdrive log` prints both.
const writeGap = time.Minute

func logCmd() *cobra.Command {
	var limit int
	var pathFilter string
	c := &cobra.Command{
		Use:   "log [folder]",
		Short: "Show change history: who changed which file, when, on which device",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			sess, _, err := openSession(cmd.Context(), folder, false)
			if err != nil {
				return err
			}
			// Limit after the display sort, not before: -n 25 means the 25
			// newest by the time shown, not the 25 highest lamport.
			entries, err := syncer.LogEntries(sess.Store, pathFilter, 0)
			if err != nil {
				return err
			}
			syncer.SortForDisplay(entries)
			if limit > 0 && len(entries) > limit {
				entries = entries[:limit]
			}
			out := cmd.OutOrStdout()
			if len(entries) == 0 {
				fmt.Fprintln(out, "no history yet")
				return nil
			}
			for _, op := range entries {
				commit := syncer.CommitTime(op)
				when := commit.Local().Format("2006-01-02 15:04:05")
				kind := op.Kind
				if kind == journal.KindPut {
					kind = "put   "
				} else {
					kind = "delete"
				}
				// Prefer the signed-in account over the git/OS author fallback,
				// so team history shows hub identities.
				who := op.UserName
				if who == "" {
					who = op.User
				}
				if who == "" {
					who = op.Author
				}
				line := fmt.Sprintf("%s  %s  %-40s  %s on %s", when, kind,
					safeField(op.Path, 160), safeField(who, 64), safeField(op.DeviceName, 64))
				if op.Kind == journal.KindPut {
					line += fmt.Sprintf("  (%s)", humanBytes(op.Size))
				}
				// The first column is when the change was journaled, so the
				// rows read monotonically. The file's own write time still
				// matters — but the daemon scans every 3s and a hook sync is
				// prompt, so a sub-minute gap is just scan latency and would be
				// noise on every row. A larger gap means the file genuinely
				// predates its arrival here — a rename, or an old document
				// added today — and that is the case the reader has to see.
				// Deletes have no file left to stat, so they never carry it.
				if written := syncer.DisplayTime(op); commit.Sub(written) >= writeGap {
					line += fmt.Sprintf("  (written %s)", written.Local().Format("2006-01-02 15:04:05"))
				}
				if note := safeField(op.Note, 200); note != "" {
					line += "  [" + note + "]"
				}
				fmt.Fprintln(out, line)
			}
			return nil
		},
	}
	c.Flags().IntVarP(&limit, "limit", "n", 50, "max entries to show (0 = all)")
	c.Flags().StringVarP(&pathFilter, "path", "p", "", "only show history for this file or directory")
	return c
}

// safeField prepares a string that came out of a peer's journal for a terminal
// row. Every string `bdrive log` and `bdrive restore --list` print — Path,
// Note, User, UserName, Author, DeviceName — is arbitrary JSON someone else
// wrote, and a terminal executes what it is handed: ESC sequences repaint
// rows, clear the scrollback, set the window title, write the system clipboard
// (OSC 52) and, through DECRQSS/CPR, make some emulators type a reply onto the
// shell; a lone CR redraws the row that was just printed as something else; a
// newline forges a whole entry. The audit tool an operator uses to catch a
// peer must not be renderable BY that peer.
//
// So: no C0 or DEL, one entry is one line, and each part is bounded — 50 rows
// of log is also owned by one 40 KB entry that scrolls the rest away.
// statusSecretsMax caps the block: a folder that trips the rules everywhere
// would otherwise bury the rest of `status` under its own output.
const statusSecretsMax = 20

// printSecrets renders what the last cycle that read these files found. The
// wording is the same contract the share gate has: each file was checked WHEN
// IT CHANGED, which says nothing about a file that has not. Rule id and line
// only — the matched bytes never leave internal/secrets.
func printSecrets(found map[string][]secrets.Finding) {
	if len(found) == 0 {
		return
	}
	paths := slices.Sorted(maps.Keys(found))
	fmt.Printf("  secrets: %d file(s) looked like they contain credentials when they last changed\n", len(paths))
	for i, rel := range paths {
		if i == statusSecretsMax {
			fmt.Printf("             +%d more\n", len(paths)-i)
			break
		}
		for _, f := range found[rel] {
			// The path is a project member's string reaching a terminal,
			// exactly like the rows above.
			fmt.Printf("             %s:%d  %s\n", safeField(rel, 160), f.Line, secrets.Label(f.Rule))
		}
	}
}

func safeField(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f:
			return -1
		// C1, U+0080..U+009F. In a UTF-8 terminal these arrive as two bytes
		// and xterm and its descendants decode them straight back to 8-bit
		// controls: U+009B IS CSI, U+009D IS OSC, U+0090 IS DCS, U+0085 IS
		// NEL. The whole escape vocabulary, with no ESC byte anywhere.
		case r >= 0x80 && r <= 0x9f:
			return -1
		// Every format character (category Cf) plus the tag block, as a
		// CLASS. The bidirectional controls this used to enumerate (Trojan
		// Source, CVE-2021-42574) are Cf: not control characters by Unicode's
		// own definition, so they survive every C0/C1 filter, and one U+202E
		// draws the rest of the row right-to-left — the columns naming the
		// actor and the device come after the path on the same line.
		//
		// The class, not the list, for the reason journal.SafeText and
		// webapp.trimText arrived at the same rule in round 13: an enumeration
		// grows by neighbours and misses the rest. U+E0020..U+E007F encodes all
		// of printable ASCII with no glyph at all, and this output is read by
		// agents as often as by people — `bdrive status` and `bdrive log` land
		// in a session's context verbatim.
		case unicode.Is(unicode.Cf, r), r >= 0xe0000 && r <= 0xe01ef:
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "…"
	}
	return s
}

func daemonCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    "daemon",
		Short:  "Manage the background sync daemon",
		Hidden: true,
	}
	var scanInterval, remoteInterval time.Duration
	run := &cobra.Command{
		Use:   "run <folder>",
		Short: "Run the sync daemon in the foreground (internal)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			return daemon.Run(folder, scanInterval, remoteInterval)
		},
	}
	run.Flags().DurationVar(&scanInterval, "scan-interval", 3*time.Second, "local scan interval")
	run.Flags().DurationVar(&remoteInterval, "remote-interval", 10*time.Second, "remote sync interval")
	c.AddCommand(run)
	return c
}
