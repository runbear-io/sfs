---
title: How sync works
description: Journals, blobs, deterministic replay, and why no locking service is needed.
---

Data flows in two hops, and the local volume store is the pivot.

```
working folder  ←materialize/scan→  local volume store  ←push/pull→  object store
 (real files)                       ~/.bdrive/volumes/<vol>              s3:// gs:// file://
                                    ├─ blobs/   content-addressed (sha256)
                                    ├─ journal/ one append-only op log per device
                                    ├─ state.json  what's materialized
                                    └─ sync.json   lamport clock + push cursor
```

## Ops and journals

Every change becomes an **op** — `put` or `delete` — in this device's
append-only journal, stamped with a lamport clock, wall-clock time, device id,
account, and author. File content goes into a content-addressed blob store.

**Each device writes only its own journal.** This is the entire concurrency
story: no locking service is needed because no object ever has two writers, so
any dumb object store suffices.

## A sync cycle

One cycle is a single pass:

1. **Scan** the working folder — a cheap size and mtime check against the
   materialization cache.
2. **Commit** local ops, capturing content into blobs.
3. **Pull** peer journals and any blobs it's missing.
4. **Preserve** conflict copies.
5. **Materialize** the merged state back to disk.
6. **Push** blobs, then this device's journal.

Two orderings matter. **Blobs are pushed before the journal**, so a peer never
sees an op whose content is missing. **Scan happens before pull**, so local
edits are journaled before remote state can overwrite the working folder.

## Convergence

The folder's state is a deterministic **replay** of all journals ordered by
`(lamport, time, device, seq)`. Every device folds the same ops in the same
order and converges to the same view, last-writer-wins per path.

Concurrent edits keep the last writer at the path. The loser is preserved as a
`name.bdrive-conflict-<device>-<time>` file by the device that detects the
overlap. Nothing is silently dropped.

**Joining is an adoption, not a merge.** On a device's first sync of a project,
a local file at a path the project already holds is not a concurrent edit — it
is a folder that happened to contain the same path (a checkout of the same
docs, an agent-written `AGENTS.md`, the `.bdriveignore` `bdrive init` seeds).
The project's version wins on every device and no conflict copy is made. Your
copy is still journaled and pushed as a superseded version of that path, so
`bdrive restore --list <path>` can bring it back. `bdrive sync` reports these as
`adopted`.

## The daemon

A per-mount daemon scans the folder every few seconds and exchanges with the
remote every ~10s — or immediately after local edits. Tune with
`--scan-interval` and `--remote-interval` on `bdrive daemon run` (init always
uses the defaults).

It re-reads `.bdrive/config.json` each tick. If that file vanishes because the
folder was moved, renamed, or deleted, the daemon **exits cleanly without
propagating deletes**. The next bdrive command at the new location resumes it.

## The change stream

Against a hub, the daemon also holds a long-lived connection open and syncs
when the hub says a peer pushed, rather than waiting out the remote interval —
so a teammate's edit lands in about a second instead of ten. The browser
listens on the same stream: the file tree updates, an open file re-renders in
place, and the top bar shows who else is in the project.

This is an **accelerator, not a mechanism**. Everything above still runs
underneath it, so nothing depends on a notification arriving. An older hub, a
hub that is down, a proxy that buffers the stream, or an object-store remote
with no hub at all falls back to the intervals and syncs exactly as it always
did. (Self-hosting behind a proxy? See
[Run a hub](/self-hosting/run-a-hub/).)

Presence is deliberately thin: a heartbeat every ten seconds naming the file
you are on, held for fifteen, never written to disk. It carries display names
and paths — the same pair history already shows every project member — and is
readable only by members of that project.

## Safety properties

These hold by design, and the integration tests exist to keep them holding:

- **Materialize never clobbers dirty files.** A file whose size or mtime differs
  from the state cache changed mid-cycle and is left for the next scan.
- **All state files are written atomically** — temp file plus rename.
- **Cycles are serialized by a volume flock**, so the daemon and one-shot
  `bdrive sync` coexist.
- **Errors degrade rather than fail.** Pull and push errors mark the cycle
  offline; unreadable or vanished files during scan are skipped and retried next
  cycle. Sync never breaks — it retries.

## Retention

Content is content-addressed and blobs are retained forever, which is what makes
history complete: any past version can be viewed or downloaded, and reverting to
one is just re-putting an old blob as a new op.
