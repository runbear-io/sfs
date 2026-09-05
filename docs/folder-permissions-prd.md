# PRD: Folder permissions — restrict a subtree, not just a project

Today permission granularity stops at the project. `internal/webapp/perms.go`
resolves one level per (account, project) and every per-project route declares
what it needs at registration. A team that wants `a/` visible to two people and
everything else open to everyone has exactly one move: split `a/` into a second
project — a second folder on every laptop, a second `bdrive init`, a second
sync daemon, and every cross-reference between the two broken.

This spec adds **folder rules**: a per-prefix override of the same four levels,
resolved by the same resolver, enforced at the same choke point.

Implement the phases in order; a phase is done only when every acceptance box
in its checklist is checked. Record progress and blockers in §Status.

## Decisions (settled, do not relitigate)

| Question | Answer |
|---|---|
| What a non-permitted member experiences | **Invisible and never synced.** Not in the viewer, not on disk, 403 on the wire. |
| Grant model | **Exception list on a default-open project.** No ACL tree, no inheritance engine. |
| Levels | **Viewer vs Editor** — reuse `PermRead` / `PermWrite` / `PermNone` verbatim. |
| Who administers | **Project admins and org owners.** An org owner always resolves to `PermAdmin`, so break-glass is inherent and an offboarding never orphans a subtree. |

## The design in one function

`projectPermOf(r, p)` answers "what may this account do with this project".
The whole feature is one more argument:

```go
// pathPermOf resolves the caller's level for one path inside a project.
// Longest matching prefix wins outright — rules do not union, and there is
// no recursion to reason about.
func (s *Server) pathPermOf(r *http.Request, p Project, path string) string {
	base := s.projectPermOf(r, p)
	if base == PermAdmin {
		return PermAdmin // org owner / project admin: never lockable-out
	}
	rule, ok := p.ruleFor(path)
	if !ok {
		return base
	}
	if l, ok := rule.Perms[normEmail(s.requestUser(r).Email)]; ok {
		return l
	}
	if rule.Default == "" {
		return base // rule exists but says nothing for this account
	}
	return rule.Default
}
```

Data model mirrors `Project.Default` + `Project.Perms` exactly, one level down:

```go
type FolderRule struct {
	Prefix  string            `json:"prefix"`            // "a/", always slash-terminated
	Default string            `json:"default,omitempty"` // "" = inherit the project level
	Perms   map[string]string `json:"perms,omitempty"`   // lowercase email → level
}

type Project struct {
	// ...existing fields...
	Folders []FolderRule `json:"folders,omitempty"`
}
```

**There is no stored permission epoch.** The first draft of this PRD gave
`Project` a `PermEpoch` counter bumped on every rule change. Phase 0 replaced
it with `scopeTag(project, email, base)` — a hash of *one account's* effective
visibility, derived from the rules themselves. It needs no column, no write
path and no monotonicity argument; it cannot drift between two hub processes
or survive a rollback holding a stale value; and being per-reader, changing
Alice's grant no longer forces Liam's device to re-sync a project whose
visible contents did not move. Phase 3 uses it as both the client's re-sync
trigger and the filtered-journal cache key — one concept where the draft had
two.

Snow's example is one rule:

```json
{"prefix": "a/", "default": "none",
 "perms": {"liam@runbear.io": "write", "snow@runbear.io": "write"}}
```

`PermNone` at project level already means "hidden: absent from the list, 403
everywhere". The whole feature is making that sentence true of a prefix.

**Why this is cheap and an ACL tree is not.** `Perms` is already
email→level; `level()` is already a default with an empty-string sentinel;
`grantable()` already refuses non-members and org owners; `atLeast()` already
orders the levels; `permDenied()` already renders the 403 the frontend maps.
Nothing in this list gets a second implementation.

## Goals

- A folder can be restricted to named accounts without splitting the project.
- A non-permitted member cannot read its contents through *any* surface:
  viewer, history, blob-by-sha, heat, share links, or the sync wire.
