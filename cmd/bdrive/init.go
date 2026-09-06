package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/agenthooks"
	"github.com/runbear-io/beardrive/internal/autostart"
	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
	"github.com/runbear-io/beardrive/internal/templates"
)

// starterIgnore is seeded into new projects so build artifacts, dependency
// trees and large binaries don't flood the sync. Users edit it freely; it
// syncs to every device like a normal file.
//
// Deliberately aggressive: every version of every file is retained forever,
// so a big binary checked in once is paid for forever, on every device that
// ever syncs the project. Anything genuinely wanted back is one deleted line
// away, which is much cheaper than the reverse.
const starterIgnore = `# bdrive ignore rules (gitignore-style). This file syncs across devices.

# Dependency trees and build output
node_modules/
dist/
build/
target/
out/
coverage/
__pycache__/
*.pyc
.venv/
venv/
.next/
.cache/

# (.git/ and .bdrive/ are never synced — that is built in, not a rule here,
# so removing a line cannot turn it on.)

# Large binaries. Sync the document, link the video.
*.mp4
*.mov
*.avi
*.mkv
*.iso
*.dmg
*.zip
*.tar.gz

# macOS library/junk
Library/
.DS_Store

# Local-only
*.log
.env
.env.*
`

// warnBigFolder prints a warning when the folder about to sync is far larger
// than anything a shared knowledge project should be, and says how to narrow
// it. Advice only — it never blocks init and never fails it.
//
// The thresholds are guardrails, not policy: they exist to catch the "pointed
// bdrive at my home directory" mistake while it is still cheap to undo, since
// every version is retained forever and each new device pulls the whole
// history.
const (
	bigFolderBytes = 1 << 30 // 1 GiB
	bigFolderFiles = 20_000
)

func warnBigFolder(folder string, include []string) {
	files, bytes, err := syncer.Measure(folder, include)
	if err != nil || (bytes < bigFolderBytes && files < bigFolderFiles) {
		return
	}
	fmt.Printf("\n⚠ this folder would sync %s across %s.\n", humanSize(bytes), plural(files, "file"))
	fmt.Printf("  Every version is kept forever and every new device pulls all of it.\n")
	fmt.Printf("  If that's more than you meant to share:\n")
	fmt.Printf("    bdrive scope add <dir>    sync only certain folders\n")
	fmt.Printf("    bdrive scope --explain    show what is and isn't syncing\n")
	fmt.Printf("  or add patterns to .bdriveignore.\n\n")
}

