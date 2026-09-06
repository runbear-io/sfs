---
title: Hooks in detail
description: What bdrive hooks install actually writes — the user-level config paths, hook events, idempotency, and when to re-run it.
---

`bdrive init` runs this for you — that is why setup is one command. This is what
it does, for when you want to run it yourself, review what changed, or debug a
folder that isn't syncing.

## The hooks

```sh
bdrive hooks install            # every detected platform, this machine
bdrive hooks install --agent claude,codex,gemini,hermes
bdrive hooks                    # status table
bdrive hooks uninstall          # remove them again
```

Each platform gets the same three hooks written into its own user-level config,
in its own format:

| Platform | Config it writes | Pull / push / read events |
|---|---|---|
| Claude Code | `~/.claude/settings.json` | `UserPromptSubmit` / `PostToolUse` (Write\|Edit\|MultiEdit) / `PostToolUse` (Read\|Grep\|Bash) |
| Codex | `~/.codex/hooks.json` | `UserPromptSubmit` / `PostToolUse` (apply_patch) / `PostToolUse` (read_file\|shell) |
| Gemini CLI | `~/.gemini/settings.json` | `BeforeAgent` / `AfterTool` (write_file\|replace\|edit) / `AfterTool` (read tools) |
| Hermes | `~/.hermes/config.yaml` | `pre_llm_call` / `post_tool_call` (write_file\|patch) / `post_tool_call` (read_file\|grep\|bash) |

Three hooks, three jobs:

- **Pull**, before the agent answers, so it never reads a stale file. This one
  blocks — it is the only place BearDrive makes you wait, and it is why the
  whole thing works. It also hands back the turn's brief: hub links for the
  synced folders, what teammates changed since the last turn, credential-shaped
  files, the docs this project's code has outrun, and the project's `AGENTS.md`
  if it syncs ([Shared agent memory](/guides/shared-agent-memory/)). All of it
  is local; none of it can fail the turn.
- **Push**, after an edit, so teammates see the change within seconds rather
  than whenever a daemon tick lands.
- **Read tracking**, on the agent's read-shaped tools, queued locally and sent
  on the next sync. This is what fills the [Dashboard](/guides/what-agents-read/).
  Listing tools are deliberately excluded: seeing a filename is not reading it.

Every platform pipes hook JSON with a session id, so one hook command serves all
four, and changes are stamped with `<agent> session <id>` — visible in
`bdrive log` and the hub's history.

Codex hooks are experimental and off by default. Turn them on in
`~/.codex/config.toml`:

```toml
[features]
codex_hooks = true
```

Codex then asks once to trust the hook definition. Answer yes.

## Both are safe to re-run

Merging is idempotent and preserves hooks you already have. Each hook carries
its own marker, so a config written before a hook existed gains just the missing
one, and a registered hook's matcher is upgraded in place when coverage grows.

Re-run `bdrive hooks install` after a CLI upgrade, once per machine.

## Where they live matters

Hooks are registered **once per machine**, in each platform's own user config —
never inside a project. Agent platforms read hook config only from the directory
a session starts in: never a parent, never a subfolder. A file in the project
would fire only for the sessions that happened to start exactly there, and — if
the project is synced — would travel to the whole team. A user-level
registration covers every session in every folder instead.

So BearDrive writes no agent-config directory into your project, and teammates
don't inherit your hooks: each device registers its own the first time it runs
`bdrive init`. Earlier versions did write project-level hooks; installing strips
those out as it goes, so nothing ends up running twice.

The hook opens with a shell guard that makes it a fast no-op in any folder
without a `.bdrive/` directory, which is what makes registering it globally safe.

`bdrive hooks uninstall` takes them back out — it removes only BearDrive's own
entries and leaves every other hook in those files untouched. Syncing itself is
unaffected; only turn-boundary sync stops.

## Surviving a reboot

The sync daemon is an ordinary background process, so a restart ends it. `bdrive
init` therefore also registers a login item that runs `bdrive resume`, which
starts a daemon for every project this device syncs and has not paused:

```sh
bdrive autostart              # is it registered?
bdrive autostart install      # register it (init already did)
bdrive autostart uninstall    # stop starting sync at login
bdrive resume                 # start the daemons right now
```

One registration covers every project — add or remove projects freely, nothing
to re-register. Projects paused with `bdrive stop` stay paused; only `bdrive
init` resumes those.

Where it lives, per platform — user-level either way, no `sudo`, nothing
machine-wide:

| Platform | What gets written |
|---|---|
| macOS | `~/Library/LaunchAgents/ai.beardrive.daemon.plist` (launchd loads it at login) |
| Linux | `~/.config/systemd/user/beardrive.service` plus the `default.target.wants` symlink that enables it (honors `XDG_CONFIG_HOME`) |

Linux needs systemd as the init system. Without it — Alpine or another
runit/OpenRC distro, WSL1, a slim container — `bdrive autostart` says so rather
than writing a unit nothing would read.

On macOS, the moment that file is written you get a **"Background Items Added"**
notification, and `bdrive` appears in System Settings → General → Login Items.
That notice is macOS reporting the registration above — every way of starting
at login triggers it, including Apple's own `SMAppService` and a plain
`crontab` — so `bdrive init` asks first on a terminal and says what is about to
happen when it can't ask. Answer no, or run `bdrive autostart uninstall`, and
the item goes away; sync then resumes on the next `bdrive resume`, `bdrive
init`, or agent turn instead of at login.

Either way this is not the only thing that recovers sync: an agent turn in a
project syncs it too, so a machine you actually work on catches up on its own.
