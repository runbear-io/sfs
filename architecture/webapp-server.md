# `bdrive serve` server — class diagram

Source of truth: `internal/webapp` (server, services, persistence) and
`internal/remote` (storage backends). Reflects the code as of this commit;
update this file in any PR that changes these types or their relationships.

## Server core, sources, and services

```mermaid
classDiagram
    direction LR

    class Server {
        +Source Source
        +Volume string
        +Root remote.Backend
        +Projects *ProjectDB
        +Device Identity
        +Refresh time.Duration
        +Upload UploadConfig
        +Auth AuthProvider
        +Devices *DeviceRegistry
        +Shares *ShareDB
        +Reads *ReadLedger
        +Dir Directory
        +Quota QuotaProvider
        +Billing func(email) (plan, url, ok)
        +Analytics AnalyticsConfig
        +ShareRPM int
        +TrustProxy bool
        -vols per-project volume cache
        -grants reservation ledger
        +Handler() http.Handler
    }
    note for Server "clientIP is a METHOD now, not a package func: X-Forwarded-For is honored when the PEER is loopback/private (the operator's own proxy) or TrustProxy is set, and then only its LAST hop. Every caller that gates on an IP — the auth rate limiter, /s/*, device rows, share telemetry — goes through it, so a client-supplied header cannot forge the identity a limiter counts"

    class volume {
        -source Source
        -refresh time.Duration
        -snap *snapshot (files + moves)
        +snapshot(ctx)
        +invalidate()
    }

    class Source {
        <<interface>>
        +Files(ctx) map path→FileInfo
        +Open(ctx, path, fi) io.ReadCloser
    }
    class DirSource {
        +Dir string
    }
    class RemoteSource {
        +Backend remote.Backend
        +Device Identity
        +PresignTTL time.Duration
        +Remove(ctx, path, who, note)
        +OpenBlob(ctx, sha)
        -reassemble(ctx, sha) chunked fallback
        -verify(ctx, sha) re-hash until sealed
        -blobStat(ctx, blob) remote.Object
        -sealed sync.Map sha→proved immutable
        -jcache map key→cachedJournal
        -jbytes int64 raw bytes cached
        -loadSourcedOps(ctx) []sourcedOp
        -cacheJournals(keep, misses, parsed, sizes)
        -appendOp(ctx, op)
        -appendOps(ctx, ops) ONE read-modify-write
    }
    class sourcedOp {
        +Op journal.Op
        +From journal key's device
    }
    class cachedJournal {
        +size int64
        +mod time.Time
        +bytes int64
        +ops []journal.Op
    }
    note for cachedJournal "Journals only GROW — a device appends only to its own, and appendOp rewrites its key with strictly more bytes — so the (Size, Modified) List already reports proves a parse is still current. That is why the cache needs no expiry and no new Backend method: loadSourcedOps still Lists on every request, and fetches only the keys whose size or mtime moved (concurrently, limit 8). History used to re-download and re-parse EVERY journal per page, which is what made it 8-10s and made paging cost more rather than less. Bounded by a per-project raw-byte cap with all-or-nothing eviction"
    note for sourcedOp "An op's Device field is whatever the writer typed; From is the journal object it actually came out of, which the /store door gates. Attribution reads From — a peer cannot sign someone else's name on a change by editing its own journal"
    note for RemoteSource "OpenBlob is the single blob-read door: the sha must match blobRe, and verify re-hashes the bytes whenever the backend is a PutSigner — in direct-upload mode the server never saw the content, so the store is the only thing that could have swapped it. It stops re-hashing only once the object is PROVABLY immutable: both presign doors refuse a key that exists, so every URL for a blob was minted before its first PUT and dies at mint+PresignTTL; past that age the hub is the only writer left. That is what remote.Object.Modified is for"
    note for RemoteSource "reassemble is delta sync's whole backward-compatibility story: when blobs/sha is absent (or fails verify), the manifest under manifests/sha names content-defined chunks that concatenate to the blob — spool, hash-verify against the sha requested, BACKFILL blobs/sha (best-effort; a failed write never fails the read), serve. Old clients ask for whole blobs and never learn anything changed; a hostile whole blob that fails verify is HEALED by the backfill when an honest manifest exists. Bounded by maxReassembleBytes (256 MiB) on the manifest's DECLARED sum, with each chunk's copy capped at its declared size — a member-written manifest is the one non-content-addressed object in the key space, so an amplified or oversized one is refused before it can spool"
    class MoveSource {
        <<interface>>
        +FilesWithMoves(ctx) files, moveIndex
    }
    class moveIndex {
        <<map path→[]pathEvent>>
        +buildMoveIndex(sorted ops)
        +resolveForward(idx, files, p) viewer
        +resolveShare(idx, files, p, since) /s/
        +chainSegments(idx, p) []segment
        +resolveFolder(idx, files, dir) all-or-nothing
    }
    class pathEvent {
        +At the delete that ended it
        +To ""  = deleted, not moved
        +ToAt destination's create
    }
    class segment {
        +Path
        +From, To window it WAS the file
    }
    note for moveIndex "There is no rename op — a move is put(new) + delete(old), same device, same blob, one cycle — so the index is DERIVED inside the replay Files already runs and cached with the snapshot. Pairing needs same device, |Δt| ≤ 30s, B's first-ever put, and one-to-one both ways; anything ambiguous stays a plain deletion. Nothing here writes an op: journal.Less and Replay are untouched"
    note for segment "Time-bounded on purpose: a bare set of paths would make history?path=docs/a.md show the ops of the NEW a.md that took the old address"

    class Uploader {
        <<interface>>
        +Upload(ctx, path, r, size, who, note)
    }
    class DirectUploader {
        <<interface>>
        +SignBlobPut(ctx, blob, size, ttl)
        +BlobSize(ctx, blob) size, exists
        +Commit(ctx, path, blob, size, who, note)
    }
    note for DirectUploader "BlobSize replaced HasBlob: in direct mode the server never sees the bytes, so the CALLER's declared size was the only number it had to quota-check and journal — and the caller picks it. Size now comes from storage, and the commit journals and charges that"
    note for DirectUploader "Commit's note is &quot;&quot; for an upload and &quot;restore &lt;path&gt;@&lt;sha8&gt;&quot; for POST /api/p/{id}/restore — which is the upload commit minus the upload: find the historical op for (path, sha), journal a NEW put at its blob. Never rewrites a journal."
    note for RemoteSource "Every write ends at appendOps: stamp Seq/Lamport/Time + this server's Identity across the batch, append N ops to journal/&lt;own-device&gt;.jsonl in ONE read-modify-write (appendOp is the single-op call). Commit does that for a put; Remove (POST /api/p/{id}/remove, restore's gates + a snapshot existence check) does it for a delete — the only server path that takes a file away, and itself undone by restoring the DELETED row. The batch is not an optimization: ONE Put of ONE object either lands or it does not, which is the whole atomicity argument for undoRunDoor — a loop of appendOp there would leave half a run reverted with nothing to report it."

    class undoRunDoor {
        <<Server, POST /api/p/id/undo-run>>
        planUndo(sourced, undoSel) undoPlan
        undoSel Device From-journal, Session xor Note
        undoPlan Ops, Actions, Skipped, After, Refused
        preview plan only, no write, no quota
    }
    note for undoRunDoor "The run-wide form of restore+remove: for every path the run touched, the op that puts it back — a put at the pre-run blob, or a delete for a file the run created. Selection is by sourcedOp.From (the journal, which /store gates), NEVER op.Device, and the note form additionally requires Session == &quot;&quot; because runs.ts can never file a session-carrying op under a note-keyed card. Append-only: the run's own ops are never touched. Same PermWrite + CheckWrite(org,0) gates as its two siblings; the undo's ops carry a note naming the run, so the undo is itself a run card you can undo."

    class journalDoor {
        <<Server, /api/p/id/store/*>>
        ownJournal(key) whose journal is this
        journalOps(key, spooled) parse + validate
        opsNameTheirAuthor(ops) whose name
        journalKeepsItsOps(ctx, be, key, ops) ok + storedMax
    }
    note for journalDoor "store.go — the invariant &quot;each device writes only its own journal&quot; is now ENFORCED here, not assumed. The key must be journal/&lt;canonical device id&gt;.jsonl for the device in the request header, that device must already be owned by the caller (DeviceRegistry.OwnerOf) or the caller must be a project admin (the recovery arm) — the old first-writer-claims arm is gone. Every op must pass journal.SafePath + config.ReservedPath on its Path and journal.SafeText on Note/Author/UserName, must name its own owner's account, and the upload must keep every Seq the stored journal already had: append-only, 409 on truncation. Bodies are spooled first, and a blob PUT must hash to the key it claims. A Content-Encoding: gzip body is inflated ABOVE the spool — the sha, the op count and the billed size are all properties of the plaintext, so nothing below that line knows compression happened — and the inflate is bounded (maxInflatedPut, 256 MiB, only when an encoding was declared), because compression severs the one-wire-byte-one-disk-byte relationship that made spool safe unbounded"
    note for journalDoor "Delta sync grew the key space: validStoreKey also accepts chunks/&lt;sha256&gt; (content-addressed, PUT must hash to its key, presigned like blobs incl. refuse-existing) and manifests/&lt;sha256&gt; (keyed by the whole FILE's sha — not its own content hash — so it is never presigned and gets two ingest gates instead: every chunk it names must already EXIST in the store, and the key is WRITE-ONCE — an identical re-put is a 200 no-op so an interrupted push can retry, a different body 409s. Together these make &quot;a manifest exists ⟹ its chunks exist&quot; an invariant every consumer can lean on: the client's push skip-proof, reassemble, and bdrive import)"

    class Backend {
        <<interface>>
        +Put +Get +List +Exists +Close
    }
    class PutSigner {
        <<interface>>
        +SignPut(ctx, key, size, ttl)
    }
    note for Backend "internal/remote — impls: localBackend (file://), s3Backend, gcsBackend, httpBackend (https:// hub), Prefixed wrapper"
    note for Backend "Key handling is fallible now: Prefixed.key and localBackend.path RETURN AN ERROR (safeKey / store.UnderRoot) rather than concatenating, so a `..` key cannot walk out of a project's prefix or out of a file:// root — and Prefixed.List re-checks the STRIPPED key on the way out, since the prefix it removes is the only thing that was ever validated. The httpBackend client is origin-bound: the device token is keyed to settings.Server, SameOrigin is the one rule, refuseOffOriginRedirect is its CheckRedirect, a presign target must be https on a trusted origin (directTargetOK), and List drops keys failing journal.SafePath and clamps a negative Size. gcs SignPut now signs Content-Length too. Object carries Modified (S3 LastModified, GCS Updated, file mtime; zero where the backend has none) — RemoteSource.verify reads it to decide when a blob can no longer be rewritten by a presigned URL"

    class wireCodec {
        <<internal/remote, compress.go>>
        +Compressible(r) (rejoined, worth, err)
        +AcceptsGzip(req) bool
        putPlan.AcceptEncoding []string
    }
    note for wireCodec "Transport compression, and TRANSPORT ONLY: content addressing, the storage layout and the journal format are all over the UNCOMPRESSED bytes. One probe helper serves both legs — it gzips the first 64 KiB and keeps the compressed form only if the sample shrank, so already-compressed content (JPEG, zip, weights) passes through untouched; the reader it returns is the stream REJOINED, since a probe that eats bytes corrupts every transfer. The two legs are not symmetric. PULL needs no negotiation: net/http sends Accept-Encoding: gzip itself and inflates transparently, so devices built before this get it free — which is why httpBackend.do must never set that header. PUSH cannot be unilateral, because an old hub would store the gzip bytes under the sha256 of the plaintext, so the client compresses only when sign() advertised accept_encoding (absent on an old hub → raw). putDirect stays raw: a presigned upload has no hub in the path to inflate it"

    class AuthProvider {
        <<interface>>
        +CLILoginPath()
        +Authenticate(r) User
        +Register(mux)
        +Accounts() []User
        +UseDeviceBinder(bind)
    }
    note for AuthProvider "UseDeviceBinder is a PRECONDITION of ownJournal, which refuses a journal write for every provider: the provider must call the hub's binder at every token mint. It was a field on BuiltinAuth wired behind a type assertion, so a managed provider bound nothing and every push 403'd forever. On the interface so a provider that ignores it does not compile. The hub cannot do it instead — Authenticate reports who a request is, never which credential class it used, and a device token must not reach a bind"
    class AccountApprover {
        <<interface>>
        +PendingUsers() +Approve +Deny +SetPolicy +Policy
    }
    class BuiltinAuth {
        +AllowSignup bool
        +AllowedDomains
        +RequireVerification bool
        +RequireApproval bool
        +Admins
        +InviteValid func(token)
        +BindDevice func(email, r) error
        +Offboard func(email)
        +BaseURL string
        +Seniority() []string
        -store AccountRepo
        -users, tokens, pending
        -cli CLIAuth
        -refresh re-reads the store before every decision
    }
    note for BuiltinAuth "Revocation is now a real edge, not a cookie change: revokeTokensFor / revokeGrantsForLocked kill every device token and pending CLI grant an account holds, DELETE /api/auth/token revokes one by name, and the same path runs on offboarding. BaseURL replaces trusting the request's Host when composing a reset or verification link — a mail link built from an attacker's Host header is a credential delivered to the attacker"
    class CLIAuth {
        +Register(mux)
        -session func(r) User
        -issue func(w, r, user, device)
        -pending map~cliGrant~
        -pkceOK(challenge, verifier)
        -takeGranted single-use, one lock
        -atGrantCap per-IP and global caps
    }
    note for CLIAuth "The paths bdrive login POSTs by name, served the same way for every provider: /auth/cli, /auth/device/&lt;token&gt;, /api/auth/exchange, /api/auth/device/start, /api/auth/device/poll."
    note for CLIAuth "The loopback flow is PKCE (S256) with NO compatibility arm — a grant without a challenge is refused, because the code rides in a URL the browser and anything watching it can see. cliGrant now separates the LINK the human opens from the credential the CLI polls with, takeGranted consumes a grant inside one critical section (peek-then-take let two pollers win the same code), and pending grants are capped globally and per IP with heaviest-first eviction so the map is not a free memory sink"
    class Mailer
    class User {
        +ID +Email +Name +Admin
    }

    class Directory {
        <<interface>>
        +Role(org, email)
        +Get +OrgsFor +ListInvites +ValidInvite +ManageURL
        +Create +Rename +AddMember +SetRole +RemoveMember
        +CreateInvite +RevokeInvite +Redeem
    }
    class LocalDirectory {
        +ManageURL(orgID)
    }
    class OrgDB {
        -repo OrgRepo
        -byID, invites
        -seniority func() []string
        +EvictMember(org, email)
        +SetSeniority(f)
        -heir(o) promotes an owner
        -refresh re-reads the store before every decision
    }
    class Org {
        +ID +Name +Members email→role +Created
        +Joined email→when
    }
    class offboard {
        <<Server, orgs.go>>
        drop every project grant
        Devices.Release(email)
        evict from every org
    }
    note for offboard "Deleting an account used to leave its access behind: project grants keyed by email, device rows that still owned journals, org memberships. offboard is the one gesture that unwinds all three, and the two tiny interfaces it goes through (orgEvictor, seniorityLister) keep BuiltinAuth and OrgDB from importing each other. EvictMember has no last-owner guard — it promotes an heir instead: earliest Org.Joined, ties broken by account seniority, and no orphan org if there is no evidence"
    class OrgInvite {
        +Token +Org +Creator +Expires +Uses
    }

    class ProjectDB {
        -repo ProjectRepo
        -byID
        +Get +Create +Update +Rename +List
        +SetCreator +SetDefault +SetTemplate
        +SetPerm +ClearPerm
        -refresh re-reads the store on reads AND mutators
    }
    class Project {
        +ID +Name +Org +Created
        +Description +Icon
        +Creator string
        +Template string
        +Default string
        +Perms map email→level
    }
    class seedTemplate {
        <<Server method>>
        POST /api/projects `template`
        templates.Get before GetOrCreate → 400
        Upload() per file, hub's own device
        skips paths that already exist
        CheckWrite / RecordUsage
    }
    note for Project "Default == &quot;&quot; means write — the historical behavior, so an upgraded hub needs no migration. SetPerm/ClearPerm refuse to drop the last explicit admin."

    class projectPerm {
        <<resolver>>
        org owner → admin
        explicit grant → that level
        org member → project Default
        otherwise → none
    }
    note for projectPerm "perms.go — the single authorization ladder. proj(level, h) in server.go is the one choke point: every per-project route declares its level at registration."
    note for projectPerm "Both escape hatches are closed: a project with no org, or naming an org that no longer exists, resolves to none instead of falling through to a default, and org membership is checked BEFORE an explicit grant — so a grant left behind by a removed member is no longer a way back in"

    class ShareDB {
        -repo ShareRepo
        -byToken
        +Create +Get +Revoke +SetExpiry
        -refresh re-reads the store before every decision
    }
    note for ShareDB "A share is now re-checked at READ time, not only at mint time: shareCreatorStillBelongs refuses /s/&lt;token&gt; once its creator has left the project's org, so a link cannot outlive the access that justified it"

    class secretScan {
        <<internal/secrets>>
        +ScanLimit = 1 MiB
        secretRules six anchored regexes
        +Scan(buf) []Finding
        +Label(rule) human words
    }
    class secretFinding {
        <<secrets.Finding>>
        +Rule string
        +Line int
    }
    class renderFindings {
        <<Server, server.go>>
        +renderFindings(src) []Finding
        caps at ScanLimit, then Scan
        findings on the render response, omitted when empty
    }
    note for secretScan "No longer a webapp file: internal/secrets is stdlib-only so internal/syncer can run the SAME rules on the path every file takes (the sync scan, warn-only — see cli-sync.md). The rule ids are a wire contract, keyed off by lib/secrets.ts's SECRET_LABELS and by Label, whose test asserts it covers every rule"
    note for secretScan "Mint-time gate on handleShareCreate: the one place a member turns private bytes into a public URL is the one place the bytes are read first. It returns rule ids and LINE NUMBERS only — the matched text never reaches a response body, a log line, or a metric label, the same rule ReadLedger keeps for actor identity. Bypassed by confirm:true (bdrive share --force, the UI's Share anyway) and by Server.alreadyPublic, since a path that already has a live link is public already. Fails CLOSED: an unreadable blob is 503, not a silent pass"
    note for renderFindings "The SECOND caller, and the reason the gate is no longer the only one (BEA-147): minting is the rarest path in the product, so a hub that could name an AWS key on line 3 well enough to refuse to publish a file rendered that same key to every member as prose. handleRender and renderVersion already hold the bytes RenderMarkdown needs, so the cap is a slice rather than the LimitReader the two streaming callers use — same ScanLimit, so the badge and the share dialog can never disagree about one file. Advisory: findings ride along on the render response (omitted when empty), nothing is blocked and nothing is redacted, because a member who can open the file could already read the key"

    class sandboxInline {
        <<Server, every bytes-out route>>
        inlineMarkup(ct) / inlineType(ct)
        nosniff always
        CSP sandbox allow-scripts for markup
        setContentLength from the stream
        storageErr logs detail, says little
    }
    note for sandboxInline "One helper for every route that returns stored bytes — serveBlob, render, a historical version, the history blob view, and /s/*. Uploaded HTML/SVG/XML is a document a member can author and another member will open on the hub's origin; the sandbox header is what keeps it from acting as the hub. Length is measured from the stream rather than trusting a recorded FileInfo.Size"
    class Share {
        +Token +Project +Path +Creator +Expires
    }

    class markdownRender {
        &lt;&lt;markdown.go&gt;&gt;
        frontmatterPairs(src) pairs + body
        RenderMarkdown → table + body HTML
        RenderMarkdownPairs → pairs + body HTML
        yamlValue(node) text + code flag
    }
    class FrontmatterPair {
        +Key +Value +Code
    }
    note for markdownRender "One parse, two surfaces. The share page is the reason the split exists: RenderMarkdown still bakes the key/value TABLE into its HTML, because every /s/ link ever minted serves that output, while the viewer takes RenderMarkdownPairs and gets the frontmatter as DATA so it can hang it beside the prose instead of on top of it (BEA-154). Values cross the wire as literal text plus a code flag, never pre-escaped markup — the panel is a React text node, so escaping is the client's by construction; the table escapes on its way out. shares_test pins the table, because nothing else would fail if someone later tidied shares.go onto the pairs path"

    class mermaidTag {
        &lt;&lt;shares.go, the .md branch&gt;&gt;
        body contains language-mermaid?
        → module script tag, else ""
        sharedMarkdownShell verb 2 of 4
    }
    note for mermaidTag "A share page is a zero-JavaScript document and stays one unless the document it renders actually has a diagram — the server already holds the rendered HTML, so it can decide. The tag is a MODULE, which only loads because frontend() now sets Access-Control-Allow-Origin on real assets: under this page's `sandbox allow-scripts` the origin is opaque, so a module and every import() it makes are fetched with Origin: null. The CSP itself is unchanged, and no allow-same-origin was added — the sandbox is what keeps shared content off the hub's origin"

    class DeviceRegistry {
        -repo DeviceRepo
        -byKey devKey → row
        -latest id → newest key
        +Observe(DeviceInfo)
        +Bind(user, d, visible) error
        +Release(user)
        +OwnerOf(id) owner, known
        +MayActAs(user, id) bool
        +LookupIn(id, allowed) DeviceInfo
        -refresh re-reads the store before every decision
    }
    class devKey {
        +User account email
        +ID device id
    }
    class DeviceInfo {
        +ID +Name +OS +User +IP
        +FirstSeen +LastSeen
    }
    note for DeviceRegistry "A device is keyed by (account, id), not by id alone — two accounts naming the same id hold two independent rows, so nothing a stranger sends can overwrite yours. Hub-wide ownership is FIRST CLAIM WINS, decided by FirstSeen. Bind, at token issuance, is the only thing that creates a row for an id that has never synced: it refuses an id another account already owns when the caller can see that account (same org) and otherwise binds nothing and says nothing, so the error is never a cross-org existence oracle. OwnerOf is the write gate the /store journal door asks; MayActAs is the looser telemetry gate (mine, or unclaimed); Release is offboarding's half. IDs are shape-checked and lower-cased at one door (deviceID), so case alone can't fork an identity"

    class ReadLedger {
        -repo ReadRepo
        -sessions SessionReadRepo
        -retention, sessionRetention
        -byKey, dirty, seen, pendingSess
        +Record(...)
        +RecordSession(project, session, device, path)
        +Heat(project, prefix, days)
        +SessionPaths(project, session, device)
        +WithSessions(repo, days)
        +ShareOpens(project)
    }
    class ReadStat {
        +Project +Path +Day +Kind +Actor +Count +Last
    }
    class SessionRead {
        +Project +Session +Device +Path +Last
    }
    note for SessionRead "One row per (session, device, path) — the per-session detail a History run card joins its writes to, on the un-forgeable Op.Session and never on the note. Deliberately OUTSIDE ReadLedger.byKey: that map is loaded whole at boot and full-scanned by Heat on every request, hub-wide, so session cardinality in it would slow the Dashboard for projects that never ran an agent. Device is always the ownsDevice-validated id, never a client field, so a report naming someone else's session can only ever be found under the forger's own device. Its own, much shorter retention (session_retention_days, default 30) DELETES rather than folds — no heat total was ever derived from it"
    class HeatEntry {
        +Human +Agent +Share +Readers +LastRead
    }
    class ShareOpen {
        +Count +Last
    }
    note for ShareOpen "The receipt on a public link: share-kind buckets only, which is what makes Last mean last OPENED — HeatEntry.LastRead is cross-kind, so a member viewing the file in the hub would otherwise move the date. Counts, never openers: the share actor is token+IP+UA hash (token+IP alone folded a whole office into one reader, BEA-151 — the browser component is a heuristic that raises the floor, never identity, and is hashed because Record persists the actor). Keyed by path, so two tokens on one file report the same number. Callers build the map ONCE per project and index it; a per-share call is a full byKey scan per row"

    class QuotaProvider {
        <<interface>>
        +CheckWrite(org, bytes)
        +CheckSeat(org, members)
        +RecordUsage(org, bytes)
        +CheckRead(org, bytes)
        +RecordEgress(org, bytes)
    }
    class UnlimitedQuota
    note for QuotaProvider "The read half is deliberately ASYMMETRIC. CheckRead is called on /s/* and nowhere else: a public share link is the only unauthenticated door to stored bytes, so it is the only egress a plan can cap and the only bandwidth number worth publishing. Refusing a device mid-sync would surface as ErrForbidden, which the syncer reads as 'access is gone — pause and touch nothing', so /store/* and the viewer only RecordEgress. Sync must never break over a bill"
    class countingWriter {
        <<quota.go>>
        +Write(p) n
        +n int64
    }
    note for countingWriter "Bills what actually reached the client. FileInfo.Size and the journal's Size are claims made BEFORE the write; a reader who abandons a download halfway must not be charged for the whole file. It wraps the SOCKET and gzip writes INTO it, never the reverse — RecordEgress is a bandwidth meter, so a compressed response must report its compressed size, and getting that backwards bills plaintext while nothing fails"

    class grant {
        +project +org +key
        +size +expires
    }
    class reservations {
        <<Server, reserve.go>>
        reserve / reserveIfFits(org, size, ttl)
        reservedBytes(org) / outstandingLocked
        claimGrant(project, key)
        reconcileGrants(ctx, project, be)
        dropStaleLocked
    }
    note for reservations "The seam between a presigned URL and the plan it spends. CheckWrite alone answered per request, so N concurrent signs each passed against the same free space and the org wrote N times its quota. Every write door now charges size + reservedBytes(org), reserveIfFits is the compare-and-set, and a signed-but-unspent grant expires. reconcileGrants asks the backend whether the blob actually landed and either RecordUsage-s it or gives the space back — the reservation is a hold, never the accounting"

    class AnalyticsConfig {
        +Key string
        +Host string
        +Endpoint() string
    }
    note for AnalyticsConfig "Third managed-deployment seam beside Quota and Billing, but a value rather than an interface — there is nothing to implement, only a project to name. Emitted as /api/config `analytics` when Key is set; empty means the frontend loads no tracker and contacts nobody, which is what a self-hosted hub gets. Endpoint() is exported because the cloud module renders its own loader from the same value."

    class productAnalytics {
        <<Server, analytics.go>>
        capture(email, event, props) one POST, own goroutine
        captureChange(r, source, puts, deletes) files_changed
        countOps(ops, storedMax) new ops only
    }
    note for productAnalytics "The events the browser cannot see: a device syncing through /store/* never loads a page, so an agent editing files all day is invisible to the frontend's tracker. Every write door — sync, upload, remove, restore — funnels through captureChange so the file-change count is ONE event rather than a per-route set that silently misses whichever route someone forgets, and distinct_id is the same email analytics.ts identifies with so a person is not counted twice. No SDK: posthog-go would ship a tracker inside every self-hoster's binary, and capture is one JSON POST to Endpoint() + /i/v0/e/ that does nothing at all when Key is empty. Telemetry never fails a request — the POST is a goroutine and its error is a single log line. countOps needs journalDoor's storedMax because a device PUTs its WHOLE journal every cycle: counting the body would re-report the device's entire history every ten seconds. Blob PUTs are deliberately not change events, since content-addressed storage skips a blob it already holds."

    Server o-- "0..1" Source : single-volume mode
    Server o-- "0..1" Backend : Root (hub mode)
    Server o-- ProjectDB
    Server o-- AuthProvider
    Server o-- Directory
    Server o-- DeviceRegistry
    Server o-- ShareDB
    Server o-- ReadLedger
    Server o-- QuotaProvider
    Server *-- reservations : holds before it charges
    reservations *-- grant
    reservations ..> QuotaProvider : CheckWrite(size + outstanding), RecordUsage on landing
    ShareDB ..> QuotaProvider : CheckRead before the stream, RecordEgress after
    Server ..> secretScan : handleShareCreate scans the first 1 MiB unless confirmed or alreadyPublic
    Server ..> renderFindings : handleRender + renderVersion, every markdown view
    renderFindings ..> secretScan
    secretScan ..> secretFinding
    Server ..> countingWriter : every bytes-out route that bills
    Server ..> wireCodec : gzip on /store/ GET+list, inflate above spool on PUT
    Backend ..> wireCodec : httpBackend gzips a relayed PUT when sign() allows
    reservations ..> Backend : reconcile — did the blob land
    Server *-- journalDoor : /store/* is the only way a device writes
    journalDoor ..> DeviceRegistry : OwnerOf gates the journal key
    Server *-- sandboxInline : every bytes-out route
    Server *-- offboard : account deletion
    offboard ..> OrgDB : orgEvictor
    offboard ..> ProjectDB : dropPerm
    offboard ..> DeviceRegistry : Release
    OrgDB ..> BuiltinAuth : seniorityLister, for the heir
    BuiltinAuth ..> DeviceRegistry : Bind at token issuance
    Server *-- AnalyticsConfig
    Server *-- productAnalytics : every write door emits files_changed
    productAnalytics ..> AnalyticsConfig : Key gates it, Endpoint() addresses it
    journalDoor ..> productAnalytics : storedMax tells this cycle's ops from the whole history
    Server *-- volume : per project, cached
    volume o-- Source

    Source <|.. DirSource
    Source <|.. RemoteSource
    MoveSource <|.. RemoteSource : optional, like Uploader — DirSource has no journals, so no moves
    MoveSource ..> moveIndex
    volume o-- moveIndex : cached with the snapshot
    moveIndex *-- pathEvent
    moveIndex ..> segment : chainSegments
    Uploader <|-- DirectUploader
    DirectUploader <|.. RemoteSource
    RemoteSource o-- Backend : Prefixed(Root, projectID)
    Backend <|-- PutSigner : optional capability

    AuthProvider <|.. BuiltinAuth
    AccountApprover <|.. BuiltinAuth
    BuiltinAuth *-- CLIAuth : serves bdrive login
    BuiltinAuth o-- Mailer : nil → log links
    AuthProvider ..> User

    Directory <|.. LocalDirectory
    LocalDirectory *-- OrgDB : embeds
    OrgDB ..> Org
    OrgDB ..> OrgInvite
    BuiltinAuth ..> OrgDB : InviteValid wiring

    ProjectDB ..> Project
    Server *-- seedTemplate : on create, when `template` is set
    seedTemplate ..> Uploader : RemoteSource.Upload (blob, then journal)
    seedTemplate ..> ProjectDB : SetTemplate records it once
    Server *-- projectPerm : gates every per-project route
    projectPerm ..> Project : Perms + Default
    projectPerm ..> Directory : org role
    ShareDB ..> Share
    ShareDB ..> mermaidTag : markdown shares only
    ShareDB ..> markdownRender : RenderMarkdown (table stays)
    Server ..> markdownRender : RenderMarkdownPairs (viewer)
    markdownRender ..> FrontmatterPair
    DeviceRegistry ..> DeviceInfo
    DeviceRegistry *-- devKey : (account, id)
    RemoteSource ..> sourcedOp : attribution comes from the journal key
    undoRunDoor ..> sourcedOp : selects a run by the journal it was read from
    undoRunDoor ..> RemoteSource : appendOps — the whole run in one Put
    RemoteSource *-- cachedJournal : parsed ops, keyed on size+mtime
    ReadLedger ..> ReadStat
    ReadLedger ..> SessionRead
    ReadLedger ..> HeatEntry
    ReadLedger ..> ShareOpen
    ShareDB ..> ShareOpen : shares list joins the open count per path
    QuotaProvider <|.. UnlimitedQuota
```