// humanSize renders a byte count for the warning above.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// initCmd is the front door: sign in if needed, create or connect a project,
// choose what syncs, and start syncing — one command, interactive on a TTY,
// fully flag-driven for scripts and agents. Re-running it in an initialized
// folder just resumes syncing (which is also how a moved/renamed folder
// picks up where it left off).
func initCmd() *cobra.Command {
	var projectID, projectName, serverURL, template string
	var only []string
	var yes, foreground, noHooks, noAutostart bool
	c := &cobra.Command{
		Use:   "init [folder]",
		Short: "Start syncing a project in this folder",
		Long: `Initiate a new project (or connect an existing one) in a folder and start
syncing it through your bdrive server.

The mount is always exactly the folder you name: ` + "`bdrive init wiki`" + ` syncs
./wiki and nothing else, and that folder's contents are the project's
contents. To sync a folder but hold back parts of it, use --only (or
` + "`bdrive scope`" + ` later), which writes ordinary .bdriveignore rules — there is
no separate scope setting.

On a terminal, init asks what you want: create a new project or connect an
existing one, and whether to sync the whole folder or only some subfolders.
Flags answer those questions non-interactively; without a TTY init never
prompts (it creates-or-joins a project named after the folder and syncs the
whole folder).

If this device isn't signed in yet, init runs the login flow first
(default server: ` + config.DefaultServer + `; change it with bdrive login <url>).

Re-running init in an initialized folder resumes syncing — including after
the folder was renamed or moved.`,
		Example: `  bdrive init                        # interactive
  bdrive init wiki                   # ./wiki is the project
  bdrive init ./notes --name shared-notes
  bdrive init --project 7f3a2c91-4d5e-4b8a-9c17-2ad0f6b3e9c4   # connect an existing project
  bdrive init --project 7f3a2c91-4d5e-4b8a-9c17-2ad0f6b3e9c4 --server https://hub.example.com
  bdrive init . --only wiki,docs     # this folder, but only ./wiki and ./docs sync
  bdrive init --yes                  # accept all defaults (no prompts)`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			// The beardrive home holds this device's hub credential
			// (settings.json), every project's journals and its cached blobs.
			// The reserved-directory rule only covers segments BELOW a mount
			// root, so a mount that IS the home makes settings.json an ordinary
			// top-level file and the first cycle pushes the token to the hub —
			// and to every teammate's disk.
			//
			// Both directions: a folder inside the home, and a folder that
			// CONTAINS it. The second is the same push one level down —
			// settings.json is then just a file some directories deep, and the
			// reserved-name rule only knows ".bdrive" while $BDRIVE_HOME is
			// whatever the operator called it.
			if home, herr := config.Home(); herr == nil &&
				(store.UnderRoot(home, folder) || store.UnderRoot(folder, home)) {
				return fmt.Errorf("%s and the beardrive home %s contain one another; the home holds "+
					"this device's credentials and every project's local data, so this is not a "+
					"project folder", folder, home)
			}
			// Same hole one directory over, and the one a user told to "sync
			// my skills" walks straight into with `bdrive init ~/.claude`.
			// filepath.Base on the already-absolute folder is the whole
			// check — no lookup that an unresolvable path could disable.
			if config.AgentConfigDir(filepath.Base(folder)) {
				return fmt.Errorf("%s is an agent's configuration directory: the reserved-path rule "+
					"only covers segments BELOW a mount root, so mounting it makes settings.json, "+
					".credentials.json and your saved sessions ordinary top-level files that sync "+
					"to the whole team. Sync %s instead", folder, filepath.Join(folder, "skills"))
			}
			// A workspace root holds projects; it is never one itself. Nothing
			// at a root syncs — that is what lets the user keep folders
			// BearDrive never touches beside the ones it does — so mounting it
			// would sync every one of them, and its manifest with them.
			if config.HasManifest(folder) {
				return fmt.Errorf("%s is a BearDrive workspace root, not a project\n"+
					"the root indexes the projects inside it and never syncs itself\n"+
					"run init in a folder under it instead, e.g. bdrive init %s",
					folder, filepath.Join(folder, "team"))
			}
			// And the other direction, which is the damaging one: mounting a
			// folder that CONTAINS a root sweeps up everything the root exists
			// to hold apart. The nested project is excluded (the syncer's
			// nested-mount handling), but the folders beside it are not — they
			// would sync to the whole team on the next cycle, silently.
			//
			// Found through the registry rather than by walking down: a root's
			// projects are its immediate children, and this device knows where
			// its mounts are.
			if root, found := workspaceRootUnder(folder); found {
				return fmt.Errorf("%s contains the BearDrive workspace root at %s\n"+
					"that root holds folders you chose NOT to sync, and syncing this folder "+
					"would push them to everyone in the project\n"+
					"sync the project folders under %s instead", folder, root, root)
			}
			if projectID != "" && projectName != "" {
				return fmt.Errorf("--project and --name are mutually exclusive")
			}
			// Both refusals land before any network call or file write: a
			// scope that excludes a template's top level would hide it from
			// the whole team (scope rules live in the synced .bdriveignore),
			// and an unknown name should cost nothing.
			var tpl templates.Template
			if template != "" {
				if len(only) > 0 {
					return fmt.Errorf("--template and --only are mutually exclusive: " +
						"scope rules live in the synced .bdriveignore, so a scope that leaves out " +
						"the template's folders would hide them for everyone")
				}
				if tpl, err = templates.Get(template); err != nil {
					return err
				}
			}

			// Already initialized → resume (also self-heals after a move).
			if proj, ok, err := config.ResolveMount(folder); err != nil {
				return err
			} else if ok && proj.Remote != "" {
				// The project name came from the hub; it reaches a terminal
				// here and on every later command (see safeField).
				fmt.Printf("resuming %s (project %s)\n", folder, safeField(proj.Volume, 120))
				// --only on an existing mount narrows it in place: the scope is
				// just .bdriveignore rules, so re-running init is a legitimate
				// way to set them (and is what `bdrive scope` points at).
				if cmd.Flags().Changed("only") {
					scope, err := cleanScopeDirs(only)
					if err != nil {
						return err
					}
					if err := mkdirScopeDirs(folder, scope); err != nil {
						return err
					}
					if err := writeScopeDirs(folder, scope); err != nil {
						return err
					}
					if len(scope) == 0 {
						fmt.Println("  syncing: the whole folder (scope rules removed)")
					} else {
						fmt.Printf("  syncing: ./%s only (rules written to .bdriveignore)\n", strings.Join(scope, ", ./"))
					}
				}
				// --template in an already-initialized folder is the agent's
				// path: init pulled the project, the folder turned out to be
				// empty, so the structure is written here and the usual cycle
				// pushes it. Existing paths are never overwritten, which is
				// what makes re-running this safe.
				if tpl.Name != "" {
					if err := seedLocally(folder, tpl); err != nil {
						return err
					}
				}
				if !noHooks {
					installAgentHooks(folder)
				}
				if !noAutostart {
					installAutostart(stdinIsTTY() && !yes)
				}
				return startSync(cmd.Context(), folder, proj, foreground, 3*time.Second, 10*time.Second)
			}

			// A new mount inside an existing one is two writers over one set of
			// paths: the parent syncs these files into the parent's project and
			// this mount into its own. Cheap to refuse here; the syncer's
			// nested-mount handling exists because it wasn't.
			if root, nested := findMountRoot(filepath.Dir(folder)); nested {
				return fmt.Errorf("%s is inside the project at %s\n"+
					"a project inside a project syncs the same files twice, to two different projects\n"+
					"sync it as part of that project, or move it outside %s first", folder, root, root)
			}

			// Sign in first if this device has no (valid) session.
			settings, restoreSession, err := ensureLogin(serverURL)
			if err != nil {
				return err
			}
			server := settings.Server

			// A new session is not committed until this hub has proved usable:
			// it answered with a project, and this device can open that
			// project's remote. Everything between here and hubUsable is that
			// proof, and anything that fails in it puts the previous session
			// back — one arm point and one disarm point rather than a restore
			// call bolted onto each early return. See ensureLogin.
			hubUsable := false
			defer func() {
				if !hubUsable {
					restoreSession()
				}
			}()

			interactive := stdinIsTTY() && !yes

			// Which project — and, when creating one, what does it start from?
			var p serverProject
			var created bool
			switch {
			case projectID != "":
				p, err = getProject(server, settings.Token, projectID)
			case projectName != "":
				p, created, err = createProject(server, settings.Token, projectName, template)
			case interactive:
				p, created, err = chooseProject(server, settings.Token, filepath.Base(folder), &template, &tpl)
			default:
				p, created, err = createProject(server, settings.Token, filepath.Base(folder), template)
			}
			if err != nil {
				return fmt.Errorf("cannot set up project on %s: %w", server, err)
			}
			// A template is applied when a project is created, and only then:
			// joining one that already exists must never restructure it.
			if tpl.Name != "" && !created && p.Template != tpl.Name {
				from := "an empty project"
				if p.Template != "" {
					// safeField, like p.Name one screen down. p.Template is the
					// same JSON object from the same hub and it was concatenated
					// raw into an error the terminal prints — OSC 52 (a clipboard
					// write), CSI (a repaint), C1 and bidi all rendered intact,
					// on the surface an onboarding agent reads verbatim.
					from = "the " + safeField(p.Template, 120) + " template"
				}
				return fmt.Errorf("project %q already exists and was created from %s\n"+
					"a template only applies to a new project; connect to this one without --template", p.Name, from)
			}
			// The hub chooses the id and every later cycle builds its remote URL
			// from it — but the only thing that validates it is remote.Open,
			// inside the first cycle, where a failure degrades to "offline" by
			// design. An id this device cannot open would leave init reporting
			// success and every cycle a silent no-op forever, so open it here.
			remoteURL := server + "/p/" + p.ID
			be, err := remote.Open(cmd.Context(), remoteURL)
			if err != nil {
				return fmt.Errorf("%s answered with a project this device cannot sync: %w", server, err)
			}
			be.Close()
			if err := checkNotAlreadyMounted(remoteURL, folder, p.Name); err != nil {
				return err
			}
			hubUsable = true // the session is this device's default from here on

			// All of this folder, or only some of it?
			if len(only) == 0 && interactive && !cmd.Flags().Changed("only") {
				only, err = chooseScope()
				if err != nil {
					return err
				}
			}
			scope, err := cleanScopeDirs(only)
			if err != nil {
				return err
			}
			if err := mkdirScopeDirs(folder, scope); err != nil {
				return err
			}

			if err := os.MkdirAll(folder, 0o755); err != nil {
				return err
			}
			proj := config.Project{
				Volume: p.Name,
				Remote: remoteURL,
			}
			proj, err = config.SaveProject(folder, proj)
			if err != nil {
				return err
			}
			ignorePath := filepath.Join(folder, ".bdriveignore")
			if _, err := os.Stat(ignorePath); os.IsNotExist(err) {
				if err := os.WriteFile(ignorePath, []byte(starterIgnore), 0o644); err != nil {
					return err
				}
			}
			if len(scope) > 0 {
				if err := writeScopeDirs(folder, scope); err != nil {
					return err
				}
			}
			fmt.Printf("initialized %s\n  server:  %s\n  project: %s (%s)\n", folder, server, safeField(p.Name, 120), p.ID)
			if tpl.Name != "" {
				// Seed from the client's OWN registry whatever the hub answered.
				// `p.Template` is a string the hub chose, and it used to be the
				// only evidence this branch had: a hub that answers "docs" and
				// stores nothing left the folder empty under a success message
				// that named the structure, and an older hub that ignored the
				// field made --template a quiet no-op. seedLocally skips every
				// path that already exists, so this is the repair in both cases.
				//
				// ponytail: when the hub really did seed, the same bytes are
				// journaled twice — once by this device, once by the hub — so
				// History shows two entries per template file. Both come from the
				// same embedded registry, so they are the same blob and nothing
				// diverges. Deduplicating would mean waiting for the first cycle
				// to finish before deciding, which the foreground path never
				// returns from.
				if err := seedLocally(folder, tpl); err != nil {
					return err
				}
			}
			if !noHooks {
				installAgentHooks(folder)
			}
			if !noAutostart {
				installAutostart(interactive)
			}
			if len(scope) > 0 {
				dirs := make([]string, len(scope))
				for i, d := range scope {
					dirs[i] = "./" + d
				}
				fmt.Printf("  syncing: %s only (rules written to .bdriveignore)\n", strings.Join(dirs, ", "))
			}
			// After the ignore file and any scope rules are on disk, so the
			// measurement is of what would REALLY sync — and before the first
			// cycle, so narrowing is still free.
			warnBigFolder(folder, proj.Include)
			if err := startSync(cmd.Context(), folder, proj, foreground, 3*time.Second, 10*time.Second); err != nil {
				return err
			}
			if foreground {
				return nil // daemon already ran and exited; "syncing automatically" would be false now
			}
			fmt.Printf(`
done — the daemon now keeps this folder in sync automatically.

  your project:  %s/%s   (sign-in required; `+"`bdrive url <file>`"+` links any file)

next steps:
  connect another device or teammate:  bdrive init --project %s
  see who changed what:                bdrive log
  share a file by public URL:          bdrive share <file>
`, server, p.ID, p.ID)
			// The one and only place the CLI asks for a star. A folder was just
			// set up successfully — about once per project per machine — which
			// is the single moment that earns the ask. Never from a command that
			// repeats (sync, status, the daemon), and never without a TTY: a
			// star plea in a CI log or in output a script parses is exactly what
			// got postinstall ads banned from npm.
			if stdinIsTTY() {
				fmt.Printf("\nif this is useful, a star helps other teams find it: %s\n", repoURL)
			}
			return nil
		},
	}
	c.Flags().StringVar(&serverURL, "server", "", "hub to connect to (default: the remembered one); signs in there if this device has no session")
	c.Flags().StringVar(&projectID, "project", "", "connect an existing project by id (p-xxxxxxxx)")
	c.Flags().StringVar(&projectName, "name", "", "project name to create or join (default: folder name)")
	c.Flags().StringVar(&template, "template", "", "start from a structure ("+strings.Join(templates.Names(), ", ")+"); default: an empty project")
	c.Flags().StringSliceVar(&only, "only", nil, "sync only these subfolders of the mount (comma-separated, e.g. wiki,docs) — written as .bdriveignore rules")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "accept defaults, never prompt")
	c.Flags().BoolVarP(&foreground, "foreground", "f", false, "run the sync daemon in the foreground")
	c.Flags().BoolVar(&noHooks, "no-hooks", false, "skip registering agent sync hooks")
	c.Flags().BoolVar(&noAutostart, "no-autostart", false, "skip registering sync to restart at login")
	return c
}

