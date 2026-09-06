// Fetch helpers shared by every API hook. All URLs are root-absolute: a
// deep path like /<project>/<dir>/<file> must never break relative
// resolution.

import { track } from "../analytics";

// Product events, derived from the write that just succeeded rather than
// hand-fired at each button. Every mutating call in the app goes through
// api() or postJSON(), so this table is the whole instrumentation surface —
// a new write shows up here or nowhere, instead of silently going unmeasured
// because someone forgot a capture() call. The one write that bypasses these
// helpers (share creation, a raw fetch in Browser.tsx) fires its own.
//
// Nothing here carries a path, project name, email or token: the event says
// what kind of thing happened, never to which customer object.
//
// Two writes are deliberately absent. POST .../reads is read telemetry, not a
// user action — counting it would mean the analytics of our analytics. The
// /store/* routes are device replication, which is a sync cycle rather than
// anything a person did.
const PRODUCT_EVENTS: [RegExp, string][] = [
  [/^POST \/api\/projects$/, "project_created"],
  [/^DELETE \/api\/projects\//, "project_deleted"],
  [/^POST \/api\/p\/[^/]+\/restore$/, "file_restored"],
  [/^DELETE \/api\/shares\//, "share_revoked"],
  [/^PATCH \/api\/shares\//, "share_expiry_changed"],
  [/^POST \/api\/orgs\/[^/]+\/invites$/, "invite_created"],
  [/^DELETE \/api\/orgs\/[^/]+\/invites\//, "invite_revoked"],
  [/^POST \/api\/invites\//, "invite_accepted"],
  [/^PUT \/api\/p\/[^/]+\/permissions\/./, "project_access_granted"],
  [/^DELETE \/api\/p\/[^/]+\/permissions\/./, "project_access_revoked"],
];

function trackWrite(method: string, url: string) {
  // The query string can hold a path; match on the path alone.
  const key = method + " " + url.split("?")[0];
  const hit = PRODUCT_EVENTS.find(([re]) => re.test(key));
  if (hit) track(hit[1]);
}

function toLogin(): never {
  // Auth required: sign in, then come back to the current route.
  location.href =
    "/auth/login?next=" +
    encodeURIComponent(location.pathname + location.search);
  throw new Error("signing in…");
}

// Server messages are written for operators and CLIs — lowercase, unpunctuated,
// occasionally naming internals ("forbidden: seat limit reached for plan free").
// They surface verbatim in toasts, so the ones we can predict get product copy
// and everything else falls back to the server's own words, which is still
// better than a generic apology when the cause is specific.
function errorFor(status: number, body: string): string {
  const raw = body.trim();
  switch (status) {
    case 403:
      if (raw.includes("seat"))
        return "This plan is out of seats. Upgrade to add more people.";
      if (raw.includes("owner")) return "Only owners can do that.";
      return "You don't have access to that.";
    case 409:
      // The 409 body carries the URL that owns the thing, which is the whole
      // point of the message — keep it, but capitalize like the rest.
      return raw
        ? raw[0].toUpperCase() + raw.slice(1)
        : "That is managed outside this hub.";
    case 404:
      return "That is gone — it may have been removed already.";
    case 413:
      return "This project is over its plan limit.";
    case 429:
      return "Too many requests. Give it a moment.";
    default:
      if (status >= 500) return "The server had a problem. Try again.";
      return raw
        ? raw[0].toUpperCase() + raw.slice(1)
        : "Something went wrong.";
  }
}

async function fail(r: Response): Promise<never> {
  throw new Error(errorFor(r.status, await r.text()));
}

export async function getJSON<T>(url: string): Promise<T> {
  // Explicit Accept so endpoints that content-negotiate (e.g. /billing:
  // JSON data vs the app shell) know this is a data fetch.
  const r = await fetch(url, { headers: { Accept: "application/json" } });
  if (r.status === 401) toLogin();
  if (!r.ok) await fail(r);
  return r.json();
}

// Same auth/error handling as getJSON, but hands back the raw Response —
// for endpoints that serve bytes rather than JSON (blob?sha=).
export async function getResponse(url: string): Promise<Response> {
  const r = await fetch(url);
  if (r.status === 401) toLogin();
  if (!r.ok) await fail(r);
  return r;
}

/* fetch wrapper for methods without a body-returning helper */
export async function api<T = unknown>(
  method: string,
  url: string,
  body?: unknown,
): Promise<T> {
  const opt: RequestInit = { method };
  if (body !== undefined) {
    opt.headers = { "Content-Type": "application/json" };
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(url, opt);
  if (!r.ok) await fail(r);
  trackWrite(method, url);
  return r.status === 204 ? ({} as T) : r.json();
}

export async function postJSON<T>(url: string, body?: unknown): Promise<T> {
  const r = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
  if (r.status === 401) toLogin();
  if (!r.ok) await fail(r);
  trackWrite("POST", url);
  return r.json();
}

/* putText writes a file's whole contents. It is the browser editor's save
   (Editor.tsx), and rides the same upload/content route the CLI and the
   desktop app already use — which is why the desktop gets editing for free:
   its sidecar proxies that route to the hub, so the write is journaled by the
   hub under this account exactly like any other. */
export async function putText(url: string, text: string): Promise<void> {
  const r = await fetch(url, {
    method: "PUT",
    headers: { "Content-Type": "text/plain; charset=utf-8" },
    body: text,
  });
  if (r.status === 401) toLogin();
  if (!r.ok) await fail(r);
  trackWrite("PUT", url);
}