- Its bytes never reach a non-permitted device's disk.
- A read-only folder round-trips: a member edits it locally, the hub refuses,
  and the local edit is reverted rather than wedging that member's sync.
- Zero journal-format change. `journal.Less` and `Replay` are untouched, so
  every device still converges — each to its own permitted view.
- A hub with no folder rules behaves byte-identically to today.

## Non-goals (do NOT do these)

- **No new permission vocabulary.** Four levels, same names, same order. No
  "commenter" (there are no comments), no per-file grants (prefixes only).
- **No inheritance engine.** Longest prefix wins; rules never union or merge.
  A member granted on `a/` and denied on `a/b/` loses `a/b/`. That is the
  feature, not a bug.
- **No per-folder sharing-onward.** Only project admins and org owners edit
  rules — no "anyone with access can add others".
- No change to the local volume store layout, the blob store, or
  `store.CachedFile`.
- No encryption. A hub admin and the object store still see everything; this
  is an authorization boundary, not a confidentiality-from-the-operator one.
  Say so in the docs — see §Threat model.
- No blob GC when a folder is restricted. Blobs are retained forever today and
  keep being.
- No re-keying of existing projects. `Folders` absent = today's behavior.

## The hard part: hiding a subtree on the sync wire

The browser surfaces are easy — they all read one fold. The sync wire is where
this gets interesting, and where a naive filter silently corrupts peers.

### What already helps

- **`RemoteSource` caches parsed ops per journal** (`jcache`, validated on
  size+mtime). A per-reader filter is a filter over already-parsed
  `[]sourcedOp`, not a re-parse per request.
- **Push already parses ops** — `handleStorePut` calls `journalOps` and runs
  `opsNameTheirAuthor` and `journalKeepsItsOps` over the result. Per-op path
  authorization is one more loop in a function that already has the ops in hand.
- **Reads are always hub-mediated.** `PutSigner` presigns uploads only; there
  is no read presign, so `handleStoreGet` / `OpenBlob` is the single door
  every byte leaves through.
- **"Each device writes only its own journal"** is what makes per-reader
  filtering safe at all. A client only ever *reads* peer journals; it never
  pushes one back. So a filtered copy on Alice's disk can never propagate as
  the truth, and `journalKeepsItsOps` never sees a filtered body.

### What breaks if you just drop lines

`syncer.pull` does two things that make journal filtering sharp:

```go
if o.Size <= localSize && localSize > 0 { continue }   // skip if not grown
// ...and then resumes parsing from a BYTE offset, never an op count
```

1. **`List` must report the filtered size.** `handleStoreList` returns the
   backend's real object size. Serve a shorter filtered body against a longer
   advertised size and the client re-pulls the whole journal every cycle,
   forever.
2. **Byte-resume demands a stable prefix.** The client resumes at
   `len(local)`. Dropping lines is prefix-stable *only while the filter holds
   constant* — appending ops appends lines. Any rule change relocates ops
   inside the stream, in **both** directions: a revocation shortens it (and
   `o.Size <= localSize` then makes the client skip that peer's journal
   *forever*), and a grant interleaves previously-hidden ops into the middle
   of a prefix the client already trusts.

### The shape that works

- **Filter by dropping whole lines, verbatim.** The journal is JSONL. Read the
  stored bytes, drop lines whose `Op.Path` is not visible to this reader,
  concatenate the rest unchanged. Retained lines stay byte-identical, so
  nothing downstream can tell a re-serializer changed field order.
- **`handleStoreList` reports `len(filtered)`** for every `journal/` key,
  computed from the same cache that renders it. Cache key:
  `(device, stored size+mtime, scope hash, perm epoch)`.