// seedLocally writes a template into the folder and says what it wrote.
// Paths that already exist are never touched, so seeding twice — an agent
// re-running init, a hub that already seeded — is a no-op rather than a
// conflict.
func seedLocally(folder string, tpl templates.Template) error {
	wrote, err := tpl.WriteTo(folder)
	if err != nil {
		return fmt.Errorf("seed the %s template: %w", tpl.Name, err)
	}
	if len(wrote) == 0 {
		fmt.Printf("  start:   %s template (already present)\n", tpl.Name)
		return nil
	}
	fmt.Printf("  start:   %s template (%d files — read AGENTS.md)\n", tpl.Name, len(wrote))
	return nil
}

// installAgentHooks registers turn-boundary sync hooks as part of init, so
// the one command a user (or their agent) already ran covers hooks too — a
// separate `bdrive hooks install` is one more permission prompt, and it is
// exactly the command agent permission layers flag.
//
// The hooks go in each platform's USER config, once per machine: platforms
// read hook config only from the directory a session starts in, so a
// per-project file would cover only the sessions that happen to start there
// (and, living inside a mount, would sync to the whole team).
// Idempotent; failure never fails init (sync is already up).
func installAgentHooks(folder string) {
	results, err := agenthooks.Install(folder, nil)
	if err != nil {
		fmt.Printf("  hooks:   %v — run `bdrive hooks install` to retry\n", err)
		return
	}
	for _, r := range results {
		state := "hooks registered"
		if !r.Changed {
			state = "hooks already registered"
		}
		fmt.Printf("  %-8s %s  →  %s\n", r.Agent, state, r.Path)
		if r.Migrated != "" {
			fmt.Printf("           moved out of %s (project hooks are no longer used)\n", r.Migrated)
		}
		if r.Note != "" {
			fmt.Printf("           note: %s\n", r.Note)
		}
	}
}

