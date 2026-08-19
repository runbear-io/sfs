// Hand-written TypeScript shapes for the Go API responses. Derived from the
// handler structs in internal/webapp — keep in sync when handlers change.
// The API is deliberately storage-blind: nothing here ever names a bucket,
// remote URL, or credential, and heat responses carry no actor identities.

// GET /api/config (handleConfig, server.go)
export interface ServerConfig {
  mode: "volume" | "hub";
  volume: string;
  brand: string;
  upload: { enabled: boolean };
  auth: {
    enabled: boolean;
    cli_login?: string;
    allow_signup?: boolean;
    admin?: boolean;
  };
  reads: { enabled: boolean };
  // The BearDrive Desktop sidecar (`bdrive desktop`): a loopback server over
  // this machine's own mounts. Projects report perm "read" (local state is
  // never written through the viewer), but hub-backed surfaces — heat,
  // share links — are proxied to the hub, which enforces the real
  // permission, so desktop-aware gates key off this instead of perm.
  desktop?: boolean;
  // Starting structures a new project can be created from (internal/templates,
  // go:embed'ed into the server). Served rather than hardcoded here so a hub
  // shipping another one needs no frontend change.
  templates?: StartTemplate[];
  me?: { email: string; name: string };
  // Managed deployments only: where billing lives + the user's current plan.
  billing?: { plan: string; url: string };
  // Managed deployments only: PostHog project key + ingestion host. Absent
  // on a self-hosted hub, and absence is what keeps analytics.ts inert.
  analytics?: { key: string; host: string };
}

// GET <config.billing.url> with Accept: application/json (managed hubs).
// The SPA's /billing view renders this; the money actions stay server-side
// (checkout_url/portal_url are plain form-POST targets that leave the SPA).
export interface BillingInfo {
  org: string;
  role: string;
  owner: boolean;
  plan: { id: string; name: string; status?: string };
  usage: { used: string; cap: string; pct: number };
  seats: { used: number; cap: number };
  plans: { id: string; name: string; price: string; blurb: string; current: boolean }[];
  has_customer: boolean;
  checkout_url: string;
  portal_url: string;
}

// One entry of /api/config's `templates` (templates.Template).
export interface StartTemplate {
  name: string; // the API/flag value, e.g. "docs"
  title: string; // menu label, e.g. "Docs + decision records"
  blurb: string; // the one-line shape, e.g. "docs/, decisions/"
}

// Per-project permission levels (perms.go). Ordered: each includes the ones
// before it.
export type PermLevel = "none" | "read" | "write" | "admin";
const PERM_RANK: Record<string, number> = { read: 1, write: 2, admin: 3 };
// Mirrors atLeast() on the server. The UI uses it to hide affordances; the
// server still enforces every one of them.
export function atLeast(have: string | undefined, want: PermLevel): boolean {
  return (PERM_RANK[have || ""] || 0) >= (PERM_RANK[want] || 0);
}

// GET /api/projects (handleProjectList, server.go)
export interface Project {
  id: string;
  name: string;
  org?: string;
  created?: string;
  description?: string;
  /** lucide icon name (kebab-case); unknown or absent → the folder placeholder */
  icon?: string;
  creator?: string;
  /** the starting structure it was created from ("" / absent for an empty project) */
  template?: string;
  // The signed-in account's effective level on this project, resolved
  // server-side. A project you cannot read never appears in the list at all,
  // so this is always read or better here.
  perm?: PermLevel;
}

// GET /api/p/{id}/permissions (handleProjectPerms, perms.go)
export interface ProjectPerms {
  default: PermLevel; // what org members get without a grant
  me: PermLevel; // the caller's own effective level
  creator?: string;
  grants: Array<{ email: string; level: PermLevel }>;
}

export interface ProjectList {
  projects: Project[];
}

// POST /api/projects (handleProjectCreate) — create-or-join by name.
export interface ProjectCreated {
  project: Project;
  created: boolean;
}

// GET /api/orgs (handleOrgList, orgs.go)
export interface OrgMember {
  email: string;
  role: string; // "owner" | "member"
}

export interface Org {
  id: string;
  name: string;
  role: string; // the signed-in account's role in this org
  members: OrgMember[];
  created?: string;
  // Where this org is administered: the hub's own page on a self-hosted
  // install, an external directory's page on a managed one. Follow it; do
  // not branch on it.
  manage_url: string;
}

export interface OrgList {
  orgs: Org[];
}

// POST /api/invites/{token} (handleInviteAccept, orgs.go)
export interface InviteAccepted {
  ok: boolean;
  org: { id: string; name: string };
}

// GET /api/admin/pending (handleAdminPending, admin.go)
export interface PendingList {
  pending: Array<{ id: string; email: string; name: string }>;
}

// GET .../tree (handleTree → Node, server.go)
export interface Node {
  name: string;
  path: string;
  dir?: boolean;
  size?: number;
  time?: string;
  user?: string;
  user_name?: string;
  author?: string;
  device?: string;
  children?: Node[];
}

// GET .../render (handleRender, server.go)
export interface RenderDoc {
  path: string;
  html: string;
  size: number;
  time?: string;
  user?: string;
  user_name?: string;
  author?: string;
  device?: string;
}

// GET .../heat (handleHeat, reads.go) — counts only, never who.
export interface HeatEntry {
  human?: number;
  agent?: number;
  share?: number;
  readers?: number;
  last?: string;
}
export type HeatMap = Record<string, HeatEntry>;

// GET .../history (HistoryEntry, history.go)
export interface DeviceInfo {
  id?: string;
  name?: string;
  os?: string;
}
export interface HistoryEntry {
  time: string;
  kind: string; // add | edit | delete (older servers: raw "put")
  path: string;
  size?: number;
  blob?: string;
  user?: string;
  user_name?: string;
  author?: string;
  device: DeviceInfo;
  note?: string;
  // The agent session this change was committed during (hook-set, and unlike
  // the note not settable by hand). It groups a run card and is the key the
  // card's reads are fetched with.
  session?: string;
}

// POST .../shares (handleShareCreate, shares.go)
export interface ShareCreated {
  token: string;
  url: string;
}

// GET /api/orgs/{org}/invites (handleInviteList, orgs.go)
export interface OrgInviteInfo {
  token: string;
  url: string;
  creator?: string;
  created?: string;
  expires: string;
  uses: number;
}

// One live public link. Both listings return the same shape (shareJSON,
// shares.go): GET /api/p/{project}/shares for one project, and
// GET /api/orgs/{org}/shares for the org-wide audit, which alone adds
// project_name.
export interface ShareInfo {
  token: string;
  url: string;
  path: string;
  project: string;
  project_name?: string;
  creator?: string;
  created?: string;
  expires?: string;
  /* Share-link receipts (shareJSON, shares.go). Both keys are ABSENT — not
     zero — when the hub has read telemetry off, which is why opens must be
     tested with `=== undefined` and never with a falsy check: 0 is a real,
     meaningful value ("minted, never opened"). Counted per FILE, not per
     link, because heat is keyed by path. */
  opens?: number;
  last_opened?: string;
}

// GET/POST /api/admin/policy (handleAdminPolicy, admin.go)
export interface AdminPolicy {
  require_verification: boolean;
  require_approval: boolean;
  allow_signup: boolean;
  allowed_domains?: string[]; // read-only (server config)
  admins?: string[]; // read-only (server config)
  mailer: boolean;
}

// POST .../upload/init (handleUploadInit, upload.go)
export interface UploadPlan {
  mode: "direct" | "server";
  exists?: boolean;
  url?: string;
  method?: string;
  headers?: Record<string, string>;
}
