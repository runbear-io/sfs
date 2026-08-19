---
title: CLI reference
description: Every bdrive command and what it does.
---

One binary, `bdrive` — the CLI, the sync daemon, and the web server.

## Commands

| Command | Description |
|---|---|
| `bdrive login [server-url]` | Sign this device in. Browser flow — the page names the account this terminal would act as and lets you switch before approving; `--device` forces the approval-link flow, and shells without a TTY (agents, CI, SSH) fall back to it automatically. Default server is beardrive.ai — the managed cloud, free personal workspace on signup; pass your hub URL to self-host. Switch hubs with `bdrive login <new-url>`. `--status` shows the current server and account |
| `bdrive logout` | Sign this device out — revokes this device's token on the hub, then clears it locally. `--forget` also drops the remembered server |
| `bdrive init [folder]` | Create or connect a project and start syncing — the mount is always exactly the folder named. Interactive on a TTY; flags (`--name`, `--project`, `--server`, `--only`, `--template`, `--yes`) for scripts. `--template docs\|wiki\|para\|skills` starts a new project from a structure instead of an empty folder. Also registers agent sync hooks for detected platforms (`--no-hooks` skips them) and a login item so sync resumes after a reboot (`--no-autostart` skips), and prints the project's hub link. Re-run to resume |
| `bdrive resume` | Restart the sync daemon for every project on this device that isn't paused — after a reboot, a crash, or a manual kill. Idempotent, so running it twice is harmless. This is what the login item runs |
| `bdrive autostart [install\|uninstall]` | Show, add, or remove the login registration that runs `bdrive resume` after a reboot: a user LaunchAgent on macOS, a systemd user unit on Linux (needs systemd). `bdrive init` installs it; `--no-autostart` skips it |
| `bdrive stop [folder]` | Stop syncing — daemon and agent sync hooks both pause. Files stay on disk; `bdrive init` resumes |
| `bdrive scope [add\|rm <dirs...>]` | Show or change which subfolders sync — edits the managed block of `.bdriveignore` rules that `init --only` writes. Run from the mount root; the daemon picks changes up in seconds. `rm` stops syncing a folder but deletes nothing, locally or on the hub |
| `bdrive scope --explain` | List every path in the folder, split into what syncs and what does not, with counts — the verifiable answer to "what leaves this machine". Pure read: no daemon, no lock, no network |
| `bdrive grep <pattern> [folder]` | Search the text **inside** the files a project syncs. `pattern` is a Go RE2 regexp, or a literal string with `-F`. `-i` ignores case, `-l` prints matching paths only, `-n` caps the lines printed (default 200, `0` = all). Pure read: no daemon, no lock, no network |
| `bdrive stale [folder]` | Find synced markdown that links to a file written **after** the doc itself — staleness by what moved, not by the calendar. `-l` prints outgrown paths only, `-n` caps the docs printed (default 50, `0` = all). Pure read: no daemon, no lock, no network. Exit status is 0 whether or not anything is stale |
| `bdrive forget <path>...` | Stop syncing a path and remove it from the hub. Adds the rule to `.bdriveignore` (which syncs) and prunes in one step. Local files are never touched, here or on teammates' devices |
| `bdrive url [path]` | Internal hub link for a file or folder — sign-in and membership required. `--sync` pushes first; no argument gives the project home. Computed locally |
| `bdrive share <file>` | Public URL for a synced file — links are per-file, so a folder is refused with a file inside it named instead. `--list`, `--revoke`, `--expires` (the hub's Share dialog can also set an expiry on an existing link). Refuses a file whose first 1 MiB holds credential-shaped strings — `--force` shares it anyway |
| `bdrive sync [folder]` | Run one sync cycle now. Refuses folders this device never `init`ed and folders paused by `bdrive stop`. `--note <text>` stamps session context onto changes; `--note-ttl` (default 30m) bounds it, and a plain `bdrive sync` with no `--note` clears it. `--prune` also removes from the hub what `.bdriveignore` now excludes (files stay on disk everywhere). `--hook <label>` is agent-hook plumbing: it also reports the files teammates changed since the agent's last turn |
| `bdrive hooks [install\|uninstall]` | Register turn-boundary sync hooks in each detected agent platform's user config — once per machine, covering every folder. Run automatically by `bdrive init`; idempotent; `--agent` overrides detection. `uninstall` removes only BearDrive's own hook entries |
| `bdrive read-log [folder]` | Hook plumbing: queue agent file reads for the hub's read heatmap. Registered by `bdrive hooks install` |
| `bdrive status [folder]` | Projects, daemon state, pending changes, and any synced files that looked like they held credentials when they last changed |
| `bdrive log [folder] [-p path] [-n N]` | Change history: account, device, time, file — newest first by the time shown, which is when the change was journaled; a file written more than a minute before it arrived (a rename, or an old document added today) also shows `written <time>` |
| `bdrive restore <file> [version]` | Put an earlier version of a file back, as a new change. No version restores the previous one; `--list` shows the versions with their short hashes |
| `bdrive export [folder]` | Export the whole project — all devices' history and content — to a portable `.tar.gz` (`-o` names the file) |
| `bdrive import <archive>` | Import an export archive as a new project on the hub you're logged into (`--name` overrides the archive's name) |
| `bdrive serve [folder \| storage-root-url]` | Web server: viewer, uploads, multi-project sync hub (`bdrive web` is a deprecated alias) |
| `bdrive whoami` | Signed-in account and device identity used in change tracking |
| `bdrive version` | Version (also `bdrive --version`) |