// installAutostart registers the login unit so a reboot doesn't quietly stop
// sync. Best effort and one line of output: a platform without one (a BSD, or
// Linux without systemd) or an unwritable config dir is not a reason to fail
// an init that otherwise worked — the folder syncs, it just won't come back by
// itself.
//
// On macOS the write itself is what users see: the moment anything lands in
// ~/Library/LaunchAgents, Ventura+ pops "Background Items Added", naming a
// binary they just installed. Nothing suppresses that notice — SMAppService, a
// System Settings login item and a real crontab all trigger it (cron also
// wants Full Disk Access on top) — so the fix is to make it expected rather
// than to dodge it: say what is about to happen before it happens, and on a
// TTY ask first. Linux and Windows show nothing, so they are not asked.
func installAutostart(interactive bool) {
	if runtime.GOOS == "darwin" && !autostart.Installed() {
		const notice = `macOS will show a "Background Items Added" notice for it`
		if interactive {
			ok := true
			// An interrupt reads as "no": autostart is a convenience, and the
			// caller is one step from starting the daemon anyway.
			//
			// ponytail: a decline is not remembered, so re-running `bdrive init`
			// interactively asks again. Persist it in settings.json if that ever
			// nags — agents and scripts have no TTY and never see the prompt.
			if err := survey.AskOne(&survey.Confirm{
				Message: "Restart syncing at login? (" + notice + ")",
				Default: true,
			}, &ok); err != nil || !ok {
				fmt.Println("  login:   autostart skipped — `bdrive autostart install` enables it later")
				return
			}
		} else {
			fmt.Println("  login:   registering sync to restart at login — " + notice)
		}
	}
	res, err := autostart.Install()
	if err != nil {
		if !errors.Is(err, autostart.ErrUnsupported) {
			fmt.Printf("  login:   autostart not registered (%v) — run `bdrive resume` after a reboot\n", err)
		}
		return
	}
	state := "autostart registered"
	if !res.Changed {
		state = "autostart already registered"
	}
	fmt.Printf("  login:   %s  →  %s\n", state, res.Path)
}