- **The scope tag is the resync trigger.** `/api/p/<id>/scope` returns
  `scopeTag(project, caller, base)`; the client fetches it before every scan,
  stores it in sync state, and on a mismatch discards its peer journals and
  re-pulls that project from zero. Rule changes are rare; a full re-pull is the
  right price for never reasoning about a moved byte offset.

  It was first put on the store LISTING, on the theory that a device should
  notice its view moved from a call it already makes. Nothing ever consumed it
  — the client learns its scope from `/scope` regardless — so it is gone. A
  field nothing reads, carrying a comment calling it load-bearing, is worse
  than no field.
- **Old clients are refused, not degraded.** A client that does not advertise
  `X-Bdrive-Perms: 1` gets 403 on any project that has folder rules, with an
  operator-voice "this project uses folder permissions; upgrade bdrive". The
  capability-negotiation precedent is `handleStoreSign`'s `accept_encoding`.
  Serving such a client an unfiltered journal is the one outcome this feature
  must never have.

### Blobs, chunks, manifests

Content is addressed by sha, and a sha does not know what path references it.

- `blobs/<sha>` is served iff **some op visible to this reader references that
  sha**. The visible-sha set falls out of the filtered fold for free.
- The rule is a **union over visible paths**: content that also exists at an
  unrestricted path stays readable. This is correct — restricting one copy of
  bytes that are already public hides nothing — and the UI must say so when an
  admin restricts a folder whose content is duplicated outside it.
- `chunks/<sha>` and `manifests/<sha>` (delta sync) follow their manifest's
  file sha, same rule.
- `store/exists` and `store/list?prefix=blobs/` are gated identically. A
  filtered journal already means an honest client never asks; this is
  defense-in-depth against one that does, and closes the count/sha existence
  leak in the listing.

### The client side reuses the scope machinery

`GET /api/p/<id>/scope` returns `{epoch, deny: ["a/"], readonly: ["b/"]}` — the
caller's own effective scope, no other account's grants.

- `deny` folds into `internal/syncer/ignore.go`, which is already applied
  symmetrically in scan and materialize, and whose existing rule — *a newly
  filtered path is dropped from the cache without a delete op* — is exactly
  the revocation semantics we want: opting out locally never deletes remotely.
- `readonly` is new: materialize normally, but never commit a local op for
  those paths. A local edit under `readonly` is reverted from the hub's
  version on the next cycle and reported by `bdrive status`, the same way
  Drive reverts an edit to a view-only file.

Without the client half, a member who edits a read-only folder has the hub
reject their whole journal PUT and their sync wedges permanently. The
server-side push check stays anyway as defense-in-depth against a stale or
hostile client.

## Surfaces (every one of these needs the path check)

| Route | Today | Becomes |
|---|---|---|
| `GET .../tree`, `resolve`, `file`, `download`, `render` | `PermRead` on project | + path filter on the fold |
| `POST .../upload/{init,content,commit}` | `PermWrite` on project | + `PermWrite` on the target path |
| `GET /api/p/{id}/history` | `PermRead` | rows filtered to visible paths |
| `GET /api/p/{id}/blob?sha=` | `PermRead` | visible-sha gate |
| `POST /api/p/{id}/{restore,remove,undo-run}` | `PermWrite` | `PermWrite` on each affected path |
| `GET /api/p/{id}/heat` | `PermRead` | buckets filtered; counts must not leak hidden paths |
| `POST /api/p/{id}/reads` | `PermRead` | reports for hidden paths dropped silently |
| `POST /api/p/{id}/shares` | `PermWrite` | `PermWrite` on the shared path |
| `GET /api/p/{id}/shares` | `PermRead` | list filtered to visible paths |
| `GET /s/{token}` | public | the ordinary 404 if its creator can no longer read the path |
| `GET .../store/list` | `PermRead` | filtered sizes + visible shas + epoch |
| `GET .../store/object` | `PermRead` | filtered journal body; visible-sha gate on blobs |
| `GET .../store/exists` | `PermRead` | visible-sha gate |
| `PUT .../store/object` | `PermWrite` | every op's path at `PermWrite` |
| `POST .../store/sign` | `PermWrite` | **unchanged** — see below |

