# BearDrive — Google Drive for AI agents

[![Star on GitHub](https://img.shields.io/github/stars/runbear-io/beardrive?style=flat&logo=github&label=star%20this%20repo&color=f5a623)](https://github.com/runbear-io/beardrive)
[![Go Reference](https://pkg.go.dev/badge/github.com/runbear-io/beardrive.svg)](https://pkg.go.dev/github.com/runbear-io/beardrive)
[![Docs](https://img.shields.io/badge/docs-beardrive.ai-2f81f7)](https://docs.beardrive.ai)

**BearDrive** mounts any folder as a synced volume: its contents stay
synchronized across all your devices and teammates through a BearDrive
**hub**, every change is tracked (who, when, on which device), and
everything keeps working offline. The CLI is `bdrive`; a hub is a
`bdrive serve` server you (or we) run on an object store — clients sync
through it over HTTPS and never touch the storage directly.

What it's for, first and foremost: **sharing context across AI agents** —
give every agent on the team the same folder as memory, and your agent
knows what their agent knows. (People are covered too: any synced file
becomes a public URL that renders as a page.) Notes, plans, findings, and
artifacts follow the team everywhere — and unlike a memory API, they stay
**real files with provenance**: every change is attributed to the human,
agent, and device that made it, and the hub's Dashboard shows what your
agents actually read (and which hot-but-stale docs nobody maintains).

<p align="center">
  <img src="docs/assets/insights.png" alt="Knowledge Insights — every file plotted by agent/human reads vs staleness; hot-but-stale docs are the danger zone" width="820">
</p>

| Browse with read heat | Public share pages |
|---|---|
| ![Folder listing with per-file agent read counts and change feed](docs/assets/browse.png) | ![A shared markdown file rendered as a public page](docs/assets/share.png) |

```console
$ bdrive login https://your-hub   # once per device — self-host a hub in ~10 min (docs/self-hosting.md)
$ cd ~/workspace && bdrive init
initialized /Users/snow/workspace
  server:  https://your-hub
  project: workspace (7f3a2c91-4d5e-4b8a-9c17-2ad0f6b3e9c4)
  claude   hooks registered  →  /Users/snow/.claude/settings.json
  login:   autostart registered  →  ~/Library/LaunchAgents/ai.beardrive.daemon.plist
  daemon:  running (pid 55434, scan 3s, remote sync 10s)
```

> BearDrive Cloud — zero-setup, bare `bdrive login`, free personal
> workspace on signup — at [beardrive.ai](https://beardrive.ai). Or
> self-host your own hub.

On another machine:

```console
$ bdrive login https://your-hub && cd ~/workspace && bdrive init
# … connect the same project; the files appear and stay in sync
```

## What this looks like

Say your teammate Dana spent yesterday afternoon with her agent working out
why checkout fails for EU cards, and the agent wrote it down. Today you hit a
related bug. Here's the difference a shared folder makes.

**Without one**, your agent starts from nothing:

> **You:** why do EU checkouts fail?
>
> **Agent:** Let me investigate — searching the payment flow…

Some minutes and a few thousand tokens later it rediscovers what Dana's agent
already knew. Or it reaches a different conclusion and writes a second doc
that quietly contradicts hers.

**With BearDrive**, the folder is already fresh before the agent reads a
single file — the pull hook runs the moment you hit enter:

> **You:** why do EU checkouts fail?
>
> **Agent:** `shared/findings/eu-checkout.md` [🔗](https://your-hub/p/…)
> covers this. Dana's agent traced it to the 3-D Secure redirect dropping the
> session cookie on Safari, and the fix it proposes isn't applied yet. Want me
> to apply it?

When your agent writes its own findings down, the push hook puts them on the
hub seconds later, and Dana's next session starts from *your* work. Both
directions, no one exporting or pasting anything.

Because these are real files, the same folder is also a website: any file can
become a public link that renders as a page, the History view shows who
changed what from which device, and the Dashboard shows which docs your agents
actually read (and which hot ones nobody maintains).

## Features

- **Any folder is a project** — `bdrive init` turns any folder into a synced
  project. Files are *real files on disk*: every tool, editor, and agent can
  use them with zero integration work. Rename or move the folder freely —
  state is keyed by a stable id, never the path.
- **Multi-device sync** — devices converge through a shared hub. Each
  device only writes its own append-only journal, so no locking service is
  needed; the hub can be backed by any object store.
- **Change tracking** — `bdrive log` and the web UI's History view show
  which account changed which file, when, from which device (name, OS).
  Content is stored content-addressed, so every version is retained — view
  or download any point in a file's history. The feed can be narrowed by
  path, author and date range, and the narrowed view has its own URL.
- **Cloud-provider agnostic** — a hub can store on Amazon S3 (`s3://`),
  Google Cloud Storage (`gs://`), any S3-compatible store (MinIO, Cloudflare
  R2 via `AWS_ENDPOINT_URL`), or a plain shared directory (`file://`, e.g. a
  NAS). Clients never see it.
- **Offline-first** — the working folder is always fully usable with no
  network. Changes are journaled locally and pushed when the remote becomes
  reachable again.
- **Live** — against a hub, a teammate's change lands in about a second
  rather than on the next poll, and an open file in the web UI updates in
  place. The top bar shows who else is in the project, ringed when they are
  on the same file as you. Both ride one change stream; a hub that cannot
  serve it (older version, buffering proxy) falls back to polling and nothing
  else changes.
- **Conflict-safe** — concurrent edits resolve deterministically
  (last-writer-wins), and the losing version is preserved as a
  `name.bdrive-conflict-<device>-<time>` file. Nothing is silently dropped.
  Connecting a folder is not a concurrent edit: on the first sync, files the
  project already has (its `.bdriveignore`, `AGENTS.md`, anything a checkout
  brought along) keep the project's version — no conflict copies. Your copy is
  journaled as a superseded version, so `bdrive restore --list <path>` still
  has it.
- **Your agent's skills sync too** — `.claude/skills`, `.claude/commands`,
  `.claude/agents`, `AGENTS.md` and `CLAUDE.md` are ordinary files in the
  project, so a skill one person writes is on every teammate's disk before
  their agent's next turn — no export, no registry, no MCP server per client.
  Agent **hook** configuration never syncs: sharing what an agent *reads* is
  the product; sharing what it *runs* is not. Start a project from the
  `skills` template (`bdrive init --template skills`) for the shape.
- **Selective sync** — a gitignore-style `.bdriveignore` opts files out, and
  `bdrive init . --only wiki,docs` (or the interactive prompt) narrows a mount
  to some of its subfolders by writing those same rules for you.
- **macOS & Linux.**

## Install

BearDrive is meant to be set up by the agent that will use it. The fastest
path is to have your agent do it; the CLI route below is the same
destination, by hand.

### Have your agent set it up (recommended)

No terminal needed: start any agent (Claude Code, Codex, Gemini CLI, Hermes)
in the folder you want synced and give it one paste:

```
Follow beardrive.ai/setup to set up BearDrive. Ask me which folder to sync.
```

Keep the second sentence. Without it an agent reads the first as permission to
decide for you, and the usual guess is *this whole folder* — the one answer the
runbook tells it never to recommend. With it, you get a named recommendation to
accept or override: `shared/` from a standing start, or a notes folder it
already found.

Self-hosting? Say where: `… to set up BearDrive on https://hub.example.com.`
Joining a teammate's project? Use the paste on that project's home page in the
web UI instead — it carries the hub URL and project id, so the agent joins the
project rather than creating a second one beside it.

`beardrive.ai/setup` redirects to [INSTALL_FOR_AGENTS.md](INSTALL_FOR_AGENTS.md),
which the agent fetches and follows: install the CLI, then one `bdrive init` —
which signs in (an approval link when there is no local browser), registers the
sync hooks, and prints the project link. The instructions live at that URL
rather than inside the prompt so they never go stale in someone's copy, and the
agent handles every deviation (already installed, no Homebrew, sign-in, wrong
folder).

Those hooks are the whole integration, and `bdrive init` registers them in
each platform's user config (`~/.claude/settings.json` and friends), once per
machine, so every session in every folder is covered:

- a **blocking pull** when you send a message, so the agent always reads
  fresh team files — it also injects the project's link convention, so the
  agent appends a hub link to any synced path it mentions;
- an **async push** after every file edit, so artifacts are on the hub
  seconds after the agent writes them;
- **read tracking**, so the hub's Dashboard can show what your agents
  actually read.

Each hook no-ops instantly outside BearDrive projects, which is what makes a
machine-wide registration safe. `bdrive hooks` prints what's set up on this
machine; re-run it after a CLI upgrade.

### Install the CLI yourself

```sh
brew install runbear-io/tap/beardrive  # macOS (and Linuxbrew); installs the `bdrive` CLI
```

or from source:

```sh
go install github.com/runbear-io/beardrive/cmd/bdrive@latest
```

## Quick start

```sh
# 1. Sign this device in against your hub (once per device).
#    Self-host a hub in ~10 minutes (docs/self-hosting.md), then:
bdrive login
#    (BearDrive Cloud: sign up in the browser, get a free personal
#    workspace automatically. Self-hosting? bdrive login https://your-hub)

# 2. Start syncing a project — interactive: create or connect a project,
#    sync the whole folder or just ./shared. Re-run any time to resume.
cd ~/my-project && bdrive init

# 3. Work normally — create, edit, delete files with any tool.
echo "remember this" > memory.md

# On every other device: `bdrive login https://your-hub` once, then bdrive init in a folder
# and connect the same project.

# See what changed, who changed it, and from which device
bdrive log

# An agent clobbered a file? Put the old version back (as a new change)
bdrive restore memory.md

# Check sync state and the daemon
bdrive status

# Stop syncing — pauses everything, including agent turn hooks
# (files stay on disk; bdrive init resumes any time)
bdrive stop
```

Renaming or moving a project folder is safe: state is keyed by a stable
project id, never the path. The daemon notices the move, steps aside, and
the next `bdrive init` (or any bdrive command) at the new location resumes
exactly where it left off — zero re-scan, zero spurious changes.

### Credentials

beardrive uses each provider's standard credential chain — nothing beardrive-specific.
Note: **client devices always use an `https://` hub remote** — the
`s3`/`gs`/`file` rows below are how the *hub operator* configures the
hub's own storage, never something a syncing client points at directly:

| Remote | Credentials |
|---|---|
| `s3://bucket/prefix` | `AWS_PROFILE`, `~/.aws/credentials`, env vars, IAM roles. S3-compatible stores via `AWS_ENDPOINT_URL`. |
| `gs://bucket/prefix` | Application Default Credentials (`gcloud auth application-default login`) or `GOOGLE_APPLICATION_CREDENTIALS`. |
| `file:///path` | none — any local or network-mounted directory |
| `https://host:port/p/<id>` | none — syncs through a BearDrive hub; only the server holds storage credentials (see [The sync hub and `bdrive init`](#the-sync-hub-and-bdrive-init)) |

## Commands

| Command | Description |
|---|---|
| `bdrive login [server-url]` | Sign this device in (browser flow — the page names the account this terminal would act as and lets you switch before approving; `--device` forces the approval-link flow, and shells without a TTY fall back to it automatically; default server beardrive.ai — the managed cloud, free personal workspace on signup; pass your hub URL to self-host). Switch hubs with `bdrive login <new-url>` |
| `bdrive logout` | Sign this device out — revoke this device's token on the hub and clear it locally (`--forget` also drops the remembered server) |
| `bdrive init [folder]` | Create/connect a project and start syncing — the mount is always exactly the folder named. Interactive on a TTY, flags (`--name/--project/--server/--only/--template/--yes`) for scripts; `--template docs\|wiki\|para\|skills` starts the project from a structure (directories plus the `AGENTS.md` that explains them) instead of an empty folder; registers agent sync hooks and the login autostart in each platform's user config (`--no-hooks` skips the hooks), prints the project link; re-run to resume |
| `bdrive resume` | Restart the sync daemon for every project on this device that isn't paused — after a reboot, a crash, or a manual kill. Idempotent; this is what the login agent runs |
| `bdrive autostart [install\|uninstall]` | Show, add, or remove the login registration that runs `bdrive resume` after a reboot — a launchd user agent on macOS, a systemd user unit on Linux. `bdrive init` installs it; `--no-autostart` skips it |
| `bdrive stop [folder]` | Stop syncing, including agent sync hooks (files stay; `bdrive init` resumes) |
| `bdrive scope [add\|rm <dirs...>]` | Show or change which subfolders sync — edits the managed block of `.bdriveignore` rules that `init --only` writes, so no one hand-writes negation syntax. The daemon picks changes up in seconds; `rm` deletes nothing, locally or on the hub. `--explain` lists every path in the folder split into what syncs and what does not, so you can verify what leaves this machine (pure read — no daemon, no lock, no network) |
| `bdrive grep <pattern> [folder]` | Search the text **inside** the files a project syncs — Go RE2 regexp, or a literal with `-F`; `-i` ignores case, `-l` prints matching paths only, `-n` caps the lines printed (default 200, `0` = all). Output is `path:line: text`. Only files the project actually syncs are searched, so a `.bdriveignore` rule or a narrowed `bdrive scope` excludes a file from search exactly as it excludes it from sync; binary files are skipped. Pure local read — no daemon, no lock, no network, works offline and never blocks a sync in progress. Exit status 0 on match, 1 on none, so it composes in scripts |
| `bdrive stale [folder]` | Find docs the code has outgrown: synced markdown (`.md`/`.markdown`) that links to a file written **after** the doc itself. Staleness here is not age — a doc goes stale when what it describes moves. Write times come from the **journal**, not `os.Stat`: materialize stamps a peer's file with this device's mtime, so on a freshly synced machine every mtime is identical and only the journal still knows. `-l` prints outgrown paths only, `-n` caps the docs printed (default 50, `0` = all). A reference that does not resolve to a file this project syncs — a URL, a `../` escape, a made-up path — is silently ignored. Pure local read — no daemon, no lock, no network. **Exit status is 0 whether or not anything is stale**: this is advisory, not a gate |
| `bdrive forget <path>...` | Stop syncing a path *and* remove it from the hub — adds the rule to `.bdriveignore` (which syncs) and prunes in one step. Local files are never touched, here or on teammates' devices |
| `bdrive url [path]` | Internal hub link for a file/folder (sign-in + membership required; `--sync` pushes first, and warns on stderr if the hub refused that push; no arg = project home). Computed locally |
| `bdrive share <file>` | Public URL for a synced file (`--list`, `--revoke`, `--expires`) |
| `bdrive sync [folder]` | Run one sync cycle now. `--note <text>` stamps session context (e.g. an agent session id) onto changes — shown in `bdrive log` and hub history; keeps applying to daemon-committed changes until `--note-ttl` (default 30m) expires. A plain `bdrive sync` with no `--note` clears it, so a hand edit is never stamped with the last agent session's note. `--prune` also removes from the hub what `.bdriveignore` now excludes (files stay on disk everywhere). `--hook <label>` is agent-hook plumbing: event JSON on stdin, sync + note, gated-link formula (Claude Code hook JSON) on stdout |
| `bdrive hooks [install\|uninstall]` | Register turn-boundary sync hooks in each agent platform's user config (Claude Code, Codex, Gemini CLI, Hermes) — pull each turn, push after edits, session-note stamping, agent-read tracking. Once per machine, covering every session; run automatically by `bdrive init`; idempotent (`--agent` overrides detection) |
| `bdrive read-log [folder]` | Hook plumbing: queue agent file reads from a hook event (JSON on stdin) for the hub's read heatmap — native reads, grep matches, and files named in shell commands; drained on the next sync. Registered by `bdrive hooks install` |
| `bdrive status [folder]` | Projects, daemon state, two separate change counts — `pending` (journalled, not yet pushed) and `local` (on disk, not yet scanned — what a stopped daemon leaves invisible) — and any synced files that looked like they held credentials when they last changed. Pure local read: no ops, no journal writes, no network |
| `bdrive log [folder] [-p path] [-n N]` | Change history: account, device, time, file — newest first by the time shown, which is when the change was journaled; a file written more than a minute before it arrived (a rename, or an old document added today) also shows `written <time>` |
| `bdrive restore <file> [version]` | Put an earlier version of a file back, as a new change (`--list` shows the versions; no version = the previous one). Nothing is erased and it syncs everywhere like any edit. To un-create a file a run *created*, use **undo — remove file** on that row in the hub's History view — or **undo this run** in the run card's header to put back every file that run touched at once |
| `bdrive export [folder]` | Export the whole project — every device's journal, all blobs, full history — from its hub to a portable `.tar.gz` (`-o` names the file) |
| `bdrive import <archive>` | Import an export archive as a new project on the hub you're logged into (always a NEW project; `--name` overrides the archive's); history and authorship carry over. Refuses an archive whose journals reference content it doesn't hold (`--allow-incomplete` overrides). Move projects between hubs — cloud → self-hosted or back — with `export` + `login` + `import` |
| `bdrive serve [folder \| storage-root-url]` | Web server: viewer (rendered markdown, downloads, history), uploads, multi-project sync hub (`bdrive web` is a deprecated alias) |
| `bdrive whoami` | Signed-in account and device identity used in change tracking |
| `bdrive version` | Print the version (also `bdrive --version`) |

## Project files

Each mounted folder carries its own settings, so configuration travels with
the project:

- **`.bdrive/`** — the folder's settings directory: `config.json` holds the
  **stable mount id** plus the project and remote (and, on older mounts, a
  legacy `include` list — still honored, never written now). Written by
  `bdrive init`, safe to hand-edit (a running daemon picks changes up
  automatically). Never synced, and it holds no credentials (the session
  token stays in `~/.bdrive`). Because everything is keyed by the mount id,
  the folder can be renamed or moved freely; copy it to another machine and
  `bdrive init` resumes the same project.
- **`.bdriveignore`** — gitignore-style opt-out list at the mount root. Syncs
  like a normal file, so every device shares the same rules. Supports `#`
  comments, `*`, `**`, `?`, trailing `/` for directories, leading `/` (or any
  `/`) for root-anchoring, and `!` to re-include.

```jsonc
// .bdrive/config.json
{ "id": "m-5a10b713", "volume": "notes",
  "remote": "https://drive.example.com/p/7f3a2c91-4d5e-4b8a-9c17-2ad0f6b3e9c4",
  "post_sync": "qmd update && qmd embed" }
```

### `post_sync` — run something when teammates' changes land

Optional. A shell command run **on this device** after a cycle applies changes
from the hub, so a local search index, cache or notifier can stop polling. The
applied batch arrives as JSON on stdin:

```json
{ "project": "m-5a10b713", "folder": "/Users/you/notes",
  "changed": [ { "path": "wiki/onboarding.md", "op": "write" },
               { "path": "notes/retired.md",   "op": "delete" } ] }
```

Once per cycle that applied at least one path (an initial sync of 400 files is
one invocation), inbound only — a cycle that just commits and pushes your own
edits fires nothing — and never blocking: the command is spawned detached, and
a hook that hangs, exits non-zero or does not exist is logged and forgotten.
`bdrive restore` also counts as inbound, since it writes an older version back
into the folder.

It lives in `.bdrive/config.json` and only there. That directory never syncs,
so no hub and no teammate can put a command on your machine — but note that
`.bdrive/` does travel with a folder you copy by hand, so a folder copied from
a teammate brings their `post_sync` with it.

Opting out is non-destructive: when a pattern starts matching an
already-synced file, the file stops syncing but is deleted nowhere — which
also means the hub keeps the copy it already has. `bdrive forget <path>` (or
`bdrive sync --prune` for rules you added by hand) takes it off the hub, and
still deletes nothing on disk: every device receives the rule alongside the
removal and simply stops tracking the path.

## Web server

`bdrive serve` serves a website — browse folders and files, read markdown
rendered Obsidian-style (including `[[wikilinks]]`, task lists, tables,
and ```` ```mermaid ```` diagrams), download any file — and, pointed at a storage root, becomes a
**multi-project sync hub**. It is read-only unless started with `--upload`.

```sh
bdrive serve                              # serve the current directory (viewer)
bdrive serve ./notes                      # serve a folder from disk (viewer)
bdrive serve -c config.json               # everything from a config file
bdrive serve s3://my-bucket/root --upload # multi-project sync hub
```

With a folder it serves files straight from disk — on a BearDrive mount the
daemon keeps them fresh, so this is the simplest read-only deployment (no
cloud credentials on the serving machine). With a storage root URL it runs
in hub mode, described below.

Flags: `--addr` (default `:4173`), `--volume` (display name), `--refresh`
(listing cache, default `10s`), `--dir` / `--remote` (explicit forms of
the positional argument), `--upload` (allow client writes, off by default),
`--upload-ttl` (presigned-URL lifetime, default `15m`), `--projects-db`
(hub project registry file, default `$BDRIVE_HOME/projects.json`),
`-c/--config` (read all of the above from a JSON file; explicit flags win):

```jsonc
// bdrive serve -c config.json
{
  "remote": "s3://my-bucket/root",   // storage root (hub) — or "dir": "./folder" (viewer)
  "addr": ":4173",
  "upload": true,
  "upload_ttl": "15m",
  "refresh": "10s",
  "projects_db": "/var/lib/bdrive/projects.json",
  "share_rpm": 120,                  // per-IP rate limit on public /s/* links
  "trust_proxy": false,              // read X-Forwarded-For from a proxy on a PUBLIC address;
                                     // a proxy on loopback/private is already trusted
  "auth": {                          // optional knobs; hub auth is always on
    // Signup is invite-only by default. To allow self-service signup,
    // open it WITH a gate (an ungated open hub is refused at startup):
    "allow_signup": true,
    "allowed_domains": ["example.com"],  // only these domains may sign up
    "require_approval": true,            // …and an admin must approve each one
    "base_url": "https://drive.example.com",  // public origin for MAILED links (reset, verification)
    //   REQUIRED whenever smtp is set: without it the only other origin a link
    //   could carry is the Host header of the request that triggered it, which
    //   an anonymous stranger chooses. Unset = links go out root-relative.
    "users_db": "/var/lib/bdrive/auth.json",
    "admins": ["admin@example.com"],
    "smtp": { "host": "smtp.example.com", "port": 587,
              "user": "drive@example.com", "pass": "…", "from": "drive@example.com" }
  },
  "reads": {                         // read heatmap telemetry (hub mode)
    "enabled": true,                 // default true; aggregate counts only
    "retention_days": 400            // daily buckets older than this fold into all-time totals
  }
}
```

### The sync hub and `bdrive init`

In hub mode the server hosts many **projects** on one storage root — each
project's data lives under its own prefix (`<root>/<project-id>/`), and a
file-backed registry (`projects.json`, loaded at start, rewritten
atomically on every change) maps project ids to names. Client devices sync
whole folders through the hub without ever knowing where the storage is or
holding any cloud credentials; the server device is the only one configured
with the bucket.

Projects are walled by **organization**: every project belongs to one org
(file-backed `orgs.json`), and only that org's members — accounts with the
`owner` or `member` role — can see, browse, or sync it. Your first
`bdrive init` creates an org for you automatically; an owner invites
teammates from the web UI (the org name in the sidebar footer — Invite
mints an expiring join link, `/join/<token>`, that any signed-in account
can open to become a member). A hub upgraded from an earlier version
sweeps its existing projects into a `default` org that all existing
accounts join, so nothing breaks. Public share links stay outside the
wall on purpose.

Inside an org, each project carries its own **permissions** — four ordered
levels, edited under Project settings → People:

| Level | Can |
|---|---|
| `none` | nothing: the project is hidden — absent from the project list, every route denied |
| `read` | browse, view, download, history, read heat — and **pull**, so a device stays current |
| `write` | + upload, sync push, and minting/revoking share links |
| `admin` | + rename, delete, and edit this project's permissions |

The default is `write` for every org member, which is exactly the old
behavior — an upgraded hub changes nothing until someone edits
permissions. Setting the **default** to `No access` makes a project
invite-only: only explicit grants get in. Whoever creates a project becomes
its first admin, and **org owners are implicitly admin on every project in
their org**, so nobody can lock them out. Grants are org members only, and
a project always keeps at least one admin.

Two things follow on the **device** side, because a refusal is not the same
as being offline (see `bdrive status`):

- **read-only** — pushes are refused, so the daemon goes **pull-only**. Your
  local edits stay journaled on the device, never pushed and never lost;
  they go out if you're granted `write` again.
- **no access** — pulls are refused too, so sync **pauses**. Nothing is
  pulled, pushed, or written: revoking access never deletes or reverts a
  file on someone's disk. Re-granting resumes on the next tick.

`bdrive status` and `bdrive sync` print the hub's own sentence under either
one, as `reason:`. Read it: not every refused push is a permissions
question. `this device is not registered to your account on this hub` means
this machine's device identity was never bound to your account — update
`bdrive` and run `bdrive login` here. Project settings will show `write` and
explain nothing. Both states are recorded only by a cycle that actually
reached the hub, so the local-only ticks in between never revise the answer.

`bdrive sync` also **exits non-zero** when the hub refused this device's
changes. The blobs really do upload — only the journal is refused — so the
progress line reads `uploading 11 files` and the summary reads `local
changes: 0`, which a script, an agent, or a person skimming takes for
success. It isn't one: nothing reached the hub, and nothing will until the
refusal is dealt with. Being offline still exits 0; that one retries itself.

Public `/s/<token>` share links are **unaffected** by any of this: they are
anonymous by design and keep serving until revoked, so cutting someone's
access does not kill links they already minted.

```sh
# On the server device (knows the storage)
bdrive serve -c config.json

# On each client device (knows only the server) — one command does it all:
bdrive login https://drive.example.com:4173   # once per device
cd ~/some-project && bdrive init              # once per project
```

`bdrive login` signs the device in and remembers the server (`settings.json`
under the bdrive home; bare `bdrive login` defaults to beardrive.ai — the
managed cloud, where signup auto-creates a free personal workspace; pass
your hub's URL to use a self-hosted hub instead — `--status` shows the
current server and account). To move to a **different
hub**, run `bdrive login <new-url>` and then re-run `bdrive init` in each
folder to connect it to a project there; `bdrive logout` signs out entirely.
`bdrive init` then, per
project, walks you through it on a terminal: **create a new project or
connect an existing one** (picked from the server's list), **start from a
structure or from scratch**, and **sync the whole folder or only some of its
subfolders** (e.g. `./wiki`). Every question has a flag (`--name`,
`--project`, `--template`, `--only`, `--yes`), and without a TTY init never
prompts — it creates-or-joins a project named after the folder, empty, and
syncs everything.

`--template docs`, `wiki`, `para` or `skills` starts a **new** project from a
structure rather than an empty folder: a small directory skeleton plus the
`AGENTS.md` that tells an agent where a new note goes, when something is
archived, and what a good filename looks like — which is the part that keeps
a shared folder from rotting into a pile. The hub seeds it at creation, so it
is already there for the browser and for every device that connects later —
and `init` writes the same structure locally from its own copy, so the folder
has what the command said it would whatever the hub stored. Seeded files carry
no account and a "seeded from the … template" note, so change history tells
them from a file a teammate wrote;
joining a project that already exists never restructures it, and
`--template` is refused together with `--only` (scope rules live in the
synced `.bdriveignore`, so a scope that left out the template's folders would
hide them for the whole team). Creating a project in the web UI offers the
same starting points. It writes `.bdrive/config.json`, seeds a starter
`.bdriveignore` (node_modules, build dirs, caches, `.env*`), and starts the
daemon — local changes are detected within seconds, and the agent sync
hooks sync at every turn boundary. Not signed in yet? init runs the login
flow first.

Under the hood the `https://` remote speaks the hub's per-project
`/api/p/<id>/store` API — journal reads/writes relay through the server,
blob uploads go direct to the object store via the same short-lived
presigned URLs browser uploads use (falling back to relaying when the
backend can't presign). Client pushes and project creation require the
server to run with `--upload`; against a read-only hub, clients still pull
and `bdrive status` reports `access: read-only (pull only)` rather than
pretending to be offline.

### Sharing files by URL

For teammates, every synced file already has an internal link — the hub
viewer URL, gated by sign-in and the project's org membership:

```console
$ bdrive url wiki/report.html
https://drive.example.com/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d/wiki/report.html
```

It's computed locally (no network), always shows the latest synced
content, and is the link agents should drop in their replies when they
create an artifact in the shared folder (`--sync` pushes first so a
just-created file resolves immediately).

For people **outside** the hub, any synced file can instead be shared
with a public link — hand someone the URL and they see the file, no
account needed:

```console
$ bdrive share wiki/report.html
https://drive.example.com/s/eacc1df3ee6a6ebbdacc535c2796dc30
```

Links are **per-file** — there is no folder link, and `bdrive share` on a
folder says so and names a file inside it to share instead. Links always
serve the file's **latest** synced content (right for wiki pages and
living reports), and live until revoked — `bdrive share --list`
and `--revoke <token-or-url>` manage them, `--expires 24h` makes one
self-destruct. The web UI has a Share button on every file, and its
dialog can put an expiry on the link it just minted (24 hours, 7 days,
30 days) without changing the URL you already copied.

Shared HTML renders as a real page, markdown renders like the viewer
(with a small "Shared with BearDrive" footer; raw HTML is served
byte-for-byte), PDFs open inline. Rendering is sandboxed: `/s/*` responses
carry a strict CSP, never see auth cookies, and sit behind a generous
per-IP rate limit (`share_rpm`), so a malicious shared file's scripts
can't touch hub sessions and a scraper can't turn the hub into a CDN.
Any org member can mint links, and a link is public to whoever has the
URL — note that a LAN-bound hub means LAN-only links.

Before it mints, the hub reads the **first 1 MiB** of the file and refuses
if it finds credential-shaped strings — an AWS access key, a private key
block, a GitHub, Slack, GitLab or OpenAI token. It names the rule and the
line, never the matched text, and nothing is shared:

```
$ bdrive share deploy.md
Error: deploy.md looks like it contains credentials (checked at the moment you shared it):
  line 12   an AWS access key (aws_access_key_id)
  line 40   a private key (private_key)
Nothing was shared. Re-run with --force if that is intentional.
```

`--force` shares it anyway (the web UI's Share button asks the same
question and offers **Share anyway**). Know the two limits: the check runs
**at the moment you share**, and a link serves the file's latest content
forever — so a key committed into an already-shared file is never caught —
and it only reads the first 1 MiB. It shortens the odds; it is not a
promise that a file is clean.

The same six rules also run **on every file as it syncs**, which is the path
every file takes rather than the rare one. They only ever warn: the change is
journaled and pushed exactly as it would be otherwise, and the finding shows up
in `bdrive status` and in the agent's context at the start of its next turn.

```
$ bdrive status
  secrets: 1 file(s) looked like they contain credentials when they last changed
             deploy.md:12  an AWS access key
```

Nothing is blocked, held, or un-pushed — a false positive costs a line of text,
never a stalled file. Fixing the file is the whole remedy: the next sync cycle
reads it again and the line goes away, with no command and no flag. Same limits
as above — a file is checked **when it changes**, and only its first 1 MiB, so
this never says a file is clean. It runs on the device that made the change:
a file another device synced before its version had this check stays unflagged
until it next changes here.

### Agent integration

Setup is conversational — one paste and the agent does the rest, hooks
included ([Have your agent set it up](#have-your-agent-set-it-up-recommended)).
The payoff, once those hooks are in place: "write a report and share it"
becomes the agent generating `wiki/report.html` and replying with a link.

The web UI lists your orgs' projects in the sidebar (⌘K opens a command
palette: fuzzy file search, project switching, the project's own pages, and
history — plus share and download of the file you have open); selecting one
browses that project's files, and the **History**
view shows every change — which
account made it, when, from which device (name and OS — never the connecting
IP), with view/download of any past version (content is
content-addressed and retained forever; reverting to a version is the next
phase and the API is already shaped for it). Folder rows have a history
shortcut for a subtree feed; the topbar button shows the current file's
versions or the whole project feed. A filter bar above the feed narrows it
by path substring, author and date range (UTC) — the filters ride in the
URL, so a narrowed feed is a link you can send, and they are applied
server-side, so paging through a filtered feed stays correct.

Hubs also track **read heat**: viewer opens and downloads count as human
reads, share-link hits as share reads, and agent tool reads (reported by
the sync hooks via `bdrive read-log`) as agent reads — sync replication
never counts. Folder listings show heat dots and 30-day read counts to
every member, and every member gets the project **Dashboard**
(sidebar or ⋯ menu), four sections with an all/human/agent lens: a **treemap** of
every file (cell size = reads, color = staleness, ⚠ on hot+stale — click
through to any file), the **reads × freshness** scatter whose hot-but-stale
quadrant is the knowledge people rely on that nobody maintains, the
**hot path** (top files by reads, agent/human split — effectively the
team's agent context window), and an **agent coverage matrix** (which
agent devices read which folders). The API
(`GET /api/p/<id>/heat?prefix=&days=`) exposes only aggregate counts,
distinct-reader counts, and last-read times — never who read what;
`?by=device` adds the agent-only per-device folder breakdown (device
identity is already public via history; human emails never appear).

### Authentication & database

Hubs always require sign-in — every change is attributed to a real
account. **Signup is invite-only by default** (the safe posture for a
public URL); self-service signup opens only with a gate (admin approval,
or allowed domains + email verification). Hub metadata (accounts,
projects, orgs, shares) lives in a file-backed store by default, or
SQLite/Postgres (incl. Supabase) via the `database` config block.

Full reference — the three signup postures, SMTP, admins, CLI device
sign-in, and database selection: **[docs/self-hosting.md](docs/self-hosting.md)**.


### Uploads

The browser client is deliberately storage-blind: it never sees the remote
URL, bucket, or any credentials. On page load it fetches `/api/config` and
follows whatever the server allows.

With `--upload` set, the server decides per upload how the bytes travel:

- **Direct** — for backends that can presign (S3 and S3-compatible stores;
  GCS when the server runs with credentials that can sign, e.g. a service
  account): the server mints a short-lived presigned `PUT` URL for the
  content-addressed blob (`blobs/<sha256>`), the browser uploads straight
  to the object store, then asks the server to commit. The commit verifies
  the blob actually exists and appends a `put` op to the *server's own*
  journal — the blobs-before-journal ordering and the one-writer-per-journal
  invariant both hold. Expired URLs are refused by the store; the client
  just re-runs init. Direct uploads to a bucket also need a CORS rule on
  the bucket allowing `PUT` from the viewer's origin.
- **Through the server** — `file://` remotes and plain-folder serving can't
  presign, so the client sends content to the server, which stores it
  (object store + journal, or straight to disk for a served folder, where
  the daemon will pick it up like any local edit).

## How it works

```
working folder  ←materialize/scan→  local volume store  ←push/pull→  object store
 (real files)                       ~/.bdrive/volumes/<vol>              s3:// gs:// file://
                                    ├─ blobs/   content-addressed (sha256)
                                    ├─ journal/ one append-only op log per device
                                    ├─ state.json  what's materialized
                                    └─ sync.json   lamport clock + push cursor
```

- Every change becomes an **op** (`put`/`delete`) in this device's
  append-only journal, stamped with a lamport clock, wall-clock time, device
  ID, and author. File content goes into a content-addressed blob store.
- A **sync** uploads new blobs, then the journal; it downloads other
  devices' journals and any blobs it's missing. Since each device writes
  only its own journal, there are no concurrent writers per object and any
  dumb object store suffices.
- Files **larger than 4 MiB move as content-defined chunks** (delta sync):
  the remote holds `chunks/<sha256>` pieces plus a `manifests/<sha256>`
  chunk list keyed by the whole file's hash, and a push uploads only the
  chunks the store doesn't already hold — a small edit to a large file
  transfers roughly one chunk (~1 MiB), not the file. Chunk boundaries are
  chosen by a rolling hash, so insertions don't shift them. The local blob
  store keeps files whole; chunking exists only on the wire and in the
  remote. Older clients are unaffected: the hub reassembles whole blobs on
  demand for anything that asks for `blobs/<sha256>`.
- The folder's state is a deterministic **replay** of all journals ordered
  by `(lamport, time, device)` — every device converges to the same view.
  Concurrent edits keep the last writer at the path; the loser is preserved
  as a conflict-copy file by the device that detects the overlap.
- A per-mount **daemon** scans the folder every few seconds (cheap
  size+mtime check) and exchanges with the remote every ~10s — or
  immediately after local edits. Tunable with --scan-interval and
  --remote-interval on the daemon (defaults 3s / 10s).
- Against a hub, the daemon also holds a **live change stream** open, so a
  teammate's push arrives in about a second instead of waiting out the
  remote interval. It is an accelerator only: a hub that is down, too old to
  serve the stream, or behind a proxy that buffers it simply falls back to
  the intervals above, and object-store remotes never had it to begin with.

### What beardrive does not sync

`.git` directories (per-file LWW would corrupt repositories), `.DS_Store`,
the `.bdrive` settings file, agent **hook** configuration
(`.claude/settings.json` and `settings.local.json`, `.codex/hooks.json` and
`config.toml`, `.gemini/settings.json`, `.hermes/config.yaml`, and `.mcp.json`
— a hook is a
shell command, and syncing one would let a teammate install it on your
machine; BearDrive's own hooks live in each machine's user config. Everything
else under those directories — skills, commands, agents — syncs normally),
its own temp files, nested mounts (a
subdirectory with its own `.bdrive/config.json` syncs only through its own
project — the parent never scans into it, writes over it, or propagates
deletes for it), and anything excluded by `.bdriveignore` or omitted from an
`include` list. Empty directories are not tracked (like git).

## Roadmap

See [ROADMAP.md](ROADMAP.md) — the public, dated roadmap, including the
items we'd love help with. Highlights: `beardrive restore <path>@<time>`
(time travel — all content is already retained), FUSE/NFS mount mode,
journal compaction & blob GC, per-path access scopes for multi-agent
setups.

## Development

Contributions welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for the
build/test workflow and the rules that matter, [ROADMAP.md](ROADMAP.md)
for where help is wanted, and [CHANGELOG.md](CHANGELOG.md) for what
shipped when. Self-hosting a hub: [docs/self-hosting.md](docs/self-hosting.md).

```sh
go build ./...
go test ./...
```

The integration tests in `internal/syncer` simulate multiple devices syncing
through a `file://` remote, including offline operation and concurrent-edit
conflicts. Set `BDRIVE_HOME` to relocate all beardrive state (used heavily in tests).

### Web frontend

The hub's web UI is a React + TypeScript app in `internal/webapp/frontend`
(Vite + Tailwind v4 + shadcn/ui components owned in-repo; TanStack
query/table/virtual, react-hook-form + zod, cmdk, sonner, lucide-react —
routing stays a small in-repo history router). Its
**built output is committed** at `internal/webapp/static`, the `go:embed`
target, so building or `go install`-ing the binary never needs Node.

Only when changing `frontend/src`:

```sh
cd internal/webapp/frontend
npm install
npm run dev       # hot-reload dev server, proxying /api to a local hub
                  # (BDRIVE_DEV_PROXY=http://localhost:8993 to point elsewhere)
npm run build     # rebuild internal/webapp/static — commit the result
npm run e2e       # Playwright suite; starts its own seeded hub on :8993
./check-dist.sh   # verify the committed static/ is fresh (pre-release check)
```

## License

GNU AGPL-3.0 — Copyright 2026 Runbear, Inc. See [LICENSE](LICENSE).

We chose AGPL-3.0 deliberately: it keeps BearDrive fully open and
self-hostable forever while preventing a cloud provider from offering a
closed BearDrive-as-a-service. The managed service at beardrive.ai funds
the project; the code stays open.

Everything in this repo is open source and self-hostable: a complete BearDrive
server for one organization's deployment, teams included. The managed service
at beardrive.ai is the same core plus what only makes sense as an operated
service — hosting, PropelAuth SSO, billing and plan quotas, backups, and
support. Provider-specific and billing code stays out of this repo permanently;
the server exposes interfaces (`AuthProvider`, `QuotaProvider`) that the
managed deployment fills in.