// checkNotAlreadyMounted refuses a second folder for a project this device
// already syncs. Each device writes one journal per project on the remote, so
// two local mounts of one project are two writers of the same journal: the
// second one's push overwrites the first one's ops and those files disappear
// from the hub. Stale registry entries (folder gone) are ignored.
func checkNotAlreadyMounted(remote, folder, name string) error {
	mounts, err := config.LoadMounts()
	if err != nil {
		return nil // registry unreadable: don't block setup over it
	}
	for _, mi := range mounts {
		if mi.Remote != remote || mi.Path == folder || mi.Path == "" {
			continue
		}
		if !config.IsMount(mi.Path) {
			continue // moved or deleted; the registry entry is stale
		}
		return fmt.Errorf("this device already syncs project %q at %s\n"+
			"one folder per project per device: a second mount would overwrite that folder's history on the hub\n"+
			"use that folder, or release it first with `bdrive stop --forget %s`", name, mi.Path, mi.Path)
	}
	return nil
}

func installHooksIn(folder string) {
	results, err := agenthooks.Install(folder, nil)
	if err != nil {
		fmt.Printf("  hooks:   %v — run `bdrive hooks install` to retry\n", err)
		return
	}
	if len(results) == 0 {
		return
	}
	for _, r := range results {
		state := "hooks registered"
		if !r.Changed {
			state = "hooks already registered"
		}
		fmt.Printf("  %-8s %s  →  %s\n", r.Agent, state, r.Path)
		if r.Note != "" {
			fmt.Printf("           note: %s\n", r.Note)
		}
	}
}