## Metadata persistence (`MetaStore`)

Service structs keep in-memory maps + logic; every change persists as one
record through a typed repo. Blobs and journals never touch this layer.

Two things changed shape here. Every service now `refresh()`es — re-reads its
repo before any decision — because a hub is not always one process: with two
replicas over one Postgres, a revocation or a removal only took effect on the
replica that served it, and the other kept honoring the old map until restart.
And a whole-record `Put` is no longer the only write: the row-scoped
interfaces below let a permission or a membership persist as ONE row, so two
concurrent grants stop overwriting each other's map.

```mermaid
classDiagram
    direction LR

    class MetaStore {
        <<interface>>
        +Accounts() AccountRepo
        +Projects() ProjectRepo
        +Orgs() OrgRepo
        +Shares() ShareRepo
        +Devices() DeviceRepo
        +Reads() ReadRepo
        +SessionReads() SessionReadRepo
        +Close()
    }

    class AccountRepo {
        <<interface>>
        +Load() +PutAccount +DeleteAccount +PutToken +DeleteToken +PutPolicy
    }
    class ProjectRepo {
        <<interface>>
        +Load() +Put +Delete
    }
    class OrgRepo {
        <<interface>>
        +Load() +PutOrg +DeleteOrg +PutInvite +DeleteInvite
    }
    class ShareRepo {
        <<interface>>
        +Load() +Put +Delete
    }
    class DeviceRepo {
        <<interface>>
        +Load() +Put +Delete(user, id)
    }

    class Versioned {
        <<optional interface>>
        +Version() (string, error)
    }
    note for Versioned "Every registry re-reads its whole store before every authorization decision — a correctness floor, not a cache. This is that read made cheap: one os.Stat (file) or one lookup on a per-registry meta_version counter bumped inside every write transaction (SQL). A repo that cannot answer, or errors, counts as CHANGED, so the fallback is the unconditional re-read. Not a TTL: a moved token is always followed by the full re-read"

    class versionGate {
        <<per registry>>
        -token, valid
        +stale(repo) token, bool
        +fresh(token)
    }

    class rowScopedProjectRepo {
        <<optional interface>>
        +PutMeta(p)
        +PutPerm(project, email, level)
    }
    class rowScopedOrgRepo {
        <<optional interface>>
        +PutOrgMeta(o)
        +PutMember(org, email, role, joined)
    }
    note for rowScopedProjectRepo "Type-asserted, not part of ProjectRepo/OrgRepo, so a third-party MetaStore stays valid — the whole-record Put is still the fallback. All four in-tree repos implement them. An empty level or role means delete the row. ProjectDB.put / putPerm and OrgDB.putOrg / putMember write through them and roll the in-memory map back when the write fails, so the map and the store can no longer disagree"

    class storable {
        <<db.go — one gate>>
        storable / storableMap
        checkAccount checkToken checkProject
        checkOrg checkInvite checkShare
        checkDevice checkReadStat checkSessionRead
    }
    note for storable "Called at the top of every repo write in BOTH backends. A NUL byte or invalid UTF-8 in a name is accepted by JSON and rejected by Postgres, so the file backend used to persist rows the SQL backend would refuse — the same hub, migrated, would silently lose them. Refusing at one gate makes the two backends agree on what is storable"
    class ReadRepo {
        <<interface>>
        +Load() +PutBatch +DeleteBatch
    }
    note for ReadRepo "batch-oriented: one flush = one write"
    class SessionReadRepo {
        <<interface>>
        +PutBatch +ListBySession +PruneBefore
    }
    note for SessionReadRepo "read_sessions / sessions.json — never Load()ed whole; queried by (project, session, device) and pruned by date, which is what keeps the boot load and Heat's scan the size they are today"

    class fileMetaStore {
        JSON files, atomic rewrite per change
    }
    class sqlMetaStore {
        one database/sql impl
        sqlite (modernc) or postgres (pgx)
        +schema_meta version row
        +addColumns(cols, guarded) ALTER
        +device_rows keyed (user, id)
    }
    note for sqlMetaStore "ProjectRepo.Put is transactional over projects + project_perms (same shape as orgs + org_members), and is now the FALLBACK path — a single grant goes through PutPerm."
    note for sqlMetaStore "addColumns takes a second, GUARDED set. Probing the live columns and re-adding a missing one is how a running hub gains a field on restart, but on a security column (projects.default_level) it is also how a downgrade silently re-creates it EMPTY — every project back to its default visibility. A guarded column missing from a non-empty table past schema version 0 is now a hard startup error. device_rows is the new (account, device) primary key, copied once from the old id-keyed devices table, which is left in place"

    MetaStore <|.. fileMetaStore
    MetaStore <|.. sqlMetaStore
    MetaStore *-- AccountRepo
    MetaStore *-- ProjectRepo
    MetaStore *-- OrgRepo
    MetaStore *-- ShareRepo
    MetaStore *-- DeviceRepo
    MetaStore *-- ReadRepo
    MetaStore *-- SessionReadRepo

    class BuiltinAuth
    class ProjectDB
    class OrgDB
    class ShareDB
    class DeviceRegistry
    class ReadLedger

    BuiltinAuth o-- AccountRepo
    ProjectDB o-- ProjectRepo
    OrgDB o-- OrgRepo
    ShareDB o-- ShareRepo
    DeviceRegistry o-- DeviceRepo
    ReadLedger o-- ReadRepo
    ReadLedger o-- SessionReadRepo

    BuiltinAuth *-- versionGate
    ProjectDB *-- versionGate
    OrgDB *-- versionGate
    ShareDB *-- versionGate
    DeviceRegistry *-- versionGate
    versionGate ..> Versioned : asks before every reload
    fileMetaStore ..> Versioned : mtime + size
    sqlMetaStore ..> Versioned : meta_version row per registry
    fileMetaStore ..> rowScopedProjectRepo : its project and org repos also implement
    sqlMetaStore ..> rowScopedProjectRepo : its project and org repos also implement
    fileMetaStore ..> rowScopedOrgRepo
    sqlMetaStore ..> rowScopedOrgRepo
    ProjectDB ..> rowScopedProjectRepo : one perm, one row
    OrgDB ..> rowScopedOrgRepo : one member, one row
    fileMetaStore ..> storable : before every write
    sqlMetaStore ..> storable : before every write
```