## Notes on a few

### `bdrive init`

The front door. **The mount is always exactly the folder you name** —
`bdrive init wiki` makes `./wiki` the project, and its contents are the
project's contents. Nothing re-roots a mount somewhere else.

Interactive on a TTY, with survey menus for create-new versus
connect-existing (showing a project list) and whole-folder versus only some
subfolders. To sync part of a folder without moving the mount, use
`--only <dirs>` (comma-separated — `bdrive init . --only wiki,docs`), which
writes a managed block of `.bdriveignore` rules rather than a separate scope
setting. Full flag bypass with `--name`, `--project`, `--template`, `--only`,
`--yes`, and it never prompts without a TTY.

A **new** project can start from a structure rather than an empty folder:
`--template docs` (docs/, decisions/), `--template wiki` (an LLM-maintained
wiki: sources/, wiki/, index.md, log.md), `--template para` (projects/, areas/,
resources/, archives/) or `--template skills` (a shared agent-skill library:
`.claude/skills/`). Each one is a small directory skeleton plus the
`AGENTS.md` that says where a new note goes, when something is archived, and
what a good filename looks like — the instructions are the point, the folders
are the scaffolding. On a TTY the same starting points are offered as a
menu (recommended first, "empty project" last, and preselected); `--yes` and
non-TTY never prompt and stay empty.

The hub seeds the template when the project is created, so it is there in the
browser and arrives on every device that connects afterwards; `init` also
writes the same structure locally from the binary's own copy, so the folder has
what the command said it would even if the hub stored nothing. Files the hub
seeds are journaled with no account and a `seeded from the <name> template`
note, so change history tells them from a file a teammate wrote. Joining a
project that already exists never restructures it, seeding never overwrites a
file that already exists, and `--template` together with `--only` is refused before
anything is written — scope rules live in the synced `.bdriveignore`, so a
scope that left out the template's folders would hide them for the whole team.

It runs the login flow first when there is no session, writes
`.bdrive/config.json`, seeds `.bdriveignore`, registers agent sync hooks for
every detected platform (Claude Code, Codex, Gemini CLI, Hermes — `--no-hooks`
skips them) and a login item that restarts syncing after a reboot
(`--no-autostart` skips), starts sync, and prints the
project's hub link. That is deliberate: one command means one permission prompt
for an agent, instead of four. Re-running it resumes — including after a folder
move.

The hooks land in each platform's **user** config (`~/.claude/settings.json` and
friends), once per machine, so they cover every session in every folder; nothing
is written inside the project. See
[Hooks in detail](/manual/hooks/).