// ensureLogin returns settings with a working session, running the login
// flow first when there is none (or the token went stale). A non-empty
// wantServer targets that hub — the reason `bdrive init --server <url>`
// exists at all: without it, connecting to a hub this device has never seen
// takes a separate `bdrive login <url>` first, and every extra command is
// another permission prompt for whoever is driving.
// It also returns a rollback: `--server` is the one value the runbook has an
// agent take on faith out of a paste prompt, and signing in to it drops the
// session this device already had (see the comment on the `server != prev`
// branch below). Nothing put that back, so one mistyped or hostile URL signed
// the device OUT of its real hub and left it defaulting to the other one — on
// a run that ended in "Error:" — after which the next bare `bdrive login`,
// `bdrive init` or `bdrive status` targeted the attacker. The caller commits
// the new session only once the hub has proved usable.
//
// The rollback is a no-op when there was no session to lose: a device signing
// in for the first time keeps the token it just minted, because there is
// nothing to strand it away from and re-doing the device flow on the retry
// helps nobody.
func ensureLogin(wantServer string) (config.Settings, func(), error) {
	settings, err := config.LoadSettings()
	if err != nil {
		return settings, func() {}, err
	}
	before := settings
	rollback := func() {}
	if before.Token != "" {
		rollback = func() {
			if err := config.SaveSettings(before); err != nil {
				fmt.Printf("warning: could not restore this device's session on %s: %v\n", before.Server, err)
			}
		}
	}
	server := settings.Server
	if wantServer != "" {
		server = normalizeServer(wantServer)
	}
	if server == "" {
		server = config.DefaultServer
	}
	cfg, err := fetchServerConfig(server)
	if err != nil {
		return settings, rollback, fmt.Errorf("cannot reach bdrive server at %s: %w (set one with `bdrive login <url>`)", server, err)
	}
	prev := settings.Server
	// A device token belongs to the hub that issued it, and settings.Server is
	// the whole of that binding (remote.deviceToken hands the token to any base
	// with the same origin). Pointing init at a different server therefore has
	// to drop the old session first — on the no-auth branch too, where nothing
	// else ever clears it and a server that simply answers "auth: disabled"
	// would otherwise be handed the previous hub's credential.
	if server != prev {
		settings.Token, settings.Email, settings.Name = "", "", ""
	}
	if !cfg.Auth.Enabled {
		settings.Server = server
		return settings, rollback, config.SaveSettings(settings)
	}
	settings.Server = server
	switch {
	case settings.Token != "" && prev == server:
		if _, err := whoAmIOnServer(server, settings.Token); err == nil {
			// Same hub, same live session: nothing was replaced, so nothing
			// needs putting back.
			return settings, func() {}, nil
		}
		fmt.Println("session expired — signing in again")
	case prev != "" && prev != server:
		fmt.Printf("signing in to %s (this device was signed in to %s)\n", server, prev)
	}
	if err := runLogin(server, cfg, false); err != nil {
		rollback()
		return settings, func() {}, err
	}
	settings, err = config.LoadSettings()
	return settings, rollback, err
}

