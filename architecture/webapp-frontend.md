# Hub frontend (React SPA) — module diagram

Source of truth: `internal/webapp/frontend/src`. The built output is
committed at `internal/webapp/static` (the `go:embed` target), so `go build`
never needs Node. Reflects the code as of this commit; update this file in
any PR that changes these modules or their relationships.

```mermaid
classDiagram
    direction LR

    class ErrorBoundary {
        getDerivedStateFromError
        renders a page with a way back
    }
    note for ErrorBoundary "ErrorBoundary.tsx — the app's floor, mounted in main.tsx ABOVE QueryClientProvider so it covers every route. React unmounts the whole tree when a render throws and nothing catches it, and the address bar keeps the URL, so a reload reproduces the blank page: a permanent client-side DoS that another member's CONTENT can reach (a link in a teammate's markdown reaching decodePath, a folder named `constructor` reaching ProjectIcon). Deliberately the smallest thing that works — no reporting, no retry machine, no per-route boundaries"

    class shareMermaid {
        <<second Vite entry>>
        src/share-mermaid.ts → static/share-mermaid.js
        picks DARK/LIGHT from prefers-color-scheme
    }
    note for shareMermaid "The only script the server-rendered /s/ share page ever loads, and only when shares.go finds a mermaid fence in the document. Built as a SECOND rollup input with a fixed, unhashed name at the static root — sharedMarkdownShell is a Go const and cannot know a content hash, and server.go marks assets/ immutable for a year, so an unhashed file there would pin a stale bundle in shared caches. Mermaid's own chunks keep their hashed assets/ names, which is why the share page needs the ACAO header: its sandbox origin is opaque"

    class App {
        mode from /api/config
    }
    note for App "App.tsx — picks HubApp (multi-project) or VolumeApp (single volume) from server config; frontend learns everything from the API, never sees storage or credentials"

    class HubApp {
        project list, org walls
        admin panels, invites
        remembers last opened project (localStorage)
    }
    class VolumeApp {
        thin wrapper: one volume
    }
    class Browser {
        folder listing, file view
        per-view routes
        +moved: /resolve?path= on a tree miss only
        +scroll restoration: contentRef, memo, goal from lib/scroll
    }
    note for Browser "A missing path is decided from /tree alone — the file is never fetched — so the X-Bdrive-Canonical-Path header /file answers with would never reach the browser, and a moved FOLDER has no content fetch to hang a header on. The not-found branch asks GET /resolve?path= instead, then replaceState-navigates to the destination and prints one Moved from … line above it (BEA-81)"

    class router {
        +VIEW_ROUTES dashboard history install settings
        +LEGACY_VIEWS insights to dashboard
        +top-level routes orgs billing
        +parseRoute(url, mode) Route
        +Route.version ?v= sha, one past version
        +Route.trailingSlash notes/ resolves, then replaces to notes
        +Route.queryTarget history ?path= / ?prefix= resolves, then replaces to /history/target
        +Route.filters q user since until, history feed
        +historyFilterQuery(filters) / hasHistoryFilters
        +urlForPath(path, projectId, version)
        +urlForView(view, projectId, target, filters) / encodePath / decodePath
        +projectByName(projects, seg) id, only when exactly one name matches
    }
    class nav {
        +navigate(url)
        +useLocationPath() pathname + search
        +linkProps(href)
        +Redirect
    }
    note for router "Two lookups on peer-authored path segments are now prototype-safe and one is throw-safe: legacyView() goes through Object.hasOwn, because LEGACY_VIEWS['constructor'] is truthy and turned a folder of that name into a view whose name was a FUNCTION; decodePath falls back to the raw segment instead of letting decodeURIComponent throw URIError out of a useMemo during render. Same shape as ProjectIcon's PROJECT_ICONS lookup in shell.tsx"
    note for router "projectByName is what makes /wiki reach the project called wiki: the id never appears in the UI as something to copy, so a hand-typed first segment is the NAME the sidebar shows. It decodes the segment (route.project is the still-encoded slice) and returns an id only on EXACTLY one case-insensitive match — ProjectDB names are scoped per organization, so a viewer in two orgs can hold two projects named wiki and guessing between them is worse than the not-found page (BEA-140)"
    note for nav "nav.ts + router.ts — deliberately NOT a router library (react-router v7 startTransition left stale views); History-API path routing, slashes literal, every user-facing page owns a URL path. A version is not a view route (the first segment after the project id is reserved for view names) — it rides as ?v=, so useLocationPath must snapshot the search too or the URL changes and nothing re-renders"

    class api {
        +getJSON / postJSON / api
        +getResponse (raw bytes)
        +PRODUCT_EVENTS method+path → event
        types.ts server contracts
    }
    note for api "api/http.ts — all URLs root-absolute so deep paths never break relative resolution. Every mutating call goes through api()/postJSON(), so one table there is the whole product-event surface: a new write is measured or it isn't, instead of depending on someone remembering a capture() call"

    class analytics {
        +initAnalytics(config)
        +track(event, props)
    }
    note for analytics "analytics.ts — posthog-js is fetched from the CDN at runtime, never installed: with no `analytics` in /api/config this module makes no request and the OSS bundle carries no tracker. capture_pageview history_change because the router is History-API. Replay masks every text node (maskTextSelector *) — in this product nearly all of it is customer file names and document bodies"

    class hooks {
        +useConfig
        +useHub
        +useBrowse
        +useTextAt (any URL) → useBlobText (sha-keyed, immutable)
    }
    note for hooks "TanStack Query wrappers over the viewer APIs. useTextAt fetches any URL and sniffs it — the Content-Length cheap-out lives here (HTTP), the byte decision in lib/sniff.ts (pure). A live path must not be cached immutable; a sha can be"

    class components {
        FileView FolderListing FileTree
        HistoryView HistoryRow HistoryFilters DiffView VersionBanner ConflictBanner
        Insights ShareDialog NewProjectDialog
        ShareBanner SharesTable AdminTable
        OrgAdmin HubSettings ProjectSettings
        Palette shell AccountBar ...
    }
    note for components "NewProjectDialog replaced ProjectNav's name-only modalPrompt: name + starting point, POSTing {name, template}. Its options come from useConfig()'s `templates`, never a hardcoded list, so a hub shipping another template needs no frontend change; the initial selection is options[0].value — the same array element the RECOMMENDED badge indexes, so the badged row and the checked row are one row by construction (on a template-less hub that row is 'I already have a folder', which still creates an empty project). modal.tsx keeps its one-field API — teaching it about choices would tax every other caller"
    note for components "HistoryFilters drives the SERVER (?q=/?user=/?since=/?until= on the history API), never the loaded page — filtering what is on screen would lie about everything below the fold and break next_cursor. Its state is Route.filters, so a narrowed feed is linkable, survives reload, and Back undoes it; the author list accumulates across fetches, because filtering by one author leaves only their rows loaded"
    note for components "FileView's transformHTML resolves the server's `wiki:` marker against flatFiles into a real urlForPath() href (unresolvable ones lose the href and get .wiki-missing), so copy-link/middle-click/new-tab work and only a plain click reaches the delegated handler — resolution used to happen at click time, which left a dead `wiki:guide` string in the DOM (BEA-136). It also drops `data:image/svg` from any rendered img and any `data:` href from any rendered link — goldmark admits them, and an inline SVG is a document rather than a picture (the same property the server's sandboxInline walls off). Insights builds its per-device folder bag with Object.create(null), since folder names come off a peer's journal and one named __proto__ silently emptied the matrix. style.css sets unicode-bidi isolate-override on the peer-authored strings a reader is expected to CHECK (listing rows, breadcrumb, history path/note/device) — journal.SafeText refuses the bidi CONTROLS, but a single strong-RTL LETTER is legal and still reorders a row"
    note for components "HistoryView's RunGroup header carries the run-wide undo (POST undo-run, gated by the same write permission as the per-row restore/remove). It asks the SERVER for the file list first (preview: true) rather than deriving it from the loaded feed — that window is paged and filterable, so a client-computed list is wrong exactly when the run is old. modal.tsx's Confirm.message widened from string to ReactNode for it (the prompt's one-field API is untouched), so the dialog can show every path, its action, and the &quot;changed after this run&quot; warning inline"
    note for components "FileView's MarkdownView renders SecretBadge above the content when the render response carries findings — VersionBanner's shape (a strip, role=status, no actions), the red family rather than the accent because accent+glow already means 'you are looking at an old version' and the two strips stack on the ?sha= view. It phrases from lib/secrets so the badge and the share dialog name the rule and the line identically"
    note for components "components/ui — shadcn/ui primitives (Radix, copied in), themed from BearDrive tokens in tw.css; rendered markdown is transformed as a string before mounting, link clicks delegated on the container — never patch the dangerouslySetInnerHTML subtree"

    class lib {
        +diff.ts splitLines lcsDiff diffText
        +runs.ts groupRuns runFileCount
        +heat.ts heatFor heatTotal heatText heatLevel hotPathSplit
        +heat.ts HEAT_DISCLOSURE (what the count includes, said once)
        +heat.ts ageRange isFlatRange ageSpanLabel (treemap scale)
        +heat.ts orphanPaths (reads whose file left the tree)
        +heat.ts placeLabels LABEL_MAX (scatter danger-dot labels)
        +heat.ts HOT_READS STALE_DAYS isDanger daysSince agoLabel staleNote
        +conflict.ts parseConflict Conflict
        +sniff.ts sniffBytes BlobText MAX_BYTES
        +scroll.ts Goal armGoal applyGoal noteScroll MAX_APPLY
        +csv.ts parseDelimited Csv CSV_ROWS
        +secrets.ts SecretFinding secretsMessage secretsBadge
        +mermaid.ts hasMermaid renderMermaid Palette DARK LIGHT
        +utils.ts
    }
    note for lib "mermaid.ts is the one exception to 'pure, no React, unit-tested on node': it needs a DOM and a browser-only library, so its coverage is Playwright. html in → html out, so neither caller can be tempted to patch a live subtree. It imports mermaid only when hasMermaid() says a document has a fence — that gate is what keeps a diagram-free page from downloading any of it — and every failure (unparseable fence, render throw, chunk that never loads) returns the untouched &lt;pre&gt;&lt;code&gt; instead of throwing"
    note for lib "pure, no React, unit-tested on node (npm test) — the line diff is ~40 lines, cheaper than auditing a diff package. heat.ts is the one read-count arithmetic: every surface (file header, folder listing, Dashboard bar) totals and splits through it, so they cannot disagree; useBrowse re-exports it. HEAT_DISCLOSURE sits beside that arithmetic for the same reason: a member's own views count toward the number, and four surfaces printing their own copy of that promise is four promises that can drift (BEA-61). The constant is NOT re-exported through useBrowse — surfaces import it straight from lib/heat, and a unit test asserts src/ holds exactly one copy of the sentence. The hot-and-stale VERDICT joined the totals for the same reason (BEA-119): HOT_READS/STALE_DAYS/isDanger were private to Insights.tsx, so the Dashboard was the only screen that could say a doc was hot and unmaintained — the file page and the folder listing showed the ingredients and no verdict. isDanger takes (reads, days) rather than a heat entry because only the Dashboard has a reader lens: it passes its lens-filtered count, the other two pass heatTotal. staleNote returns a STRING (empty when not flagged) so the badge stays pure and survives whichever component owns the meta line"
    note for lib "scroll.ts is the per-route scroll goal, lifted out of Browser.tsx so it can be tested at all — the frontend suite is node --test over pure TS with no jsdom. A route change arms a goal (0 on a fresh navigation, the memoized offset on POP) and views re-apply it up to MAX_APPLY times as content grows after first paint; noteScroll RETIRES it the moment the reader scrolls, which is the choke point that stops any onRendered caller — a metadata refresh, a poll, anything added later — from moving a reader mid-read (BEA-155). Two clauses in that guard are load-bearing and each has its own test: scrollTo fires a scroll event of its own, so movement is measured against the goal and never against zero, and a page shorter than the last one makes the browser CLAMP the carried-over offset to the bottom, which is either the old page arriving or a goal that does not fit yet"
    note for lib "secrets.ts is the six credential rules in words, mirroring internal/secrets' Label map. It lived in Browser.tsx under a comment saying one caller did not justify a file; BEA-147 gave it a second one, and the two callers are the two surfaces that report the SAME finding — the share dialog that refuses to mint, and FileView's badge that only warns. One map is what makes 'wording consistent with the share dialog' mechanical rather than a copy-editing promise"
    note for lib "conflict.ts recognises a conflict copy from its NAME alone — syncer.conflictName is a pure function of the path, so the device and the moment come out of the string with no server route, no journal field and no request. The regex is an ANCHORED suffix and a strictly narrower match of the Go convention (sanitize's character class, clip's 32), and every mismatch — truncated suffix, impossible date — is null rather than a throw, so a stray filename can never break a listing. Two callers: FolderListing marks the row, ConflictBanner explains the file (BEA-128)"
    note for lib "csv.ts parses .csv/.tsv for FileView's table view — ~50 lines against RFC 4180, so no papaparse. It NEVER throws: null means 'not a table' (unterminated quote, no delimiter) and the caller falls back to the plain-text preview, which is why the fallback is a type-level guarantee rather than a try/catch someone can forget"

    ErrorBoundary --> App : wraps the whole tree
    App --> HubApp
    App --> VolumeApp
    HubApp --> Browser
    VolumeApp --> Browser
    HubApp --> router
    Browser --> router
    Browser --> lib : parseConflict
    Browser --> components
    HubApp --> components
    components --> nav : linkProps navigate
    components --> lib : diffText groupRuns hotPathSplit placeLabels staleNote isDanger parseDelimited renderMermaid parseConflict secretsBadge
    Browser --> lib : secretsMessage (the share dialog's half of lib/secrets)
    Browser --> lib : armGoal applyGoal noteScroll
    hooks --> lib : re-exports heat.ts, sniffBytes
    shareMermaid --> lib : renderMermaid
    hooks --> api
    Browser --> hooks
    HubApp --> hooks
    hooks --> analytics : initAnalytics + identify on config
    api --> analytics : track(product event)
    Browser --> analytics : share_created (the one raw fetch)
```