The daemon scans every 3s and talks to the hub every 10s; those intervals are
tunable on `bdrive daemon run`, not on init.

### `bdrive sync --note`

Stamps session context — an agent session id, say — onto changes. It shows up in
`bdrive log` and hub history, and keeps applying to daemon-committed changes
until `--note-ttl` expires.

A plain `bdrive sync` with no `--note` **clears** the note: an explicit sync is a
human act, so the edit you just made by hand is not filed under the last agent
session. Hook- and daemon-driven syncs are unaffected and keep the note until the
TTL expires — that is what the TTL is for.

### `bdrive restore` — undoing a change

An agent rewrote a file you liked. Put the old bytes back:

```
$ bdrive restore docs/spec.md
restored docs/spec.md to the version from 2026-07-28 14:01 (a3f9c1e2, 12.0 KB)

$ bdrive restore docs/spec.md --list      # short hash, time, size, who
$ bdrive restore docs/spec.md a3f9c1e2    # a specific version (any unique prefix)
```

Restoring writes those bytes back as a **new change**. Nothing is erased: the
versions in between stay in the history, the restore itself shows up in
`bdrive log` and the hub's History view, and it syncs to every device and
teammate like any other edit — so you can restore away from a restore. The hub
has the same button on every history row.

The hub's History view narrows the feed by path substring, author and date
range (dates are UTC days, inclusive at both ends). The filters live in the
URL — `<project>/history?q=runbook&user=mira@acme.io&since=2026-07-01&until=2026-07-31`
— so a narrowed feed is a link you can send, and it survives reload and Back.
Filtering happens on the server, so paging through a filtered feed shows every
match, not just the ones on the first page.

**Restore puts content back; it does not delete.** To un-create a file an agent
run *created*, open that run in the hub's History view and use the row's
**undo — remove file** button (it asks first: the file leaves every synced
device, and the DELETED row it leaves behind restores it). From the CLI, delete
the file yourself and let the next sync carry that.

### `bdrive grep` — search what is inside the files

`bdrive grep` searches file *contents*, not names:

```sh
bdrive grep 'retention.*fold'
# wiki/runbook.md:42: the retention fold collapses day buckets
# specs/reads.md:8: retention folding happens at boot
# 2 files, 2 matching lines

bdrive grep -i -l TODO      # matching paths only, case-insensitive
bdrive grep -F 'a[b]c'      # literal string, not a regexp
```

**It searches exactly what the project syncs.** A `.bdriveignore` rule or a
narrowed `bdrive scope` excludes a file from search the same way it excludes it
from sync, so results never mention a file your teammates cannot see, and
`.bdrive/` state never appears. Binary files are skipped.

It reads the real files on disk, so it works offline, takes no lock, and never
blocks on a sync in progress. Exit status is 0 when something matched and 1
when nothing did — `bdrive grep -q` has no equivalent, use `-l` and check the
status.

:::note[Local, not hub-wide]
This searches *this* folder. Searching contents across a whole hub, or from the
browser, is not built yet — the ⌘K palette covers file names, projects and
actions.
:::

### `bdrive stale` — find the docs your code has outgrown

A doc does not go stale because a month passed. It goes stale when the thing it
describes moves. `bdrive stale` reads that directly: it scans synced markdown
for references to other synced files, and reports every doc that links to
something written *after* the doc itself.

```sh
bdrive stale
# docs/architecture.md    3 files newer   (oldest gap 41d)
#   internal/syncer/syncer.go    41d newer
#   internal/store/store.go      41d newer
#   docs/hub-config.md           40d newer
# archive/retired-spec.md 1 file newer    (oldest gap 6d)
#   cmd/bdrive/init.go            6d newer
#
# 2 outgrown docs, 4 stale references

bdrive stale -l       # outgrown paths only, one per line
bdrive stale -n 5     # the five worst
```

