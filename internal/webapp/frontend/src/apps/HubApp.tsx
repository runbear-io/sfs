import { useEffect, useMemo, useState } from "react";
import { desktopPost, postJSON } from "../api/http";
import type { InviteAccepted, Project, ProjectCreated, ServerConfig } from "../api/types";
import { useFetchProjects, useOrgs, usePending, usePermissions, useProjects, useHubRefresh } from "../hooks/useHub";
import { decodePath, parseRoute, projectByName, urlForPath, urlForView } from "../router";
import { linkProps, navigate, Redirect, useLocationPath } from "../nav";
import { AppShell, Page, Topbar, VaultHeader, closeSidebarOnMobile } from "../components/shell";
import { OrgAdmin } from "../components/OrgAdmin";
import { HubSettings } from "../components/HubSettings";
import { ProjectNav } from "../components/ProjectNav";
import { useQueryClient } from "@tanstack/react-query";
import { AccountBar, SignedOutBar } from "../components/AccountBar";
import { BillingView } from "../components/BillingView";
import { ProjectSettings } from "../components/ProjectSettings";
import { ConnectGuide } from "../components/ConnectGuide";
import { EmptyState } from "../components/EmptyState";
import { Setup, type SetupStep } from "../components/Setup";
import { EXISTING, NewProjectDialog } from "../components/NewProjectDialog";
import { toast } from "../toast";
import { lastProject, rememberProject } from "../util";
import Browser from "./Browser";