**`store/sign` cannot be path-gated, and does not need to be.** The draft
listed it; the code says otherwise. A sign request names a store KEY —
`blobs/<sha>` or `chunks/<sha>` — and never a path, because a blob is
content-addressed. It is also inert: until an op names it, a blob is
unreferenced bytes that reveal and change nothing, and the op is exactly what
`handleStorePut` gates. Journals and manifests are never presigned at all. So
the enforceable write on the sync wire is the journal, and that is where the
check lives.

Share links are the one deliberate hole in the org wall today ("public is their
point"). A share **minted before** a folder was restricted must stop serving —
checked at serve time rather than migrated at rule-change time, so a storage
failure during an admin action cannot leave a link live.

## Threat model — what this does and does not stop

Stops: a project member reading, syncing, or writing a restricted subtree
through any hub API, including by guessing a sha or naming a journal key
directly.

Does not stop: the hub operator, anyone with object-store credentials, or
anyone who already had the bytes on disk before the restriction landed
(§Known gaps). Folder permissions are an authorization boundary. Do not let
the docs or the UI imply encryption.

## Known gaps (state these in the docs, do not paper over them)

1. **Revocation purges an honest device, and cannot reach a hostile one.**
   Better than this gap first claimed. When access is lost the hub stops
   serving those ops, the device drops the peer journals filtered under the old
   scope, the replayed target no longer holds the paths, and materialize's
   existing cache-vs-target pass removes them — no delete op, nobody else
   affected (`TestLosingAccessRemovesTheFolderFromTheDevice`). It falls out of
   machinery that already existed, which is also why it is pinned: nothing in
   that pass is aimed at revocation, so a change there would quietly turn this
   back into "the files stay forever". A modified client can of course keep
   what it already has; the hub's guarantee is that no new bytes arrive and
   nothing can be fetched again.
2. **Duplicate content stays readable** via any unrestricted path referencing
   the same sha (§Blobs). Surface this in the restrict dialog.
3. **Move across a boundary is asymmetric and correct.** `a/x` → `public/x`
   makes the content visible to everyone (the new op is visible, and its sha is
   now referenced by a visible path). `public/x` → `a/x` deletes it from
   non-permitted disks (they see the delete, not the put). Both fall out of
   last-writer-wins per path rather than out of any move handling, which is why
   the asymmetry looks like a bug until you follow the ops — and why it is
   pinned by `TestMoveAcrossAHiddenBoundary`.
4. **The `?sha=` doors cost one pass over the project's ops.** `visibleSHA`
   answers "may you read these bytes?" by asking whether any op you can read
   names them, over the already-parsed journal cache — and only on a project
   that has rules. `ponytail: a linear scan per version request; a sha→paths
   index if a real hub's history view gets slow.` The browser surfaces need no
   per-scope fold at all (see Phase 2), so the memory gap the draft recorded
   here applies only to Phase 3's filtered journal.
5. **A restricted folder's NAME is visible to project members; its contents
   are not.** DECIDED in Phase 3: `/scope` publishes denied prefixes. A device
   syncs a real local filesystem and has to know which paths it must never
   journal, or a member who happens to create `a/notes.md` locally has their
   whole journal PUT refused and their sync wedges permanently. What stays
   hidden is everything else — the file names inside, the contents, the
   history, the heat, the bytes. An admin who needs the name secret too needs a
   separate project, and the README, the docs page and the UI all say so.
6. **Lamport clocks diverge harmlessly.** A non-permitted device never sees the
   restricted ops' lamport values, so its clock lags. Conflict resolution is
   per-path last-writer-wins and the divergent ops touch no shared path, so
   convergence on visible paths is unaffected — pinned by
   `TestHiddenOpsDoNotBreakConvergenceOnVisiblePaths` rather than left as an
   argument, since a device whose clock lags by ten ops still has to win a
   later write on a shared path.

## Phases

Ship value before the expensive half. **Do not advertise "invisible" until
Phase 3 lands** — Phase 2 hides a folder from the browser while it is still on
every member's disk, which is a worse lie than not shipping.

### Phase 0 — model and admin surface

- [x] `FolderRule`, `Project.Folders`; persisted through both `MetaStore`
      backends with a migration — `project_folders` + `project_folder_perms`,
      row-scoped writes (`PutFolder`/`DeleteFolder`) so a rename by a second
      hub process cannot resurrect a removed rule.
      `TestMetaStoreFolderRuleConformance`.
- [x] `pathPermOf` + `ruleFor` (longest prefix, normalized, always
      slash-terminated). `TestFolderLevelResolvesLongestPrefix`,
      `TestFolderRuleWithoutDefaultInherits`,
      `TestFolderLevelFailsClosedOnAJunkGrant`, `TestNormPrefix`.
- [x] `GET/PUT/DELETE /api/p/{id}/folders` — admin only, reusing `grantable()`.
      `TestFolderRulesAreAdminOnly`, `TestFolderRuleRefusesAdminLevel`,
      `TestFolderGrantRefusesANonMember`, `TestFolderRulesUpsertAndClear`.
- [x] `db_conformance_test.go` covers folder rules on every backend.
- [x] No enforcement anywhere yet. `TestFolderRulesDoNotYetGateContent`.

Three things Phase 0 found that the draft did not have:

- **`scopeTag` replaces the stored epoch** (above). `TestScopeTagIsPerReader`.
- **A hidden rule is not listed to its subject.** Project grants are readable
  by any member on purpose (BEA-69: a viewer seeing who has access is what
  makes "why can't I edit this?" answerable). A folder rule can describe a
  folder the caller may not know exists, so `visibleFolders` filters the admin
  API's own output. `TestHiddenFolderRuleIsNotListedToItsSubject`.
- **`project_folders` is a GUARDED TABLE.** `CREATE TABLE IF NOT EXISTS` hands
  a rollback or an older dump an empty rule set, and an empty rule set reads as
  "no folder is restricted" — every confidential subtree re-opened silently.
  `requireTables` fails closed the same way `addColumns` does for a guarded
  column, and `schemaVersion` is now 2.
  `TestSQLMigrateRefusesALostFolderTable`.

### Phase 1 — read-only folders, end to end

The Viewer/Editor half. Needs no journal filtering.

- [x] `handleStorePut`: every op's path at `PermWrite` for the caller, else 403
      — the whole journal, not the offending ops.
      `TestSec_Folder_ReadOnlyMemberCannotPushOpsForThatFolder`,
      `TestSec_Folder_OneBadOpRefusesTheWholeJournal`.
- [x] All three `upload/*` routes, `restore`, `remove`, `undo-run`, `shares`
      POST: `PermWrite` on the path. `store/sign` is unchanged and correct
      (above). `TestSec_Folder_ReadOnlyMemberCannotWriteThroughAnyBrowserDoor`,
      `TestSec_Folder_ReadOnlyMemberCannotShareOutOfTheFolder`, with
      `TestFolderRuleLeavesTheRestOfTheProjectWritable` and
      `TestFolderRuleNeverBlocksAnOrgOwner` as the controls.
- [x] `GET /api/p/{id}/scope` returns `readonly` for the caller — and
      deliberately not `deny` (below). `TestFolderScopeReportsReadOnlyButNotHidden`.
- [x] Syncer honors `readonly` via `remote.Scoper`: materialize yes, journal
      never (edits AND deletes), local edit reverted with the user's bytes kept
      beside it, reported by `bdrive sync`.
- [x] Multi-device tests in `internal/syncer`:
      `TestReadOnlyFolderRevertsLocalEditAndNeverPushes`,
      `TestReadOnlyFolderRestoresALocalDelete`,
      `TestReadOnlyFolderIsQuietWhenUntouched`,
      `TestReadOnlyScopeSurvivesAnUnreachableHub`,
      `TestNoScopeSupportClearsTheRestriction`.
- [x] UI: a Folders card in Project Settings, reusing the People matrix's
      levels, markup and edit flow.

What Phase 1 changed in the plan:

- **`/scope` reports `readonly` only.** A `deny` list would hand every member
  of a project the NAME of every hidden folder — most of what "invisible" is
  supposed to mean — and nothing in Phase 1 consumes it. Phase 3 has to solve
  delivery of denial without publishing that list; see §Known gaps 6.
- **A dirty read-only file is the one file materialize may clobber.**
  Everywhere else "dirty" means the next scan commits it; under a read-only
  folder the next scan will not, so leaving it means a file that silently stops
  receiving the project's updates forever. The user's bytes are copied aside
  first, under the same prefix, so the copy is not journaled either.
- **The cache short-circuit had to be taught about it.** `materializeFile`
  returns early when the cache agrees with the target, and a local edit changes
  neither — so the revert was unreachable until `readOnlyDrifted` stat'd the
  file. Found by writing the test, not the code.

### Phase 2 — hidden folders on the browser surfaces

- [x] A per-request `pathFilter`, **not** a per-reader fold (below).
- [x] `tree`/`resolve`/`file`/`download`/`render` return 404 for a hidden path,
      gated at the one `lookup` choke point they share.
      `TestSec_Folder_HiddenFolderIsNotReadableThroughAnyViewerDoor`,
      `TestSec_Folder_HiddenFolderIsAbsentFromTheTree`.
- [x] `history`, `blob?sha=`, `render?sha=`, `heat` (including `?by=device`),
      `shares` list, `reads` ingest filtered.
      `TestSec_Folder_HistoryHidesTheFolder`,
      `TestSec_Folder_HeatHidesTheFolderAndRefusesReportsAboutIt`,
      `TestSec_Folder_ShareListHidesLinksIntoTheFolder`.
- [x] `/s/{token}` dies with the folder —
      `TestSec_Folder_ARestrictedFolderKillsItsOlderShareLinks`.
- [x] Docs state the folder still syncs to disk (README, concepts/permissions).
- [ ] ~~Feature-flagged off by default~~ — **not built**, deliberately (below).

Three changes to the plan:

- **A predicate at the use sites, not a cached per-reader fold.** The draft
  wanted `RemoteSource` to keep a filtered fold per scope hash. A `pathFilter`
  resolved once per request needs no cache, no eviction and no memory per
  distinct scope: single-path routes pay one string comparison, and the listing
  routes already iterate every entry. This deletes §Known gaps 4 for the
  browser surfaces; Phase 3's filtered JOURNAL is a byte stream rather than a
  folded map and still needs that cache.
- **A dead share link answers 404, not 410.** A 410 would tell a stranger
  holding a revoked link apart from a stranger guessing one — a distinction
  `/s/*` has never made, and starting to make it for the one case that is about
  a folder somebody wanted private is backwards. It reuses the existing body,
  verbatim.
- **No feature flag.** The flag existed to stop the half-state — reads hidden,
  bytes still syncing — being sold as confidentiality. Two things already do
  that, and better: the UI does not offer "No access" on a folder at all (only
  a deliberate API call reaches this state), and the README and the permissions
  page both say in plain words that a folder rule controls who can change a
  folder, not who can see it. A flag on top would mean Phase 3 must remember to
  flip it, and a half-filtered state behind a config knob is harder to reason
  about than a coherent one that is documented. The honest gate is the sentence
  in the docs, and it is there.

### Phase 3 — hidden folders on the sync wire (the actual claim)

- [x] Line-drop journal filter on the STORED bytes; `store/list` reports the
      filtered length, from the same filter and the same fetch.
      `TestSec_Folder_TheSyncWireNeverCarriesAHiddenOp`,
      `TestSec_Folder_ListedJournalSizeMatchesTheFilteredBody`.
- [x] Visible-sha gate on `store/object`, `store/exists` and the blob listing.
      `TestSec_Folder_BlobListingHidesUnreadableContent`.
- [x] Scope tag on `/scope`; the client drops its peer journals and re-pulls on
      a change. `TestFolderScopeTagIsPerAccount`,
      `TestScopeChangeReSyncsFromZero`,
      `TestScopeChangeNeverDropsOwnJournal`.
- [x] `X-Bdrive-Perms` capability, gated on whether THIS caller is filtered
      rather than on whether the project has rules — an org owner sees every op
      either way, and refusing an administrator's device over somebody else's
      folder would be wrong. `TestOldClientIsRefusedOnAProjectWithFolderRules`,
      `TestOldClientKeepsWorkingWithoutFolderRules`.
- [x] `deny` in `/scope`; the syncer never journals under it.
      `TestHiddenFolderNeverReachesTheDevice`,
      `TestFolderScopeReportsWhatADeviceMustNotWrite`.
- [x] Local copies are purged on revocation — see §Known gaps 1.
      `TestLosingAccessRemovesTheFolderFromTheDevice`.
- [x] Multi-device tests (above) and a `sec_*` case per §Surfaces row.
- [x] Docs updated (README, concepts/permissions); the UI offers `No access`.
- [x] Move across the boundary in both directions, and lamport divergence
      (§Known gaps 3 and 6) — both behave as reasoned.
      `TestMoveAcrossAHiddenBoundary`,
      `TestHiddenOpsDoNotBreakConvergenceOnVisiblePaths`.

Phase 3's corrections:

- **`deny` is published after all.** Phase 1 withheld it to avoid leaking
  folder names. Phase 3 reverses that deliberately: a device syncs a real
  filesystem, so it must know which paths never to journal, or a member who
  creates a colliding local path has their whole journal PUT refused and their
  sync wedges permanently. The alternative — teach the client on refusal —
  discloses the same name to the same person the moment they trip it, needs new
  client machinery for a rare case, and leaves a silently unsynced file until
  then. The Phase 1 test was rewritten into
  `TestFolderScopeReportsWhatADeviceMustNotWrite`, not deleted, with the
  reversal recorded in the test itself.
- **Revocation purges an honest device** — §Known gaps 1 was too pessimistic.
- **The capability check is per-caller, not per-project**, caught by the test
  that asserted an org owner is not refused.

### Phase 4 — revocation hygiene

- [ ] Admin sees "N devices synced this folder before it was restricted".
- [ ] Restrict dialog warns when content is duplicated at a visible path.
- [ ] Rule changes land in the activity log with actor and timestamp.

## Testing

Per CLAUDE.md, a sync feature without a multi-device test is untested where it
matters. Every wire-level box above names its `internal/syncer` test; every
surface row in §Surfaces gets a matrix case in `internal/webapp/sec_*_test.go`
driven by a member who is *inside* the org and project but outside the folder —
the exact principal this feature exists to stop, and the one an org-wall test
never exercises.

## Docs

- `web/docs/.../reference/hub-config.md` — the folders API and the four levels.
- `web/docs/.../guides/` — a "restrict a folder" page written as what to ask an
  agent, per the agent-first sidebar rule.
- `README.md` — only if `bdrive` grows a folder-permission command.
- `CLAUDE.md` — the `webapp` paragraph currently describes the org wall as the
  only permission model and does not mention `perms.go` at all. It is already
  stale; this change is the moment to fix it.

## Review passes

The goal this work ran under required two consecutive review passes finding
nothing new. It took eleven passes to get there, and what they found is the
most useful record this document holds — every one was a hole or a break that
the phase's own tests had passed over.

| # | Found | Why the tests missed it |
|---|---|---|
| 1 | `/api/orgs/{org}/shares` handed a denied member the public token for a hidden file | Not behind `proj()`, so the context-based filter was inert there |
| 2 | `/resolve` leaked the new name of a file moved inside a hidden folder | A *live* path 404s there by accident; only a *moved* one leaked |
| 3 | The "that's a folder, try X" hint enumerated hidden filenames | A rule on `vault/` does not match the bare path `vault`, so the write gate passed it |
| 4 | `restore` could paste a version from a file's hidden past onto a visible path | Needs a sha no door reveals — defence in depth, not a live hole |
| 5 | The run card returned hidden paths an agent read | History publishes the session id, so the id was learnable from a visible op |
| 6 | *(clean)* | — |
| 7 | **A revocation bricked the member's journal for the whole project, permanently** | push re-sends the whole journal every cycle; nothing tested a rule change *after* a write |
| 8 | A hidden file's `manifests/` entry was fetchable, which is its contents | No fixture uses a file over the 4 MiB chunking threshold |
| 9 | `forgetPeerJournals` could delete this device's own journal | The guard only fails when a stat does |
| 10 | A filtered member's `bdrive export` lost every large file's content | Same 4 MiB blind spot, one consumer over |
| 11 | Read-only folders made a member "filtered", refusing their older devices for nothing | Over-refusal looks like working security from the inside |
| 12 | *(clean)* | — |
| 13 | The scope tag on the store listing was dead API surface, with a comment calling it load-bearing | Two tests asserted it, so it looked consumed |
| 14 | *(clean)* — every route in §Surfaces accounted for against the 30 gate call sites | — |
| 15 | *(clean)* — share revoke/expiry and the frontend's own rendering | — |

One route is deliberately left un-gated on the path, and is worth stating so
nobody "fixes" it later: `PATCH`/`DELETE /api/shares/{token}` revoke and expiry
take project write, not folder write. Revoking is destructive, not disclosing,
and the token is no longer obtainable for a hidden path through any listing —
so a member who could reach it already had it.

Two lessons worth keeping:

- **A filter that depends on where it is called from covers exactly the call
  sites someone remembered.** Passes 1, 2 and 5 are all that shape.
- **Delta sync has its own key space and it does not behave like the rest.** A
  chunk's key is its own hash and never an `Op.Blob`, so it is in no
  visible-sha set — and every fixture in the suite is under the threshold that
  would have shown it. Passes 8 and 10.

Fixing #7 also opened a worse hole than it closed (skipping every `Seq` the hub
already held let a device re-point an old `Seq` at a restricted path). The
Phase 1 test caught it within the same edit. That is the argument for keeping
the earlier phases' tests running while the later ones land.