// normalizeServer accepts what people (and agents) actually type: a bare
// host, with or without a port, keeps working instead of failing and being
// retried. Anything already carrying a scheme is left alone.
func normalizeServer(raw string) string {
	raw = strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if strings.Contains(raw, "://") {
		return raw
	}
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	// Local hubs are plain http; anything else on the internet is https.
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]" {
		return "http://" + raw
	}
	return "https://" + raw
}

// chooseProject asks what to do and does it. The starting point is asked
// only on the create-a-new-project branch — connecting to an existing
// project never restructures it — and the picked template is handed back
// through name/tpl so the caller's refusals and summary see it too.
func chooseProject(server, token, defaultName string, name *string, tpl *templates.Template) (serverProject, bool, error) {
	var mode string
	if err := survey.AskOne(&survey.Select{
		Message: "What would you like to do?",
		Options: []string{"Create a new project", "Connect an existing project"},
	}, &mode); err != nil {
		return serverProject{}, false, err
	}
	if mode == "Create a new project" {
		projName := defaultName
		if err := survey.AskOne(&survey.Input{Message: "Project name:", Default: defaultName}, &projName); err != nil {
			return serverProject{}, false, err
		}
		if *name == "" {
			picked, err := chooseTemplate()
			if err != nil {
				return serverProject{}, false, err
			}
			if picked != "" {
				t, err := templates.Get(picked)
				if err != nil {
					return serverProject{}, false, err
				}
				*name, *tpl = picked, t
			}
		}
		p, created, err := createProject(server, token, projName, *name)
		if err == nil && !created {
			fmt.Printf("project %q already exists — connecting to it\n", p.Name)
		}
		return p, created, err
	}
	projects, err := listProjects(server, token)
	if err != nil {
		return serverProject{}, false, err
	}
	if len(projects) == 0 {
		return serverProject{}, false, fmt.Errorf("the server has no projects yet; create one instead")
	}
	labels := make([]string, len(projects))
	for i, p := range projects {
		labels[i] = fmt.Sprintf("%s (%s)", p.Name, p.ID)
	}
	var idx int
	if err := survey.AskOne(&survey.Select{Message: "Connect to which project?", Options: labels}, &idx); err != nil {
		return serverProject{}, false, err
	}
	return projects[idx], false, nil
}