Markdown inline links, Obsidian `[[wikilinks]]` and bare path-shaped tokens all
count as references. **Resolution is the filter**: anything that does not land
on a file this project actually syncs — a URL, a `../` escape out of the mount,
a path that no longer exists — is silently ignored, so a `.bdriveignore` rule
or a narrowed `bdrive scope` excludes a file here exactly as it excludes it from
sync.

:::note[The dates come from the journal, not the filesystem]
Materialize writes a teammate's file with *this* device's mtime, so on a machine
that just cloned a project every file's mtime is within seconds of every other.
Comparing mtimes would report nothing on exactly the machine that most needs the
answer. `bdrive stale` uses the same write times `bdrive log` prints, which are
identical on every device and available offline.
:::

**Exit status is 0 whether or not anything is stale.** Unlike `bdrive grep`,
this is advisory output, not a gate — grep's "1 means nothing found" convention
would invert here and fail on a clean project. Read heat, a badge on the hub's
file view, and injecting the flag into an agent's session context are not built
yet; this ships the signal.

### `bdrive forget` and `bdrive sync --prune` — cleaning up the hub

Adding a rule to `.bdriveignore` only stops *future* uploads. Anything that
synced before the rule existed stays on the hub. These two commands are how it
comes off:

```
$ bdrive forget .omc
added `.omc/` to .bdriveignore
synced /Users/you/notes (project "notes")
  ...
  pruned:         72 path(s) removed from the hub (kept on disk)

$ bdrive sync --prune       # same cleanup for rules you added by hand
```

`forget` writes the rule (a trailing `/` for a directory) and prunes in the
same run; it is idempotent, so re-running it just prunes. A path outside the
project is an error and writes nothing.

**No device loses a file.** The removal is journaled as an ordinary delete, and
because `.bdriveignore` syncs, every device receives the rule alongside the
delete and simply stops tracking the path — the file itself stays on disk here
and on every teammate's machine. Nothing is destroyed either: blobs are
retained forever, so the removal shows in `bdrive log` and every past version
stays in the hub's history.

Prune reconciles against `.bdriveignore`, which is shared — so it refuses
outright when those rules contain `!` scope rules (the "only these folders"
block that `init --only` and `bdrive scope` write). With such a scope, pruning
would mean deleting everything outside it from the hub for every teammate. To
remove a specific path, `bdrive forget` it — that writes the exclusion into the
shared rules first, which is what makes it safe.

Mounts created before the scope moved into `.bdriveignore` may still carry a
per-device `include` list in `.bdrive/config.json`; prune never reconciles
against that, so a narrow legacy scope still means "not on my disk", not "not
on the hub".

If a teammate edits the file between your prune and their next sync, their
version wins and the path comes back. Run `--prune` again once they have synced.

### `bdrive status` — the two change counts, and the two degraded access states

`status` reports local work in two counts, because they are two different
states and a change can be in either or both:

```
  pending:  0 local change(s) not yet pushed
  local:    1 change(s) not yet scanned (0 new, 1 edited, 0 removed)
```

- **`pending`** — journalled by a sync cycle, not yet accepted by the hub.
- **`local`** — sitting in the working folder, not yet scanned by any cycle.
  This is what a stopped daemon leaves behind: edit a file with `bdrive stop`
  in effect and nothing has looked at the folder, so `pending` is honestly
  zero while the change is right there. The next sync picks it up.

The `local` count is a read-only walk of the folder — the same filter the
cycle uses, so a `.bdriveignore`d path never counts — and it commits no ops,
writes no journal, and contacts no hub. `status` describes; it never changes
what it is describing.

Alongside those, `status` prints a `secrets:` block naming any synced file
that looked like it held a credential when it last changed, and an `access:`
line whenever the hub is refusing this device. Neither access state is the same
as being offline, and neither ever touches your files:

```
  pending:  3 local change(s) not yet pushed
  access:   read-only (pull only) — 3 local change(s) stay on this device
  reason:   this device is not registered to your account on this hub; update bdrive, then run `bdrive login` on this machine
```

