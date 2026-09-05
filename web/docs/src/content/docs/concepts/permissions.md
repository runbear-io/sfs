---
title: Project permissions
description: Four levels per project — none, read, write, admin — plus per-folder rules, invite-only projects, and what a read-only or revoked device actually does.
---

Organizations decide who is in your workspace. **Project permissions** decide
what each of those people can do with each project.

Nothing here changes an existing hub until you use it: the default is `write`
for every workspace member, which is exactly the behavior BearDrive has always
had.

## The four levels

Higher includes lower.

| Level | Can |
|---|---|
| `none` | nothing — the project is hidden: absent from the project list, every request denied |
| `read` | browse, view, render, download, per-file history, read heat — and **pull**, so a device stays current |
| `write` | everything in `read`, plus upload, sync push, and creating or revoking share links |
| `admin` | everything in `write`, plus rename the project, delete it, and edit its permissions |

Edit them in the hub UI: **Project settings → People**. Everyone with access
sees the section; only an admin gets live controls.

## Who is what

- **The default** applies to every workspace member without an explicit grant.
  It starts as `write`.
- **Exceptions** are per-person grants. They are limited to members of the
  project's workspace — there are no outside collaborators.
- **The creator** of a project becomes its first admin.
- **Workspace owners are implicitly admin** on every project in their
  workspace, whether or not they appear in the list. Granting an owner a level
  is refused rather than silently ignored: they always resolve to admin, so a
  project admin can never lock an owner out.
- **A project always keeps at least one admin.** Removing or demoting the last
  explicit admin grant is refused — including an admin trying to remove
  themselves.

Projects created before permissions existed have no recorded creator, so they
start with no explicit admins and are governed by workspace owners until
someone grants one. If your workspace has no owner-level account left, you
cannot administer those projects — promote an owner first.

## Invite-only projects

Set the **default** to `No access` and the project becomes invite-only: only
the people listed as exceptions (and workspace owners) can see it. Everyone
else is treated exactly like a non-member — the project does not appear in
their project list at all.

## Folder rules

Project permissions apply to the whole project. A **folder rule** gives one
folder inside it different access — "everyone on the team can write this
project, except `designs/`, which most people may only read".

Edit them in **Project settings → Folders**. Only project admins (and workspace
owners) can add or change one.

- A rule names a **folder prefix** and the level everyone else gets there, plus
  optional per-person exceptions — the same shape as project permissions, one
  level down.
- **The longest matching rule wins.** A rule on `a/b/` governs `a/b/` even if
  another rule covers `a/`. Rules do not merge: someone granted on `a/` but
  denied on `a/b/` loses `a/b/`.
- **Workspace owners are never affected.** They are admin everywhere, so a
  folder rule can never lock out the people responsible for the workspace.
- A rule may not grant `admin` — that is a project-wide capability (rename,
  delete, edit permissions), so it has no meaning over one folder.

### What a read-only folder does on your machine

It syncs down like any other folder, so it is always current. What changes is
what happens when you edit it:

- Your device never sends the change. Nobody else sees it, and your sync does
  **not** get stuck.
- On the next sync the project's version is put back, and your version is kept
  beside the file as `name (local, not synced).ext`. Nothing you wrote is
  thrown away.
- Deleting a read-only file locally has the same shape: the file comes back on
  the next sync.
- `bdrive sync` names every file it put back, so this never happens silently.

The same applies in the web UI: uploading to, removing from, restoring into, or
creating a share link for a read-only folder is refused.

### What folder rules do not do yet

**A folder rule controls who can change a folder, not who can see it.** Every
member of the project still syncs and reads every file, including in folders
they cannot write. Hiding a folder's contents from people who are not on its
list is being built; until it ships, do not use a folder rule to separate
secrets from teammates — put those in their own project, where `No access` on
the project default already hides them completely.

## What a device does when it is refused

A hub saying *no* is not the same as a hub being unreachable, and BearDrive
keeps them apart. In both cases your local files and your local history are
left alone — losing access never deletes or reverts anything on your disk.

### Read-only: the daemon goes pull-only

Your teammates' changes keep arriving and materializing normally. Your own
edits are still journaled locally; they are simply never pushed. They are not
dropped either — if you are granted `write` again, they go out on the next
cycle with nothing to do by hand.

```
  pending:  3 local change(s) not yet pushed
  access:   read-only (pull only) — 3 local change(s) stay on this device
```

### No access: sync pauses

Nothing is pulled, pushed, or written. The daemon keeps ticking cheaply and
re-checks, so a re-grant resumes on its own.

```
  access:   no access to this project — sync paused
```

Either line shows up in `bdrive status`, in `bdrive sync` output as
`remote: read-only (pull only)` / `remote: no access — sync paused`, and once
— on the transition, not every tick — in the project's `daemon.log`.

If you see one, there is nothing to fix on the device. Ask a project admin or
a workspace owner to change your level.

## Share links are not affected

Public `/s/<token>` links are anonymous by design and keep serving until
revoked. Cutting someone's access to a project does **not** kill share links
they minted — revoke those separately (`bdrive share --list` /
`bdrive share --revoke`, or the workspace's shares view).
