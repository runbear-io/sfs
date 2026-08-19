---
title: What agents read
description: The hub's read heat and project Dashboard — which documents your agents actually consume, and which ones everyone relies on and nobody maintains.
---

Writing to shared memory is easy to observe. *Reading* is the part that's
normally invisible — and it's the part that tells you whether the folder is
working.

Hubs track it.

## Three kinds of read

| Kind | Source |
|---|---|
| **Human** | Viewer opens and downloads |
| **Share** | Public `/s/*` link hits |
| **Agent** | Agent tool reads, reported by the sync hooks via `bdrive read-log` — native reads, grep matches, and files named in shell commands |

Sync replication never counts as a read, and neither does viewing a blob in
history. Only genuine consumption.

That includes your own: a member opening a file in the hub records a human
read under their own account, so on a small or new project an author checking
their own page can be a real share of the count. Repeat opens by the same
reader inside 10 minutes count once. Every surface that prints a read count
says so beside it.

Agent reads require the hooks from [Set up with your agent](/start/setup/) —
registered once per machine, in the agent's own user config, so every device
whose agents you want counted needs its own `bdrive init` (or `bdrive hooks
install`). Without them you'll see human traffic only.

## In the file browser

Folder listings show heat dots and 30-day read counts to every member. It's the
fastest way to tell which parts of a knowledge folder are load-bearing and which
are decoration.

## The project Dashboard

Every project member gets a **Dashboard** with an all/human/agent
lens — four views:

- **Treemap** — every file, cell size by reads, color by staleness, with ⚠ on
  hot-and-stale. Click through to any file.
- **Reads × freshness scatter** — the hot-but-stale quadrant is the important
  one: knowledge the team relies on that nobody maintains. That quadrant is a
  worklist.
- **Hot path** — top files by reads with the agent/human split. Effectively your
  team's agent context window, measured rather than assumed.
- **Agent coverage matrix** — which agent devices read which folders. Useful for
  spotting an agent that never discovered the folder at all, which usually means
  a missing [root pointer](/guides/shared-agent-memory/).

## One session, read and written

History groups an agent session's changes into a single **run card**. The card
also shows what that session *read*: files it read before changing them are
marked, and files it read without touching at all get their own **Read, not
changed** list underneath.

That's the question the heat map alone can't answer — *when my agent answered,
what did it actually look at, and was it the current version or the retired
one?*

Two things worth knowing:

- Reads are shown only for files the project still has. A file a run read and
  then deleted appears as a change with no read. The card says so on screen.
- The join is on a session id the sync hook stamps, never on the run's note —
  the note is free text anyone can set with `bdrive sync --note`, so joining on
  it would let one person's changes attach to another person's card.

### Undoing a whole run

The card's header carries **Undo this run**: one click puts back every file
that run touched. A file it edited returns to the content it had just before
the run; a file it created is removed. The confirm lists every path with what
will happen to it before anything is written, so you can read the whole thing
and cancel.

Two things it says out loud, because they are the ones that can surprise you:

- **A file someone changed after the run is reverted too**, and the confirm
  counts them. That is the same last-writer-wins rule the rest of BearDrive
  follows, but it is worth seeing before you click.
- **A file already holding its pre-run content is skipped**, not written —
  reported as skipped rather than as a failure.

Nothing is erased. The undo is new changes appended to history like any other,
written in a single batch, so it is itself a run card you can undo. Undoing
needs write access on the project; a read-only member sees no button.

Per-session detail is kept for 30 days by default
(`reads.session_retention_days`, see [Hub config](/reference/hub-config/));
after that the run card shows changes only. Read *counts* are unaffected — they
come from the heat buckets, which have their own, much longer retention.

## Using it

A few things this surfaces that are otherwise guesswork:

- **A document with agent reads and a stale timestamp** is your highest-value
  edit. Agents are grounding answers in it and it's out of date.
- **A document with zero reads after weeks** either isn't discoverable or isn't
  needed. Check `AGENTS.md` mentions it before deleting it.
- **An agent device with narrow coverage** hasn't been oriented. Its machine is
  probably missing the repo-root pointer.

## Privacy

The API (`GET /api/p/<id>/heat?prefix=&days=`) exposes only aggregate counts,
distinct-reader counts, and last-read times. **Never who read what.**

`?by=device` adds an agent-only per-device folder breakdown — device identity is
already public via history. Human email addresses never appear in a heat
response.

A session id is treated the same way. It appears only in History, on the change
that carries it, and is accepted as a `?session=<id>&device=<id>` filter
**input** — both are required. Nothing enumerates sessions: no listing
endpoint, no session column in heat output, nothing new in `?by=device`. And a
session's reads are always recorded against the device the hub validated the
report came from, so nobody can paint files onto a teammate's run card.

Telemetry degrades silently: recording or flushing a read can never fail a
request or a sync cycle.

Hub operators can turn the whole thing off with `"reads": { "enabled": false }`.
See [Hub config](/reference/hub-config/).
