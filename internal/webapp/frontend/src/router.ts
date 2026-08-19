// Native path routing (no hash, no %2F):
//   volume mode:  /<path>
//   hub mode:     /<project-id>/<path>
//   invite:       /join/<token>
//   org admin:    /orgs/<org-id>
// Each path segment is percent-encoded for odd characters, but the "/"
// separators stay literal so the URL reads like a real file path. This is
// why routes are parsed by hand instead of with a route-matching library:
// encoded slashes must survive.

export function encodePath(p: string): string {
  return p.split("/").map(encodeURIComponent).join("/");
}
// A segment a browser will hand us but decodeURIComponent refuses. "%80" is a
// syntactically valid escape (Go's URL parser accepts it and the hub serves the
// SPA shell for it) that is not valid UTF-8, so decodeURIComponent throws
// URIError. parseRoute runs in HubApp's useMemo DURING RENDER, so the throw
// unmounted the whole app — and the address bar kept the URL, so a reload
// reproduced it. Delivery is a plain [x](/<pid>/%80) in a teammate's markdown.
//
// An undecodable segment is just a path that names no file. Keep the raw bytes
// and let the file lookup 404 in the app that is still on screen.
function decodeSegment(s: string): string {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}
export function decodePath(p: string): string {
  return p.split("/").map(decodeSegment).join("/");
}

// Special views are RESTful routes under the project — the first segment
// after the project id is reserved when it names a view:
//   /<project-id>/dashboard[/<path>]  the read×staleness dashboard (optionally scoped)
//   /<project-id>/history[/<path>]    change feed (project / subtree / file)
//   /<project-id>/install             connect-a-device guide
//   /<project-id>/settings            project settings
// Rule: every page gets its own URL path (see CLAUDE.md) — new surfaces are
// view routes here, not ephemeral panel state. (Root-level files literally
// named like a view lose the URL shortcut and remain reachable via the tree.)
export const VIEW_ROUTES = new Set(["dashboard", "history", "install", "settings"]);

// Shipped URLs that were renamed. Parsed into the new view and normalized
// away on arrival, so bookmarks resolve without a second live name.
const LEGACY_VIEWS: Record<string, ViewName> = { insights: "dashboard" };

// `head` is a file or folder name any member can create, and indexing a plain
// object with it reaches Object.prototype: LEGACY_VIEWS["constructor"] is the
// Object constructor — truthy — so a folder named "constructor" was parsed as a
// renamed view, `view` became a FUNCTION, and the legacy-view redirect rewrote
// the address bar to "/<pid>/function Object() { [native code] }". The folder
// was then unreachable by URL for the whole org, permanently. Same shape round
// 11 fixed in ProjectIcon; the router kept it.
function legacyView(head: string): ViewName | undefined {
  return Object.hasOwn(LEGACY_VIEWS, head) ? LEGACY_VIEWS[head] : undefined;
}

export type ViewName = "dashboard" | "history" | "install" | "settings";

// How a reader has narrowed the history feed. Server-side filters, so they
// ride to the API verbatim — and they live in the URL rather than component
// state so a filtered feed is linkable, survives reload, and comes back on
// Back like any other page (see CLAUDE.md: every surface owns a URL).
export interface HistoryFilters {
  q?: string; // substring of the path
  user?: string; // exact account
  since?: string; // YYYY-MM-DD (UTC), inclusive
  until?: string; // YYYY-MM-DD (UTC), inclusive
}
export const HISTORY_FILTER_KEYS = ["q", "user", "since", "until"] as const;

export function hasHistoryFilters(f?: HistoryFilters): boolean {
  return !!f && HISTORY_FILTER_KEYS.some((k) => !!f[k]);
}

// The query string for a filter set — "" when nothing is set, so an
// unfiltered view keeps the bare URL it has always had.
export function historyFilterQuery(f?: HistoryFilters): string {
  const p = new URLSearchParams();
  for (const k of HISTORY_FILTER_KEYS) if (f?.[k]) p.set(k, f[k]!);
  const s = p.toString();
  return s ? "?" + s : "";
}

export interface Route {
  // Org administration is not project-scoped, so it is a top-level route
  // rather than a view under a project. The server hands out this URL (see
  // manage_url on /api/orgs), which is why it is reserved here.
  org?: string;
  // Billing (managed hubs) is hub-level like the org route; the URL comes
  // from /api/config's billing block. Reserved only in hub mode — project
  // ids are UUIDs (or legacy p-…), so the segment can't collide with one.
  billing?: boolean;
  project?: string;
  path: string;
  view?: ViewName;
  viewTarget?: string;
  // The URL used a renamed segment (e.g. /insights): the app replaces it
  // with the canonical one instead of leaving two URLs for one page.
  legacyView?: boolean;
  // The URL carried a trailing separator (/notes/ — what a browser hands you
  // when you copy a folder URL). Same treatment as legacyView: it resolves,
  // then the app replaces it with the slash-free URL.
  trailingSlash?: boolean;
  // A past version of `path`, by content hash (?v=<sha>). Not a view route:
  // the first segment after the project id is reserved for view names, and a
  // version is the same page pinned to older bytes, so it rides as a query
  // param on the file route.
  version?: string;
  // How the creator said they'd fill this project ("existing" = they already
  // have a folder). Rides as a query param rather than living on the project
  // record on purpose: it is one person's intent for their next five minutes,
  // not a property of the project — a teammate connecting next week has their
  // own answer, and would be told the wrong thing by a persisted flag.
  connect?: string;
  // History feed filters (?q=&user=&since=&until=). Only ever set on the
  // history view; absent when nothing is filtered.
  filters?: HistoryFilters;
  // The history target arrived as ?path=/?prefix= rather than as a path
  // segment. Same treatment as legacyView: it resolves, then the app
  // replaces it with the canonical /history/<target> URL.
  queryTarget?: boolean;
}