- **`read-only (pull only)`** — the hub refused this device's push. Usually you
  have `read` on the project: the daemon keeps pulling teammates' changes, your
  own edits stay journaled locally, never pushed and never dropped, and they go
  out if you are granted `write` again.
- **`no access to this project — sync paused`** — your access was revoked.
  Nothing is pulled, pushed, or written; the working folder is left exactly as
  it is. Re-granting resumes on the next tick with no manual step.

Always read the `reason:` line under them — it is the hub's own sentence, and
not every refusal is about project permissions. The common non-permission one
is `this device is not registered to your account on this hub`: your device
identity was never bound to your account, which is fixed by updating `bdrive`
and running `bdrive login` on that machine, not in Project settings. Checking
your permissions there will show `write` and tell you nothing.

### The credential warning

The six rules `bdrive share` checks at mint time also run on **every file as it
syncs** — the path every file takes, rather than the rare one:

```
  secrets: 1 file(s) looked like they contain credentials when they last changed
             deploy.md:12  an AWS access key
```

It **only ever warns**. The change is journaled and pushed exactly as it would
be otherwise; the finding appears here and in an agent's context at the start
of its next turn. Nothing is held, blocked, or un-pushed — a false positive
costs one line of text, never a stalled file.

Fixing the file is the whole remedy: the next cycle reads it again and the line
goes away, with no command and no flag. Three limits worth knowing: a file is
checked **when it changes** (so this never says a file is clean), only its
first 1 MiB is read, and the check runs on the device that made the change — a
file synced from elsewhere is not flagged here until it next changes here.
Rule names and line numbers only; the matched text is never printed, logged, or
stored.

`bdrive sync` shows the same two as `remote: read-only (pull only)` /
`remote: no access — sync paused` with the reason on the line below, and the
daemon logs each once on transition rather than on every tick — including the
cheap local-only ticks between remote passes, which never ask the hub anything
and so never revise its last answer. For the permission answers, the fix is in
the hub's Project settings → People; see
[Project permissions](/concepts/permissions/).

### `bdrive login` and switching hubs

`bdrive login` remembers the server in `settings.json` under the bdrive home. To
move to a different hub, run `bdrive login <new-url>` and then re-run
`bdrive init` in each folder to connect it to a project there.

There is no client command to point a folder at a raw bucket. `init` always
writes a hub remote.

### `bdrive export` / `bdrive import` — moving a project between hubs

Re-running `init` against a new hub carries only your files' current state.
`export` + `import` carry the whole project: every device's journal and every
retained blob, so per-file history and authorship arrive intact — and devices
that later connect to the imported project resume exactly where they left off.

```sh
# on any machine that syncs the project (run bdrive sync first)
bdrive export ~/team-wiki -o team-wiki.tar.gz

# point the device at the destination hub and import
bdrive login https://your-hub.example
bdrive import team-wiki.tar.gz
bdrive init --project p-xxxxxxxx   # connect folders to the new project
```

This works in both directions — cloud to self-hosted or self-hosted to
cloud. The archive is the project's store layout in a tar.gz; `import` always
creates a NEW project (it never joins an existing one by name — pass `--name`
if the archive's name is taken), and the destination hub needs uploads
enabled. A single file in the archive may spool at most 256 MiB to local disk
during import; `--max-blob` raises that if the project really holds a bigger
file. Import refuses an archive whose journals reference content the archive
does not hold — the shape a pre-delta-sync `bdrive export` produces against a
newer hub, where large files live as chunks the old binary doesn't know to
collect; re-export with a current `bdrive`, or pass `--allow-incomplete` to
import anyway (the missing files are listed and stay missing). Shares,
invite links, and read-heat stay behind (they belong to the hub, not the
project store). Step-by-step walkthrough:
[Migrate between hubs](/reference/migration/).

## Environment

| Variable | Effect |
|---|---|
| `BDRIVE_HOME` | Relocate all BearDrive state — device identity, settings, mount registry, volume stores — away from `~/.bdrive`. Useful for testing |
| `BDRIVE_TOKEN` | Device token, taking precedence over `settings.json` |