// chooseTemplate offers the three starting points in the same words the web
// dialog uses, recommended first and "empty" as a real option rather than a
// footnote. Returns "" for an empty project.
func chooseTemplate() (string, error) {
	list := templates.List()
	options := make([]string, 0, len(list)+1)
	for i, t := range list {
		label := fmt.Sprintf("%s — %s", t.Title, t.Blurb)
		if i == 0 {
			label += "  (recommended)"
		}
		options = append(options, label)
	}
	options = append(options, "Empty project — just the folder")

	var idx int
	if err := survey.AskOne(&survey.Select{
		Message: "Start from a structure?",
		Options: options,
		Default: options[len(options)-1],
	}, &idx); err != nil {
		return "", err
	}
	if idx == len(list) {
		return "", nil
	}
	return list[idx].Name, nil
}

// chooseScope returns nil for whole-folder sync, or the subfolders to narrow
// to (written as .bdriveignore rules, not a separate setting).
func chooseScope() ([]string, error) {
	var mode string
	if err := survey.AskOne(&survey.Select{
		Message: "What should sync?",
		Options: []string{"The whole folder", "Only some subfolders"},
	}, &mode); err != nil {
		return nil, err
	}
	if mode == "The whole folder" {
		return nil, nil
	}
	dirs := "shared"
	if err := survey.AskOne(&survey.Input{Message: "Subfolder(s) to sync, space- or comma-separated:", Default: "shared"}, &dirs); err != nil {
		return nil, err
	}
	return strings.Fields(strings.ReplaceAll(dirs, ",", " ")), nil
}

type serverProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Template is the structure the hub seeded the project from, "" for an
	// empty project — and also for a hub too old to know the field, which is
	// why init falls back to seeding locally rather than trusting it blindly.
	Template string `json:"template"`
}

var initClient = &http.Client{Timeout: 10 * time.Second, CheckRedirect: dropTokenOffOrigin}

// dropTokenOffOrigin keeps a hub's 3xx from carrying this device's credential
// somewhere else: net/http strips Authorization only when the HOSTNAME
// changes, so another port, an https→http downgrade or a sibling subdomain
// kept the bearer token. Same rule the sync backend's client applies
// (remote/http.go refuseOffOriginRedirect).
func dropTokenOffOrigin(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) > 0 && !remote.SameOrigin(req.URL.String(), via[0].URL.String()) {
		req.Header.Del("Authorization")
	}
	return nil
}

// tokenGoesTo reports whether this device's credential may be attached to a
// request for rawURL: only when the target is the origin `bdrive login` signed
// in to. Every CLI destination but the login flow's own comes out of a
// folder's .bdrive/config.json, which travels with the folder (a zip, a clone,
// a colleague's copy) — so the folder must not get to choose where the token
// goes. remote.deviceToken binds the sync backend the same way.
func tokenGoesTo(rawURL string) bool {
	s, err := config.LoadSettings()
	if err != nil {
		return false
	}
	return remote.SameOrigin(rawURL, s.Server)
}

// serverDo sends an API request with this device's token attached, and
// turns a 401 into a run-bdrive-login hint.
func serverDo(method, url, token string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" && tokenGoesTo(url) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := initClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, fmt.Errorf("this server requires sign-in; run `bdrive login`")
	}
	return resp, nil
}

func httpBodyError(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
}

func getProject(server, token, id string) (serverProject, error) {
	var p serverProject
	resp, err := serverDo(http.MethodGet, server+"/api/projects/"+url.PathEscape(id), token, nil)
	if err != nil {
		return p, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p, httpBodyError(resp)
	}
	err = json.NewDecoder(resp.Body).Decode(&p)
	return p, err
}

func listProjects(server, token string) ([]serverProject, error) {
	resp, err := serverDo(http.MethodGet, server+"/api/projects", token, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpBodyError(resp)
	}
	var out struct {
		Projects []serverProject `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

func createProject(server, token, name, template string) (serverProject, bool, error) {
	body, err := json.Marshal(map[string]string{"name": name, "template": template})
	if err != nil {
		return serverProject{}, false, err
	}
	resp, err := serverDo(http.MethodPost, server+"/api/projects", token, body)
	if err != nil {
		return serverProject{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return serverProject{}, false, httpBodyError(resp)
	}
	var out struct {
		Project serverProject `json:"project"`
		Created bool          `json:"created"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return serverProject{}, false, fmt.Errorf("not a bdrive server (bad response): %w", err)
	}
	return out.Project, out.Created, nil
}
