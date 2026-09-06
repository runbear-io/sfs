# Workspace root — independent validation pass

**Verdict: 9 findings (0 blocker, 4 should-fix, 5 nit).**

Scope: `git diff HEAD` on `desktop-app` @ `1672a13` plus the six untracked
files, read against `.claude/workspace-root-goal.md` and `desktop/DESIGN.md`
§"Workspace root and projects". Gate re-run from scratch; every finding below
was reproduced, not inferred.

Two structural claims of the design **hold** and I could not break them:

- **The manifest is never a source of truth.** `config.LoadWorkspace` has
  **zero** non-test production callers (`grep -rn "LoadWorkspace"` →
  `workspace_test.go`, `cmd/bdrive/workspace_test.go`,
  `desktop_onboard_test.go` only). The single production read is
  `IsWorkspaceRoot`, which returns a bool and never yields a path. No volume
  dir, journal path, mount id, remote, or permission is derived from the
  manifest anywhere.
- **The hook walk-up in the shipped layout is correct and within budget.**
  Reproduced by hand with a real `/bin/sh`, a fake `bdrive` on `PATH`, and
  `PATH` restricted to shims so any other external command would fail loudly
  (§4 below).

---

## Findings

### F1 — should-fix — the manifest is never refreshed on resume or daemon start; three places say it is

`cmd/bdrive/sync_run.go:31-39`, `desktop/DESIGN.md:134`, `desktop/DESIGN.md:146-148`,
`cmd/bdrive/desktop_onboard.go:462-466`.

`RefreshWorkspaceOf` is called from exactly one place: `startSync`. `startSync`
has three callers — `cmd/bdrive/init.go:267`, `init.go:424`, and
`desktop_onboard.go:493`. **`bdrive resume` is not one of them**:
`cmd/bdrive/resume.go:77` calls `daemon.Start` directly. Nothing in
`internal/daemon` touches the manifest either (`grep -rn "RefreshWorkspace"`
returns only `sync_run.go:40` and `desktop_onboard.go:438,467`).

So all of these are false:

- `sync_run.go:34` — *"this runs on `bdrive init` and on every resume, which is
  what DESIGN.md means by 'refreshed on daemon start'"*
- `DESIGN.md:134` — *"…and refreshed on daemon start."*
- `DESIGN.md:147` — *"`startSync` re-indexes the root above a project on init
  and on every resume"*
- `desktop_onboard.go:466` — *"startSync below refreshes it on every later
  resume"*

**Failure scenario.** A machine reboots. The login agent runs `bdrive resume`,
which starts every daemon. A project created under the root on another device
and materialized here, or a project folder the user deleted, is never reflected
— the manifest stays whatever `bdrive init` last wrote, indefinitely. The only
refresh gesture in the product is re-running `bdrive init` inside a child, or
re-running the desktop connect flow.

**How verified.** `/tmp/refresh_e2e.sh` against the real binary
(`go build -o /tmp/valbdrive ./cmd/bdrive`), isolated `HOME`/`BDRIVE_HOME`, a
`file://` remote, a hand-built root + one project:

```
--- manifest BEFORE any sync ---   {"kind":"workspace","projects":[]}
--- run: bdrive sync (in the project) ---
synced /tmp/wsE2E/root/team (project "team")   local changes: 1   remote: pushed
--- manifest AFTER bdrive sync ---  {"kind":"workspace","projects":[]}
--- run: bdrive resume ---          started /tmp/wsE2E/root/team (pid 25019)
--- manifest AFTER bdrive resume ---{"kind":"workspace","projects":[]}
```

The project is registered, syncing, and directly under the root, and neither
gesture indexed it.

---

### F2 — should-fix — README and the docs promise the manifest self-heals; a deleted or corrupted one is never repaired

`README.md:309-311`, `web/docs/src/content/docs/reference/project-files.md:107-110`,
`desktop/DESIGN.md:99-101`, gate at `internal/config/workspace.go:135-138`.