export default function HubApp({ config }: { config: ServerConfig }) {
  const loc = useLocationPath(); // pathname + search
  const refresh = useHubRefresh();
  // Org just joined via an invite this page-load: prefer its projects over
  // whatever happens to be first in the list.
  const [joinedOrgId, setJoinedOrgId] = useState<string | null>(null);
  // The hub-admin panel still replaces the content pane without touching the
  // URL (the last of the classic app's URL-less surfaces); any navigation
  // closes it. Org administration is a real route — see /orgs/<id> below.
  const [panel, setPanel] = useState<null | { kind: "hub" }>(null);
  useEffect(() => setPanel(null), [loc]);

  // The desktop app's onboarding owns real URLs (/setup, /setup/connect,
  // /setup/syncing, /setup/done) — same project-less pattern as /join below,
  // since router.ts is project-scoped. Hub mode never renders it.
  const setupStep = useMemo<SetupStep | null>(() => {
    if (!config.desktop) return null;
    const m = loc.split("?")[0].match(/^\/setup(?:\/(connect|syncing|done))?\/?$/);
    return m ? ((m[1] as SetupStep) ?? "welcome") : null;
  }, [loc, config.desktop]);

  const joinToken = useMemo(() => {
    const m = loc.split("?")[0].match(/^\/join\/([0-9a-f]+)\/?$/);
    return m ? m[1] : null;
  }, [loc]);

  // "/join/<token>?p=<project-id>": the invite says which project it was sent
  // about, so the joiner lands on that project's install page instead of a
  // nameless list. Only a hint — it is never trusted, see onDone below.
  const joinProject = useMemo(
    () => new URLSearchParams(loc.split("?")[1] || "").get("p") || "",
    [loc],
  );
  const fetchProjects = useFetchProjects();

  const { data: projects } = useProjects(!joinToken);
  const { data: orgs } = useOrgs(!joinToken);
  const isAdmin = !!config.auth.admin;
  const { data: pending } = usePending(isAdmin);

  const route = useMemo(() => parseRoute(loc, "hub"), [loc]);

  // Creating a project is asked for from two places — the sidebar's + and the
  // empty state's button — so the dialog and its one handler live here rather
  // than in either of them. It never opens on its own: it used to on arrival
  // with zero projects, which covered the empty state's agent paste-prompt
  // and pointer-blocked its own "New project" button.
  const [creating, setCreating] = useState(false);
  // "New project" means different things on the two platforms. On the hub it
  // is a record: name it, pick a structure, done. On desktop a project with
  // no local folder is a dead end — it cannot even appear in the list, which
  // is built from this machine's mounts — so the button runs the onboarding
  // connect flow instead: pick the folder, name the shared one, sync. One
  // way to make a project on the Mac, and it always ends with files moving.
  const newProject = () => (config.desktop ? navigate("/setup/connect") : setCreating(true));
  // A read-only hub refuses creation server-side (403), so never offer it.
  const canCreate = config.upload.enabled;

  const createProject = async (name: string, template: string) => {
    // "I already have a folder" is an empty project with a different next
    // screen: the server never hears the sentinel, and the intent rides in
    // the URL instead of onto the project record — it belongs to whoever is
    // connecting right now, not to the project forever.
    const existing = template === EXISTING;
    try {
      const out = await postJSON<ProjectCreated>("/api/projects", {
        name,
        template: existing ? "" : template,
      });
      setCreating(false);
      await refresh();
      navigate("/" + out.project.id + (existing ? "?connect=existing" : ""));
      toast(`Created “${out.project.name}”.`);
    } catch (e) {
      toast("Could not create the project: " + (e as Error).message, true);
    }
  };

  // Never on desktop: newProject() routes there instead of opening it.
  const newProjectDialog = creating && !config.desktop ? (
    <NewProjectDialog
      templates={config.templates ?? []}
      onCreate={createProject}
      onClose={() => setCreating(false)}
    />
  ) : null;

  // Precedence: the URL wins, then an org joined this page-load, then the
  // project this browser last opened, then whatever sorts first. Looking the
  // remembered id up in `projects` is the whole "deleted / access revoked /
  // different account signed in" story — it simply doesn't match and the
  // fallback takes over, with nothing on screen to say so.
  const current: Project | null = useMemo(() => {
    if (!projects) return null;
    return (
      projects.find((p) => p.id === route.project) ||
      (joinedOrgId && projects.find((p) => p.org === joinedOrgId)) ||
      projects.find((p) => p.id === lastProject()) ||
      projects[0] ||
      null
    );
  }, [projects, route.project, joinedOrgId]);

  // On desktop the sidecar reports every project as "read" — that guards the
  // LOCAL stores, which never take writes. The account's real level lives on
  // the project's hub and arrives through the proxied /permissions. Resolving
  // it here, once, keeps every downstream surface (share, restore, settings)
  // platform-blind on project.perm — no scattered desktop checks.
  const hubPerms = usePermissions(config.desktop ? current?.id : undefined);
  const project: Project | null = useMemo(() => {
    if (!current || !config.desktop) return current;
    return { ...current, perm: hubPerms.data?.me ?? current.perm };
  }, [current, config.desktop, hubPerms.data]);

  useEffect(() => {
    document.title = current
      ? current.name + " — BearDrive"
      : config.brand || "BearDrive";
    // Fires on exactly the events that matter — sidebar, deep link, palette,
    // post-create navigate — so the memory needs no subscription of its own.
    if (current) rememberProject(current.id);
  }, [current, config]);

  if (joinToken) {
    return (
      <JoinInvite
        token={joinToken}
        onDone={async (orgId) => {
          setJoinedOrgId(orgId);
          await refresh();
          // Only an id the SERVER just handed back may be pasted into a URL:
          // p="/evil.com" would build "//evil.com/install", and navigate's
          // pushState throws a SecurityError on a cross-origin target.
          // Resolving against the joiner's own live list is the validator —
          // no regex needed — and it doubles as the "you cannot see that
          // project" answer. Anything unresolvable lands on "/", never on
          // the "Project not found" page below: that is right for a typed
          // URL and wrong as a new teammate's first screen. A fetch is
          // needed because useProjects is disabled while this screen is up.
          const list = joinProject ? await fetchProjects().catch(() => null) : null;
          const ok = !!list?.projects?.some((p) => p.id === joinProject);
          navigate(ok ? "/" + joinProject + "/install" : "/", { replace: true });
        }}
      />
    );
  }

  const brand = config.brand || "BearDrive";
  const org = (current && orgs?.find((o) => o.id === current.org)) || null;

  // Top of the sidebar is the brand; project and account actions live in
  // their own sections below (PropelAuth-style layout).
  // The Beta pill labels the BEARDRIVE wordmark, so it keys on what the
  // lockup actually says rather than on whether `brand` was configured. A hub
  // that set its own brand is labelling somebody else's product, and "Acme
  // Docs Beta" is a claim we have no business making for them — but the
  // managed hub and every default self-host DO reach here with "BearDrive",
  // and a `!config.brand` test would have missed the ones that set it
  // explicitly (CLOUD_BRAND defaults to "BearDrive", so BuiltinAuth hubs send
  // the string rather than an empty field).
  const vault = (
    <VaultHeader
      name={brand}
      onHome={() => navigate("/")}
      search={!!current}
      beta={brand === "BearDrive"}
    />
  );

  // Desktop app: session changes go through the sidecar (which runs the same
  // flows as `bdrive login`/`logout`), then the config query is refetched so
  // this bar tracks the new state without a reload. Sign-in resolves only
  // when the user finishes in the system browser, hence the toast first.
  const qc = useQueryClient();
  const desktopAuth = (path: string, waitMsg?: string) => {
    if (waitMsg) toast(waitMsg);
    void desktopPost(path)
      .catch(() => {})
      .finally(() => qc.invalidateQueries({ queryKey: ["config"] }));
  };

  const accountBar = config.me ? (
    <AccountBar
      me={config.me}
      org={org}
      orgActive={!!route.org}
      billing={config.billing}
      signOut={config.desktop ? () => desktopAuth("/api/desktop/logout") : undefined}
      admin={
        isAdmin
          ? {
              pending: pending?.length || 0,
              onClick: () => {
                setPanel({ kind: "hub" });
                closeSidebarOnMobile();
              },
            }
          : undefined
      }
    />
  ) : config.desktop ? (
    <SignedOutBar onSignIn={() => desktopAuth("/api/desktop/login", "Finish signing in in your browser…")} />
  ) : undefined;

  if (setupStep) {
    return (
      <AppShell vault={vault} topbar={<Topbar />}>
        <Page>
          <Setup
            step={setupStep}
            signedIn={!!config.me}
            onSignIn={() => desktopAuth("/api/desktop/login", "Finish signing in in your browser…")}
          />
        </Page>
      </AppShell>
    );
  }

  if (!projects || !orgs) {
    return (
      <AppShell vault={vault} topbar={<Topbar />}>
        <Page>
          <div className="empty">Loading…</div>
        </Page>
      </AppShell>
    );
  }

  // Nothing synced on this Mac IS onboarding, signed in or not: /setup shows
  // the welcome step or jumps to connect depending on the session. Reload and
  // back/forward work because those are real routes.
  if (config.desktop && projects.length === 0) {
    return <Redirect to="/setup" />;
  }

  if (!current) {
    return (
      <AppShell
        vault={vault}
        projectsNav={<ProjectNav projects={projects} onNew={newProject} />}
        orgBar={accountBar}
        topbar={<Topbar />}
      >
        <Page>
          {/* No desktop sign-in branch here: on desktop, zero projects is
              onboarding and the redirect above already took it to /setup,
              whose Welcome frame owns that button. This block is hub-only in
              practice, and the branch that pretended otherwise was dead. */}
          <EmptyState onNew={newProject} canCreate={canCreate} />
        </Page>
        {newProjectDialog}
      </AppShell>
    );
  }

  const activePanel = panel?.kind === "hub" ? { crumb: "Signup & access", body: <HubSettings /> } : null;

  const routeOrg = route.org ? orgs.find((o) => o.id === route.org) : null;
  // A stale link, a revoked membership, or a typo: say so. Rendering the
  // project view at /orgs/<id> told the user nothing and survived a reload.
  const orgMissing = route.org && !routeOrg;
  const orgPage = orgMissing
    ? {
        crumb: "Organization",
        body: (
          <div className="empty">
            <h3>Organization not found</h3>
            <p>This organization doesn't exist, or you're no longer a member.</p>
            <p>
              <a {...linkProps("/" + current.id)}>Back to {current.name}</a>
            </p>
          </div>
        ),
      }
    : routeOrg
    ? {
        crumb: "Organization",
        body: (
          <OrgAdmin
            org={routeOrg}
            projects={projects}
            myEmail={config.me?.email || ""}
          />
        ),
      }
    : null;

  // Same rule as orgMissing, for the project id: a deep link to an id that is
  // not yours is not a landing. `current` still resolves (the fallback chain
  // above is untouched) — it is where "back" points.
  const projectMissing = !!route.project && !projects.some((p) => p.id === route.project);
  if (projectMissing) {
    // The id never appears in the UI as something to copy, so a hand-typed
    // first segment is almost always the project NAME the sidebar shows. One
    // unambiguous match resolves to its id, carrying the rest of the URL —
    // path, view, target, filters, version — along with it.
    const named = projectByName(projects, route.project!);
    if (named) {
      return (
        <Redirect
          to={
            route.view
              ? urlForView(route.view, named, route.viewTarget, route.filters)
              : urlForPath(route.path, named, route.version)
          }
        />
      );
    }
    // Nothing resolved, so this returns early rather than falling through:
    // every redirect below rewrites the address bar off `current.id`, and all
    // of them are wrong for a segment that named nothing — /bad-id,
    // /bad-id/insights and /bad-id/notes/ would each swap in another project
    // and drop the path. The URL stays as typed.
    //
    // And it renders with NO tree. `current` still resolves, but painting its
    // sidebar beside a "not found" body is the page arguing with itself: two
    // personas read the full file tree of a project they were looking at,
    // one of them its owner, next to a line telling them they were no longer
    // a member of it.
    return (
      <AppShell
        vault={vault}
        projectsNav={<ProjectNav projects={projects} onNew={newProject} />}
        orgBar={accountBar}
        topbar={<Topbar />}
      >
        <Page>
          <div className="empty">
            <h3>Project not found</h3>
            <p>
              There's no project called “{decodePath(route.project!)}” in your account. It may
              have been renamed or deleted, or the link may be wrong.
            </p>
            <p>
              <a {...linkProps("/" + current.id)}>Back to {current.name}</a>
            </p>
          </div>
        </Page>
        {newProjectDialog}
      </AppShell>
    );
  }

  // Billing is hub-level (the managed deployment's surface), not
  // project-scoped — like the org route it borrows whichever project the
  // sidebar is showing. An OSS hub has no billing block; a hand-typed
  // /billing there says so instead of silently showing files.
  const billingPage = route.billing
    ? {
        crumb: "Billing",
        body: config.billing ? (
          <BillingView url={config.billing.url} />
        ) : (
          <div className="empty">
            <h3>No billing on this hub</h3>
            <p>This BearDrive hub doesn't have a billing surface.</p>
          </div>
        ),
      }
    : null;

  const routePage =
    route.view === "settings"
      ? {
          crumb: "Project settings",
          body: (
            <ProjectSettings
              project={project ?? current}
              org={org}
              onDeleted={async () => {
                // The id is dead now: refresh drops it from the list, and
                // navigating home lands on whatever project is left (or the
                // empty state) in one hop instead of via the stale-route
                // redirect below.
                await refresh();
                navigate("/");
              }}
            />
          ),
        }
      : route.view === "install"
        ? {
            // The same guide the project home shows, in the same column —
            // it used to sit in the .onboard card, 320px narrower and 90px
            // lower than home, two sidebar items apart.
            crumb: "Installation",
            body: <ConnectGuide project={current} existing={route.connect === "existing"} />,
          }
        : null;

  // Everything below rewrites the address bar off `current.id`. A missing
  // project can no longer reach here — it returned above — so these need no
  // guard of their own.

  // Landing ("/") resolves to a real project URL; replace so back/forward
  // never bounces through the redirect. The org route is not project-scoped,
  // so it is exempt — it borrows whichever project the sidebar is showing.
  if (!route.org && !route.billing && route.project !== current.id) {
    return <Redirect to={"/" + current.id} />;
  }

  // A renamed view URL (/insights) still resolves; swap it for the current
  // one so there is one live URL per page. Filters ride along: the hop is a
  // rename, not a reset, and dropping them would silently widen the feed.
  if (route.legacyView && route.view) {
    return <Redirect to={urlForView(route.view, current.id, route.viewTarget, route.filters)} />;
  }

  // /history?path=guide.md resolved to guide.md's feed (the query form is
  // what the History API teaches); put the canonical path URL in the address
  // bar.
  if (route.queryTarget && route.view) {
    return <Redirect to={urlForView(route.view, current.id, route.viewTarget, route.filters)} />;
  }

  // /notes/ is the same page as /notes — resolve it, then take the slash off
  // the address bar. After the rewrite the flag is false, so there is no
  // second hop.
  if (route.trailingSlash && route.path) {
    return <Redirect to={urlForPath(route.path, current.id, route.version)} />;
  }

  return (
    <>
      <Browser
      key={current.id} // fresh tree/fold state per project
      config={config}
      apiBase={"/api/p/" + current.id + "/"}
      route={route}
      hub
      project={project ?? current}
      projects={projects}
      sidebar={{
        vault,
        projectsNav: (
          <ProjectNav
            projects={projects}
            currentId={current.id}
            onNew={newProject}
            menu={{
              // Scoped views (/dashboard/<path>, /history/<path>) belong to
              // the file/folder — the tree carries the selection, no menu
              // item lights up.
              active: panel
                ? null
                : route.view === "dashboard" && !route.viewTarget
                  ? "dashboard"
                  : route.view === "install"
                    ? "install"
                    : route.view === "history" && !route.viewTarget
                      ? "history"
                      : route.view === "settings"
                        ? "settings"
                        : null,
              // Each page is a URL; explicitly close overlay panels because
              // same-path navigation doesn't change pathname.
              onDashboard: () => {
                setPanel(null);
                navigate(urlForView("dashboard", current.id));
                closeSidebarOnMobile();
              },
              onInstall: () => {
                setPanel(null);
                navigate(urlForView("install", current.id));
                closeSidebarOnMobile();
              },
              onHistory: () => {
                setPanel(null);
                navigate(urlForView("history", current.id));
                closeSidebarOnMobile();
              },
              onSettings: () => {
                setPanel(null);
                navigate(urlForView("settings", current.id));
                closeSidebarOnMobile();
              },
            }}
          />
        ),
        orgBar: accountBar,
      }}
      panel={activePanel || orgPage || billingPage || routePage}
      onClosePanel={() => setPanel(null)}
      />
      {newProjectDialog}
    </>
  );
}

/* Opening "/join/<token>" joins the invite's org. If the visitor isn't
   signed in yet, the 401 handler sends them to /auth/login with the /join
   path intact in `next`, so after signing in the server re-serves the app
   here and the join completes — the token is never lost. */
function JoinInvite({ token, onDone }: { token: string; onDone: (orgId: string | null) => void }) {
  useEffect(() => {
    let cancelled = false;
    postJSON<InviteAccepted>("/api/invites/" + token)
      .then((out) => {
        if (cancelled) return;
        toast(`Welcome — you joined the “${out.org.name}” team. Opening its projects…`);
        onDone(out.org.id);
      })
      .catch((e) => {
        if (cancelled || String((e as Error).message).includes("signing in")) return;
        toast("Could not accept the invite: " + (e as Error).message, true);
        onDone(null);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token]);
  return (
    <AppShell vault={<VaultHeader name="BearDrive" beta />} topbar={<Topbar />}>
      <Page>
        <div className="empty">Joining…</div>
      </Page>
    </AppShell>
  );
}