// `url` is pathname + search (what useLocationPath hands back).
export function parseRoute(url: string, mode: "volume" | "hub"): Route {
  const qi = url.indexOf("?");
  const q = qi === -1 ? null : new URLSearchParams(url.slice(qi));
  const version = q?.get("v") || "";
  const connect = q?.get("connect") || "";
  const r = parsePath(qi === -1 ? url : url.slice(0, qi), mode);
  if (version) r.version = version;
  if (connect) r.connect = connect;
  const filters: HistoryFilters = {};
  for (const k of HISTORY_FILTER_KEYS) {
    const v = q?.get(k);
    if (v) filters[k] = v;
  }
  if (hasHistoryFilters(filters)) r.filters = filters;
  // The History API takes ?path=/?prefix= (see CLAUDE.md), so a reader who
  // has seen the API types the query form at the page too. Honour it and
  // normalize, rather than dropping it and rendering the whole project as if
  // the file had no history of its own. The two are aliases, not modes: the
  // view decides file-vs-subtree from the tree, exactly as it does for
  // /history/<target>. A path segment already present wins, which is also
  // what leaves ?path= on every other view untouched.
  if (r.view === "history" && !r.viewTarget) {
    const t = (q?.get("path") || q?.get("prefix") || "").replace(/^\/+|\/+$/g, "");
    if (t) {
      r.viewTarget = decodePath(t);
      r.queryTarget = true;
    }
  }
  return r;
}

// Trailing separators are stripped off the raw (still-encoded) slice so a
// percent-encoded slash inside a segment survives, and flagged only when
// stripping actually changed the string — a bare "/p-1/" has nothing to
// strip, so it never asks for a redirect.
function withPath(r: Route, raw: string): Route {
  const p = raw.replace(/\/+$/, "");
  if (p !== raw) r.trailingSlash = true;
  r.path = p ? decodePath(p) : "";
  return r;
}

function parsePath(pathname: string, mode: "volume" | "hub"): Route {
  const raw = pathname.replace(/^\/+/, "");
  if (mode !== "hub") return withPath({ path: "" }, raw);
  if (raw === "orgs" || raw.startsWith("orgs/")) {
    return { org: raw.slice(5).replace(/\/+$/, ""), path: "" };
  }
  if (raw === "billing" || raw.startsWith("billing/")) {
    return { billing: true, path: "" };
  }
  const slash = raw.indexOf("/");
  if (slash === -1) return { project: raw, path: "" };
  const r = withPath({ project: raw.slice(0, slash), path: "" }, raw.slice(slash + 1));
  const seg = r.path.indexOf("/");
  const head = seg === -1 ? r.path : r.path.slice(0, seg);
  const legacy = legacyView(head);
  if (VIEW_ROUTES.has(head) || legacy) {
    r.view = legacy || (head as ViewName);
    if (legacy) r.legacyView = true;
    r.viewTarget = seg === -1 ? "" : r.path.slice(seg + 1).replace(/\/+$/, "");
    r.path = "";
  }
  return r;
}

// The URL for a file within a project (hub) or the volume (no project id),
// optionally pinned to one past version by content hash.
export function urlForPath(path: string, projectId?: string, version?: string): string {
  const enc = encodePath(path);
  const q = version ? "?v=" + version : "";
  if (projectId) return "/" + projectId + (enc ? "/" + enc : "") + q;
  return "/" + enc + q;
}

// The URL for a special view of a project, optionally carrying the history
// filter set (ignored by every other view).
export function urlForView(
  view: ViewName,
  projectId?: string,
  target?: string,
  filters?: HistoryFilters,
): string {
  let s = (projectId ? "/" + projectId : "") + "/" + view;
  if (target) s += "/" + encodePath(target.replace(/\/+$/, ""));
  return s + (view === "history" ? historyFilterQuery(filters) : "");
}

// A first segment that names no project id is, nine times out of ten, the
// project NAME: that is what the sidebar shows, and the id never appears in
// the UI as something to copy. Resolve it only when it is unambiguous —
// ProjectDB's names are scoped per organization (create-or-join-by-name), so
// a viewer who belongs to two orgs can hold two projects called "wiki", and
// guessing between them is worse than the not-found page.
//
// The segment arrives still-encoded (parsePath slices `raw` before decodePath
// runs), so it is decoded here — without that, any name with a space or a
// non-ASCII character silently fails to match, which is exactly the set of
// names most likely to be typed by hand in the first place.
//
// No id-shape check is needed: this only ever runs on a segment that already
// failed to match every id, and it compares against real names, so an
// id-shaped segment can only resolve if some project is literally called
// that. A UUID regex here would be a second rule to keep in sync with
// parsePath for zero change in behaviour.
export function projectByName(
  projects: { id: string; name: string }[],
  seg: string,
): string | undefined {
  const want = decodePath(seg).toLowerCase();
  const hit = projects.filter((p) => p.name.toLowerCase() === want);
  return hit.length === 1 ? hit[0].id : undefined;
}