Both user-facing docs say the manifest is *"safe to delete or hand-edit …
the next sync rewrites it from what is on disk"*. `RefreshWorkspace` opens with

```go
if !IsWorkspaceRoot(root) {
    return nil
}
```

and `IsWorkspaceRoot` requires the file to **exist, parse, and carry
`kind == "workspace"`**. So the two edits the docs call out as safe are exactly
the two that permanently disable the repair:

- **Delete it** → `IsWorkspaceRoot` false → `RefreshWorkspace` is a permanent
  no-op → the folder silently stops being a root. Nothing short of the desktop
  connect flow (`InitWorkspace`) ever recreates it.
- **Hand-edit it into invalid JSON, or drop `kind`** → same, and the corrupt
  bytes stay on disk forever.

Compounding: while un-rooted, `cmd/bdrive/init.go:199`'s refusal also
disappears, so `bdrive init <root>` would happily mount the whole root —
including the "folders BearDrive never touches" the root exists to hold apart.

DESIGN.md:99 states the same promise more strongly (*"a stale or hand-edited
entry is corrected, never obeyed"*). That is true only for a manifest that is
still well-formed; the round's own test `TestWorkspaceRescanCorrectsStaleEntry`
only exercises the well-formed case (`SaveWorkspace` always writes valid JSON).

**How verified.** Same `/tmp/refresh_e2e.sh` run, continued:

```
--- now DELETE the manifest, as README says is safe ---
NOT REWRITTEN: the root is no longer a workspace root
--- and a CORRUPTED manifest (README: safe to hand-edit) ---
after sync+resume: {"kind":"workspace","projects":[{"path":"ghost","id":"m-99"}
```

(`bdrive sync` and `bdrive resume` both run in between each time.)

---

### F3 — should-fix — the `kind` guards do not cover the agent-hook walk-up, and DESIGN.md implies they do

`internal/agenthooks/agenthooks.go:99-106`, `desktop/DESIGN.md:96-104`,
`internal/config/project.go:226-243`, `.claude/workspace-root-goal.md:34` (trap 3).

DESIGN.md §"The name collision" says the `kind` field and its guards stay *"for
the case the files meet anyway (a hand-edited root, or one written by an older
build)"*, and then names the two guards: `LoadProject` and `IsMount`. The goal
file's trap 3 is stronger — the guard *"must skip a workspace config and keep
climbing in pure shell"*. It does not skip a workspace config; it skips a
differently-named file. When a manifest does land in `.bdrive/config.json`, Go
and the shell now **disagree**: Go says "not a mount", the shell says "mount".

**Failure scenario.** A root whose manifest sits in `config.json` (hand-edited,
or a user following the *original* text of this very DESIGN section) makes every
agent tool call in a non-BearDrive sibling spawn `bdrive`. That is the CLAUDE.md
invariant verbatim: the guard *"must never spawn `bdrive` (or anything else)
outside a BearDrive project"*. The spawned process then errors out — because
`LoadProject` correctly refuses the manifest — so the cost is a wasted `exec`
per tool call, forever, in a folder that never syncs.

**How verified.** Real `/bin/sh`, the guard string taken from a real install
(`bdrive hooks install --agent claude` into an isolated `HOME`, then the
`sh -c` body copied verbatim out of `settings.json`), fake `bdrive` on `PATH`
that logs, `env -i` with `PATH` set to a shim dir only:

```
== /tmp/hk/rootA/not-beardrive/src        (rootA/.bdrive/config.json = {"kind":"workspace",...})
  externals=0 bdrive=1
```

i.e. it did not even reach the `grep` — the walk stopped at the collided root
and spawned immediately. Real-binary consequence:

```
$ cd /tmp/wsX/collided/not-beardrive/src && bdrive sync
Error: … is not a beardrive project (run `bdrive init` there first)
```

Note this spawn is **not new** — at HEAD, any `config.json` above you stopped
the walk and `IsMount` agreed. What is new is that the round declares that file
"not a mount" everywhere except here, and documents the collided case as
covered. Either the guard should cover it (see F5 — it can, for zero
processes), or DESIGN.md should say plainly that the collided layout is
unguarded in the hook path.

---

### F4 — should-fix — designating a workspace root is irreversible, and a failed desktop connect leaves one behind

`cmd/bdrive/desktop_onboard.go:422-441` (`undo`), `:438` (the refresh),
`:467` (`InitWorkspace`),
`cmd/bdrive/init.go:199-204`, `desktop/DESIGN.md:158-160`.

`runDesktopInit`'s doc comment promises *"On failure it removes what it made
(and only what it made) so a retry starts clean."* `undo` removes the registry
row, `<target>/.bdrive`, and the target folder when it created it — and then
calls `RefreshWorkspace(filepath.Dir(target))`, which **rewrites** the manifest
to `projects: []` rather than removing it. There is no deletion of
`<root>/.bdrive/workspace.json` or of the `<root>/.bdrive` directory anywhere in
the file. Any failure after line 467 (the `.bdriveignore` write, the template
seed, `EnrollMount`/`store.Open`/`openSession`/`Cycle` inside `startSync`) leaves
the user's chosen folder permanently converted into a workspace root by a
connect that reported failure.

The user cannot undo it: there is no `bdrive workspace` command, and DESIGN.md's
"Not done" list mentions only that no command *designates* a root — not that
none *removes* one.

**Failure scenario.** User points the Mac app at `~/MyProjects`; the connect
fails partway. `~/MyProjects/.bdrive/workspace.json` is left behind with an
empty project list. `bdrive init ~/MyProjects` — a folder they could have
initialized directly before — now refuses forever, and the only fix is to know
to `rm` a file the error message never mentions.

**How verified.** Code inspection for the "undo leaves it" half (`undo` has no
manifest removal; and `RefreshWorkspace` can only have run at all if the root
still exists). The user-visible half reproduced with the real binary on exactly
the state `undo` leaves (`/tmp/exp2.sh`):

```
$ cd /tmp/wsX/MyProjects && bdrive init --yes
Error: /tmp/wsX/MyProjects is a BearDrive workspace root, not a project
the root indexes the projects inside it and never syncs itself
run init in a folder under it instead, e.g. bdrive init /tmp/wsX/MyProjects/team
$ bdrive --help | grep -i workspace     # (no output — no command manages roots)
```

---

### F5 — nit — the deviation's stated justification is false; the deviation itself is sound

`desktop/DESIGN.md:112-116`, `internal/config/workspace.go:13-19`,
`internal/agenthooks/agenthooks.go:100-106`, `.claude/workspace-root-goal.md:58-64`.

All four say the same thing: telling a root from a project in the walk-up would
mean *"a `grep` per level"* / *"a process per ancestor"*. That is not true.
`read` is a POSIX **shell builtin**; a `kind`-discriminating walk-up costs zero
external processes.

**How verified.** `/tmp/exp5.sh` — the original DESIGN layout (manifest in
`config.json`), a walk that reads each candidate with the `read` builtin, run
under `env -i PATH=/nonexistent`:

```
/tmp/e5/root/plain/src       d=[]                            <- climbs past the root
/tmp/e5/root/team            d=[/private/tmp/e5/root/team]   <- stops at the project
/tmp/e5/root                 d=[]
```

Zero externals available and it still answers all three correctly.

This does **not** make the deviation wrong — a separate file name is still the
better choice, and the design gives the argument that actually carries the
weight one paragraph earlier ("every existing reader is correct without knowing
that workspaces exist"), plus it avoids an `open`+`read` per ancestor on every
tool call. The finding is that the *reason recorded in three files and the goal
file* is factually wrong, and it is the reason a future reader would rely on
when deciding whether F3 is fixable. It is.

---

### F6 — nit — DESIGN.md states a rule the codebase contradicts

`desktop/DESIGN.md:125`, `internal/config/workspace.go:107`.

*"Roots do not nest; a project folder does not contain another project."* The
first clause is enforced (`InitWorkspace`). The second is false in this product:
`internal/syncer/walk.go:17,56-60` has a `vNested` verdict and
`Filter.addNestedMount`, `internal/syncer/ignore.go:77-97` has
`underMountOnDisk` described as *"the authoritative form of the question"*, and
`cmd/bdrive/init.go`'s own refusal message says *"the syncer's nested-mount
handling exists because it wasn't [refused]"*. Nested mounts exist; `init`
merely declines to create new ones.

The consequence for this round is benign — `ScanWorkspace` deliberately does not
descend, so a nested project is simply absent from the index — but the sentence
as written would mislead someone deciding whether `ScanWorkspace` needs to
recurse. `workspace.go:107` repeats it as a justification comment.

---

### F7 — nit — row 10's test asserts almost nothing the feature is responsible for

`internal/syncer/workspace_test.go:50-111` (`TestWorkspaceRootNeverScanned`).

Strip the workspace feature entirely and every assertion in this test except one
still holds, because the syncer only ever walks the mount and has no idea a
parent directory exists:

- peer receives exactly `doc.md, sub/nested.txt, workspace.json` — true with or
  without the feature;
- `!config.IsMount(root)` — true; nothing writes a `config.json` there;
- `root/non-beardrive-folder-1/secret.txt` still present — true;
- `!ReservedDir("workspace") && !ReservedName(...) && !ReservedPath(...)` — true
  by inspection of `ReservedDirs`, untouched by this round;
- `ReservedPath(".bdrive/workspace.json")` — true solely because `.bdrive` is in
  `ReservedDirs`, which predates the round.

The one feature-dependent assertion (`LoadWorkspace(root)` lists `team`) tests
`InitWorkspace`+`LoadWorkspace`, already covered twice in `internal/config`.

I therefore cannot corroborate the goal file's claim that this row's test
"fail[ed] on the current tree" before the fix in any sense other than a compile
error on `config.InitWorkspace`. The valuable half — a user file literally named
`workspace.json` inside a project must keep syncing — is a genuine
anti-regression and worth keeping; the rest is scaffolding.

---

### F8 — nit — `ScanWorkspace` silently skips symlinked children

`internal/config/workspace.go:112-129` (the `if !e.IsDir()` at :118).

`for _, e := range ents { if !e.IsDir() … continue }` — `os.ReadDir` reports a
symlink's own type, so a symlink pointing at a project directory has
`IsDir() == false` and is dropped. A root laid out with symlinks to project
folders elsewhere on disk indexes as empty.

Impact today is nil (nothing consumes the manifest — see the preamble), so this
is a nit, but it is the shape of thing that turns into "the Mac app doesn't see
my project" the moment the index is wired to UI.

**How verified.** `/tmp/e6/main.go`, reproducing the exact entry test:

```
linked   IsDir=false  -> ScanWorkspace SKIPS it
real     IsDir=true   -> ScanWorkspace considers it
```

---

### F9 — nit — every other command at a root tells the user to run a command that is now guaranteed to fail there

`cmd/bdrive/init.go:199-204` vs the generic not-a-project errors
(`cmd/bdrive/helpers.go` → `mustProject`, `share.go:245`).

The workspace-root refusal is known only to `bdrive init`. Every other command
run at a root still emits the generic message, which recommends exactly the
command that will now refuse:

```
$ cd <root> && bdrive log
Error: <root> is not a beardrive project (run `bdrive init` there first)
$ cd <root> && bdrive init
Error: <root> is a BearDrive workspace root, not a project
```

Before this round the advice was correct — `bdrive init` at that folder worked.
Now it is a two-step dead end. Reproduced for `bdrive log`, `bdrive scope`
(*"…is not a beardrive project (run `bdrive init` there first)"*) and
`bdrive share --list` (*"not inside a bdrive project (run `bdrive init`
first)"*). `bdrive status` at a root is fine — it lists the child project from
the registry.

**How verified.** `/tmp/cli_at_root.sh` against the real binary, isolated
`HOME`/`BDRIVE_HOME`, a hand-built root with one registered child project.

---

## What I checked and found clean

**Design conformance (the rest of §Workspace root and projects).**
Manifest shape, relative paths, root-survives-rename, `LoadProject` refusal,
`IsMount` kind read, "nothing at the root syncs", "not in `ReservedDirs`",
"`bdrive init` at a root refuses" — all match the code. `SaveWorkspace` goes
through `config.writeJSON` (temp + rename, `internal/config/config.go:219-231`),
so the atomic-state-file invariant holds. `.bdrive` is a `ReservedDir`, so a
manifest can never reach a teammate, and `InitWorkspace` refuses a root inside a
project — the two ways a manifest could have become synced state.

**`bdrive init`'s refusal really is pre-network.** With no `settings.json` at
all, the refusal returns in 1s, writes nothing at the root, and creates no
session (`/tmp/refuse_order.sh`). Row 6 is genuinely closed.

**Every `.bdrive/config.json` toucher, handed a root.** `grep -rn
"projectConfigPath|ProjectDir|IsMount\(|LoadProject\(" --include='*.go'` → ~30
production call sites. Against a real root (no `config.json` at all) every
one behaves as before, because there is nothing there to read. Against a
collided root (`config.json` holding the manifest):

- `syncer/walk.go:56` — a nested directory carrying a workspace manifest is now
  `vDescend` instead of `vNested`. Only reachable by hand-planting one inside a
  mount; arguably the correct answer anyway.
- `syncer/ignore.go:87` (`underMountOnDisk`) — same direction, same
  reachability.
- `cmd/bdrive/helpers.go:42,85` (`findMountRoot`/`syncTargets`) — correctly
  climbs past.
- `cmd/bdrive/share.go:238` (`findProject`) — now returns
  `"…: workspace root, not a project"` where it previously returned an **id-less
  project** (`ok==true`, `ID==""`), which downstream would have built a volume
  path from `""`. Strict improvement.
- `cmd/bdrive/cmds.go:113,217`, `resume.go:51`, `daemon.go:412`,
  `desktop.go:356,409` — all treat the error as "not a project" and skip.

**`IsMount` performance in the per-directory scan loop.** The `os.Stat` →
`os.ReadFile` change is not a regression on the miss path (both are one
syscall). Measured over 2000 directories, 30 iterations, Apple M1:
`BenchmarkStatMiss 81.0 ms/op` vs `BenchmarkReadFileMiss 34.3 ms/op` — the read
is if anything faster. No finding.

**The hook guard, by hand, in the shipped layout.** Real `/bin/sh`, real guard
string lifted from a real `~/.claude/settings.json`, fake `bdrive` on `PATH`:

| session dir | result |
|---|---|
| `root/non-beardrive-folder-1` | no spawn ✅ |
| `root/non-beardrive-folder-2/src/deep` | no spawn ✅ |
| `root/team` (a real project) | fires ✅ |
| `root/team/docs/deep` (subfolder) | fires ✅ |
| `root` itself | fires via the registry ✅ (correct — a mount lives below) |

Break attempts that all behaved: a project with no `id` (fires — pre-existing,
its sibling does not); a symlink to a project (fires) and a symlink to a plain
folder (no spawn); a folder name containing a newline (no spawn — the explicit
`case "$PWD" in *"\n"*` guard) and a newline-named *project* (fires); a folder
name containing `"` (no spawn / fires respectively — `grep -F` is literal so no
pattern injection). A root at `/` cannot make the walk fire: the loop tests
`/.bdrive/config.json` and then always exits with `d=""`, falling through to the
registry — demonstrated with the loop's own arithmetic. Only the collided
`config.json` layout broke it (F3).

**Process budget.** With `PATH` set to a shim directory only, the guard runs
**at most one external command** — `grep -qF "\"$PWD/" mounts.json` — and zero
when the walk finds a mount. Nothing else is spawned before `command -v bdrive`.
Note `TestHookGuardStaysPureShell` is largely redundant with the pre-existing
`TestSec_Hooks_EveryHookCommandIsGuarded`
(`internal/agenthooks/sec_hooks_test.go:246-274`), which already pins ≤1 grep
and gate-before-spawn across *all four* platforms rather than Claude's three
commands.

**Repo invariants.** No change touches journal writing, blob/journal ordering,
scan-before-pull, materialize's dirty check, or daemon liveness. The one state
file added is written atomically. The guard stays pure shell.

**The modified existing test — `TestDesktopInitFounder`
(`cmd/bdrive/desktop_onboard_test.go:221-250`) — was strengthened, not
weakened.** It replaced `os.Stat(root/.bdrive)` `IsNotExist` with four
assertions: `!IsMount(root)`, no `config.json` at the root, *every* entry in
`root/.bdrive` must be exactly `WorkspaceFile`, and the manifest lists the one
project just connected. The original property ("the PROJECT ROOT must never
become a mount", "nothing but the manifest outside `<root>/<name>`") is
preserved and now asserted directly rather than via a proxy; the `.bdriveignore`
assertion is untouched. The `os.ReadDir` on `root/.bdrive` `t.Fatalf`s if the
directory vanishes, so a regression that stopped writing the manifest still
fails the test.

**Tests passing for the wrong reason.** Only F7. The other five new tests are
load-bearing: `TestHookGuardSkipsWorkspaceRoot` fails if the manifest moves into
`config.json` (verified independently by hand — case F3 shows the sibling would
then spawn); `TestInitRefusesWorkspaceRoot` would hang out its 20s bound if the
guard were removed — verified directly, not assumed: with no session,
`bdrive init --yes` in a plain folder was still running after 15s, having
printed *"no interactive terminal detected — using the device-code sign-in
flow"* and a live `https://beardrive.ai/auth/device/<code>` URL
(`/tmp/nologin.sh`). Note that failure mode makes a real outbound request to
beardrive.ai; a passing run does not. `TestLoadProjectRefusesWorkspace`,
`TestIsMountFalseAtWorkspaceRoot` and `TestWorkspaceManifestShape` all assert
things that are false without the change.

---

## Regression gate — run by me, not pasted

From `/Users/snow/workspace/runbear/wt-desktop-app`:

```
go build ./...   → OK
go vet ./...     → OK
go test ./... -timeout 1800s → ok, all 12 packages, 4m55s wall
                               *but* internal/webapp came back (cached);
                               see the timeout section below
```

Per-package: `cmd/bdrive 225.6s`, `internal/agenthooks 36.0s`,
`internal/autostart 9.3s`, `internal/config 3.6s`, `internal/daemon 43.5s`,
`internal/journal 44.3s`, `internal/remote 14.9s`, `internal/secrets 3.0s`,
`internal/store 4.3s`, `internal/syncer 109.5s`, `internal/templates 0.7s`,
`internal/webapp (cached)`.

`gofmt -l` over the eleven files this round touches (six modified Go files, five
new): **clean, no output**. (Consistent with the repo's known pre-existing
gofmt-dirty files being untouched here.)

### The `internal/webapp` timeout claim — verified as *predating the round*, but the prescription is wrong

**The claim that the timeout predates this round is TRUE.** The prescription
that `-timeout 1800s` is sufficient is **FALSE on this machine**, and the
"`go test ./...` green" in the goal file's Done #2 holds only with a warm cache.

I did not use `git stash`. I extracted a clean HEAD tree with
`git archive HEAD | tar -x -C /tmp/headcopy` (touches neither the worktree list
nor the stash stack) and ran the package there.

| tree | command | result |
|---|---|---|
| working tree (`1672a13` + this round) | `go test ./internal/webapp/ -count=1 -timeout 1800s` | **FAIL 1801.504s** — `panic: test timed out after 30m0s`, 0 test failures |
| clean HEAD `1672a13` (`/tmp/headcopy`) | same | **FAIL 1801.662s** — `panic: test timed out after 30m0s` |

Neither run completed. Both died on the timeout alarm rather than on any
assertion: on the working tree the killed goroutine was
`TestSec_DeviceFlow_OneApprovalMintsOneToken (2m56s)` and **not one `--- FAIL`
line** appeared. The two wall times differ by 0.16s — i.e. by nothing, because
both are the ceiling.

Two honesty notes on my own measurement:

- The clean-HEAD run reported four `--- FAIL`s
  (`TestCompressionE2E_OldBinaryPullsCompressedAndPushesRaw`,
  `TestDeltaE2E_{OldBinaryReadsChunkedStorage,MixedFleetConflictAndDelete,OldBinaryWritesNewReads}`),
  all with `git rev-parse: exit status 128`. Those are **artifacts of my
  harness**, not of HEAD: `git archive` produces a tree that is not a git
  repository, and those tests shell out to `git rev-parse` to build an
  "old binary". They are not evidence of anything about either tree.
- The two runs overlapped for ~19 minutes. Both were I/O-bound rather than
  CPU-bound (`21% cpu` on the working-tree run), and both hit the same 30m
  ceiling regardless, so contention does not explain the result — but the
  numbers are not clean single-tenant measurements.

This is **not attributable to this round**, so it is not one of the nine
findings: `internal/webapp` is untouched by the diff, and it reaches the changed
code exactly once — `cli_postsync_e2e_test.go:57` calls `config.LoadProject`
(`go list -deps` confirms `internal/config` is the only changed package in its
graph; `grep` finds no `config.IsMount` call in the package at all). It does
mean the round's Done #2 should read "green, with `internal/webapp` served from
the build cache", and that whoever inherits this needs a timeout above 1800s to
get a real answer.

---

## Suspicions (unproven — NOT findings)

These are behaviour changes or hazards I could describe precisely but could not
turn into a demonstrated product failure. Listed so the next reader does not
have to re-derive them.

1. **`IsMount` now blocks forever on a FIFO.** `os.Stat` returned instantly on a
   named pipe; `os.ReadFile` blocks in `open()` until a writer appears. Verified
   the primitive (`/tmp/fifo.sh`: `stat` → "Fifo File" instantly, `cat` → killed
   after 5s). `walk.go:56` calls `IsMount` on every directory under a mount, so
   a `<dir>/.bdrive/config.json` FIFO would wedge a scan. Not filed as a finding
   because `ReservedPath` rejects `.bdrive` at any depth
   (`internal/config/project.go:169-176`), so no peer can materialize one — it
   requires a local `mkfifo` at exactly that path.
2. **`IsMount` flipped `false` → `true` for an unreadable `.bdrive`.** Mode-000
   directory: old `os.Stat` → `EACCES` → false; new `os.ReadFile` → `EACCES` →
   `true`. In the stated safe direction, and the new doc comment claims it, but
   it is a semantic change the round describes as if it were pre-existing.
3. **`IsMount` reads the whole file.** Unbounded; a large hand-written
   `config.json` is now read in full on every per-directory scan hit. Not
   reachable via sync for the same `ReservedPath` reason.
4. **`InitWorkspace` with a relative path performs no ancestor walk.**
   `filepath.Dir(".") == "."`, so the loop breaks on its first iteration and the
   nesting/inside-a-project refusals never run (verified the arithmetic in
   `/tmp/e6`). Unreachable today — the only caller passes a path that
   `validateShared` has already required to be absolute — but DESIGN.md:160
   advertises `InitWorkspace` as "the seam when one is wanted" without saying it
   requires an absolute path.
5. **The desktop connect flow permits `root == $HOME`.** `validateShared`
   (`desktop_onboard.go:91-127`) requires only absolute + exists + is-a-dir +
   not-containing-`$BDRIVE_HOME`. `<root>/<name>` with `root == $HOME` passes,
   after which `InitWorkspace($HOME)` writes `$HOME/.bdrive/workspace.json` —
   i.e. *inside* the default `$BDRIVE_HOME`, beside `mounts.json`,
   `device.json`, `settings.json`. I found nothing that enumerates that
   directory (`grep -rn "ReadDir" internal/config cmd/bdrive` → four sites, none
   over `$BDRIVE_HOME`), so I could not show breakage, only oddity.
6. **A hand-planted `workspace.json` in a live project folder breaks
   `bdrive init` there.** The `IsWorkspaceRoot` refusal (`init.go:199`) sits
   *before* the "already initialized → resume" branch (`init.go:220`), so a
   folder that is both a mount and carries a manifest can no longer be resumed
   with the documented gesture. Not filed because that state is unreachable
   without hand-editing (`InitWorkspace` refuses a root at a mount, and
   `.bdrive` never syncs).
7. **`LoadWorkspace` validates nothing it returns.** It unmarshals and checks
   only `kind`. `WorkspaceProject.Path` is never checked for `..`/absoluteness
   and `WorkspaceProject.ID` never goes through `ValidMountID` — the guard
   `LoadProject` applies at `internal/config/project.go:268` precisely because
   the id is "read verbatim from a file that arrives with the folder". Inert
   today (nothing reads the manifest), and F2 shows a corrupt manifest is never
   rewritten, so a hand-edited `{"path":"../../..","id":"../../evil"}` survives
   indefinitely waiting for its first consumer. The design's answer is "never
   resolve from it", which currently holds — but the first reader that does
   `filepath.Join(root, entry.Path)` inherits a traversal sink with no guard in
   the loader.
8. **Concurrent `RefreshWorkspace` under one root.** Two `startSync` calls for
   two projects under the same root race; each writes a full scan atomically, so
   the loser is a complete-but-slightly-stale index rather than a torn file. No
   lock, but also no corruption.

---

## What I could not check

- **The Mac app / Tauri shell.** Nothing in `desktop/` beyond `DESIGN.md` was
  read or run; the design itself says no UI names the root yet, so there is no
  UI behaviour to validate.
- **The desktop connect flow end to end.** `runDesktopInit` needs a live hub. I
  verified the workspace-relevant lines by inspection plus the round's own
  `TestDesktopInitFounder` (which passes and is stronger than before), and
  reproduced the *resulting states* with the real binary — but I never drove a
  real connect, and specifically never drove a connect that **fails after line
  467**, which is the reachability half of F4.
- **Windows and Linux.** All shell reproduction was macOS `/bin/sh` (bash 3.2 in
  POSIX mode). The guard's `${d%/*}` arithmetic and the `case` fall-through
  should behave identically under `dash`, but I did not run it there. Note also
  that `IsMount`'s `os.ReadFile` change interacts with Windows file-sharing
  semantics differently from `os.Stat`, and `GOOS=windows go build ./...` does
  not pass in this repo anyway (per CLAUDE.md).
- **`bdrive export`/`import` round-tripping a project under a root.** By
  inspection they operate on the remote store layout and never see a manifest,
  but I did not run one.
- **Whether row 10's test ever failed before its fix** (see F7) — the goal
  file's protocol claims every row did; for that row I could construct no
  version of the tree where it fails other than a compile error.
- **Whether `internal/webapp` passes uncached at all.** Neither tree produced a
  completed run at `-timeout 1800s`; I did not retry at a higher timeout, so the
  package's true runtime and pass/fail state are unknown to me on both trees. I
  established only that the two trees are indistinguishable, which is the
  question that mattered here.
- **The frontend.** No `internal/webapp/frontend` files are touched by this
  round and I ran no e2e suite.
- **Nothing was left behind.** `git status --short` at the end matches the start
  (9 modified, 6 untracked + this report); no test file, benchmark, or scratch
  artifact was written into the repo — all harnesses live in `/tmp`
  (`/tmp/hk`, `/tmp/e4`, `/tmp/e5`, `/tmp/e6`, `/tmp/ws*`, `/tmp/headcopy`).