## Status

**Phases 0-3 landed. A folder can now be genuinely private within a project.**

- **Read-only folders** (Phase 1): enforced on every write door and honoured by
  the client, so an edit is reverted with the user's copy kept beside it rather
  than wedging their sync.
- **Hidden folders** (Phases 2-3): 404 on every browser surface, and the sync
  wire never carries an op, a blob, or a listing entry for them — so the bytes
  are never written to a non-permitted device's disk at all. Losing access
  removes what is already there.
- **What still leaks, by design:** the folder's NAME, to project members whose
  devices have to know not to write there (§Known gaps 5). Everything else —
  the file names inside, the contents, the history, the heat, the bytes — does
  not.

Plan corrections, each recorded in its own phase: the stored `PermEpoch` became
a derived per-reader `scopeTag`; `store/sign` is not path-gatable; the browser
surfaces use a per-request predicate rather than a per-reader fold; a dead
share link answers 404 rather than 410; Phase 2 shipped without a feature flag;
and `/scope` publishes denied prefixes after Phase 1 decided it should not.

Phase 4 (revocation hygiene: the duplicate-content warning, the "N devices
synced this before it was restricted" count, and an audit-log entry per rule
change) is the remaining work, and was always outside this goal's scope.

**One thing a future phase should fix:** no fixture anywhere in the suite uses
a file over the 4 MiB chunking threshold, so the chunked path through a
filtered account is covered by reading and by construction, not by a test. Two
of the eleven review findings lived there. A single large-file fixture would
have caught both.
