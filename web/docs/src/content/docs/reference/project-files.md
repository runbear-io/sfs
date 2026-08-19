---
title: Project files
description: The .bdrive settings directory and .bdriveignore, plus where global state lives.
---

Each synced folder carries its own settings, so configuration travels with the
project.

## `.bdrive/`

The folder's settings directory. `config.json` holds the **stable mount id**
plus the project and remote (older mounts may also carry a legacy `include`
list — still honored, never written now).

```jsonc
// .bdrive/config.json
{ "id": "m-5a10b713", "volume": "notes",
  "remote": "https://drive.example.com/p/7f3a2c91-4d5e-4b8a-9c17-2ad0f6b3e9c4" }
```

Written by `bdrive init` and safe to hand-edit — a running daemon picks changes
up automatically. Which subfolders sync is *not* stored here: that lives in
`.bdriveignore` (see below), edited with `bdrive scope add`/`rm`. Mounts created
before that change may still carry an `include` list here; it is still honored,
but nothing writes it any more.

It is **never synced** and holds **no credentials**; the session token stays in
`~/.bdrive`.

Because everything is keyed by the mount id, the folder can be renamed or moved
freely. Copy it to another machine and `bdrive init` resumes the same project.

### `post_sync`

An optional shell command run **on this device** after a sync applies changes
from the hub — the event a local search index, cache or notifier can hang off
instead of polling.

```jsonc
// .bdrive/config.json
{ "id": "m-5a10b713", "volume": "notes",
  "remote": "https://drive.example.com/p/7f3a2c91-…",
  "post_sync": "qmd update && qmd embed" }
```

The applied batch arrives as JSON on stdin, with the command's working
directory set to the folder:

```json
{ "project": "m-5a10b713", "folder": "/Users/you/notes",
  "changed": [ { "path": "wiki/onboarding.md", "op": "write" },
               { "path": "notes/retired.md",   "op": "delete" } ] }
```

- **Once per cycle** that applied at least one path — an initial sync of 400
  files is one invocation, not 400.
- **Inbound only.** A cycle that only commits and pushes your own edits fires
  nothing. (`bdrive restore` does count: it writes an older version back into
  the folder.)
- **Never blocks sync.** The command is spawned detached; one that hangs, exits
  non-zero or does not exist is logged to `daemon.log` and forgotten.
- **Off unless set.** No `post_sync` key, no new behavior.

This is local configuration and nothing else can set it: `.bdrive/` never syncs,
so no hub response and no teammate's change can put a command on your machine.
The one path in is your own hands — `.bdrive/` travels with a folder, so a
folder you copy from a teammate brings their `post_sync` along with it.

## `.bdriveignore`

A gitignore-style opt-out list at the mount root. It syncs like a normal file,
so every device shares the same rules. See
[Scoping the folder](/guides/scoping/).

## Paths BearDrive never carries

Some paths are excluded in **both** directions — never scanned, never uploaded,
never written onto a teammate's disk — regardless of `.bdriveignore`:

| Path | Why |
|---|---|
| `.bdrive/` | The mount's own identity. Syncing it would let one device repoint another. |
| `.git/` | Carries hook scripts that would run on a teammate's next commit. |
| `.claude/settings.json`, `.claude/settings.local.json`, `.codex/hooks.json`, `.codex/config.toml`, `.gemini/settings.json`, `.hermes/config.yaml` | Agent **hook** configuration is a shell command a teammate would be installing on your machine. BearDrive's own hooks go in each machine's user config instead. |
| `.mcp.json` | Project-scoped MCP servers: `command` + `args` pairs your agent LAUNCHES when a session starts in the folder. Same reason as the hooks above. |
| `.DS_Store`, `.bdrive-tmp-*` | Noise and in-flight temp files. |

Everything else under an agent-config directory — `.claude/skills`,
`.claude/commands`, `.claude/agents`, `AGENTS.md`, `CLAUDE.md` — syncs
normally. Sharing what an agent *reads* is the product; sharing what it *runs*
is not. See [What agents read](/guides/what-agents-read/).

## And nothing else

Those two are all BearDrive puts in a project: `.bdrive/config.json`,
`.bdriveignore`, and your own files. (A project created from a template also
starts with an `AGENTS.md` and a directory skeleton — but those are ordinary
synced files, yours to edit or delete like any other; nothing reads them but
your agents.) No agent-config directory is ever created
here — the sync hooks live in each platform's user config
(`~/.claude/settings.json`, `~/.codex/hooks.json`, `~/.gemini/settings.json`,
`~/.hermes/config.yaml`), written once per machine. See
[Hooks in detail](/manual/hooks/).

## Global state

Everything else lives under `$BDRIVE_HOME` (default `~/.bdrive`):

| Path | Contents |
|---|---|
| `device.json` | This device's identity, used in change tracking |
| `settings.json` | Default server, device token, signed-in account |
| `mounts.json` | Mount registry, keyed by stable mount id, holding each mount's last-known path |
| `volumes/<mount-id>/` | The local volume store: blobs, journals, materialization cache, sync state |

Nothing is keyed by folder path, which is why moves and renames are free.
`ResolveMount` self-heals the registry path on the next command.

## The volume store

```
~/.bdrive/volumes/<mount-id>/
├─ blobs/      content-addressed file content (sha256)
├─ journal/    one append-only op log per device
├─ state.json  what's materialized
├─ sync.json   lamport clock + push cursor
└─ secrets-<mount-id>.json  files that looked like they held credentials when
                            they last changed (what `bdrive status` reports)
```

Also here for a running project: `daemon.pid` and `daemon.log`.

The hub's storage adds two key classes the local store never holds: files
larger than 4 MiB travel as content-defined `chunks/<sha256>` pieces plus a
`manifests/<sha256>` chunk list keyed by the whole file's hash (delta sync —
a small edit to a large file uploads roughly one chunk, not the file). Local
blobs stay whole; chunking exists only on the wire and in the hub's store,
and the hub reassembles a whole blob on demand for any client that asks for
`blobs/<sha256>`, so older clients keep working unchanged.
