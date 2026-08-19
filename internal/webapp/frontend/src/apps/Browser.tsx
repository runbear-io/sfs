import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { atLeast } from "../api/types";
import { getJSON, postJSON } from "../api/http";
import type { Project, ServerConfig } from "../api/types";
import { useHeat, useTree } from "../hooks/useBrowse";
import { useShares } from "../hooks/useHub";
import { urlForPath, urlForView, type Route } from "../router";
import { currentNavType, navigate, useLocationPath } from "../nav";
import { HTML_EXT, PDF_EXT, copyText } from "../util";
import { toast } from "../toast";
import { modalConfirm } from "../modal";
import { onSearchRequest } from "../search";
import { track } from "../analytics";
import { AppShell, Icon, Page, Topbar, closeSidebarOnMobile, type PageWidth } from "../components/shell";
import { FileTree, ancestorsOf } from "../components/FileTree";
import { Breadcrumbs } from "../components/Breadcrumbs";
import { FolderListing } from "../components/FolderListing";
import { FileView } from "../components/FileView";
import { ShareDialog } from "../components/ShareDialog";
import { ShareBanner } from "../components/ShareBanner";
import { Palette, type PaletteItem } from "../components/Palette";
import { ConnectGuide } from "../components/ConnectGuide";
import { Insights, useInsightsDevices } from "../components/Insights";
import { HistoryView, historyTitle } from "../components/HistoryView";
import { VersionBanner } from "../components/VersionBanner";

// The hub's six share-time credential rules, in words. Only one caller
// (shareNow), so it lives here rather than in its own file.
const SECRET_LABELS: Record<string, string> = {
  aws_access_key_id: "an AWS access key",
  openai_api_key: "an OpenAI API key",
  github_pat: "a GitHub token",
  slack_token: "a Slack token",
  private_key: "a private key",
  gitlab_pat: "a GitLab token",
};

// secretsMessage phrases the 409 for the confirm dialog. The second sentence
// is not decoration: a link always serves the file's LATEST content, so the
// copy may only ever claim what was true at the moment of sharing — never
// that the file is clean.
function secretsMessage(findings: { rule: string; line: number }[] = []): string {
  const parts = findings.map((f) => `${SECRET_LABELS[f.rule] ?? f.rule} (line ${f.line})`);
  const what =
    parts.length > 1
      ? parts.slice(0, -1).join(", ") + " and " + parts[parts.length - 1]
      : parts[0] || "something credential-shaped";
  return (
    `BearDrive found ${what} in this file. The check covers the file at the moment you share it — ` +
    `a link always serves the file's latest content, so later changes are never checked. Share anyway?`
  );
}

// The browsing surface shared by hub projects and single-volume mode: the
// file tree, folder listings, file views, and every topbar action. Sidebar
// chrome (vault header, project nav, org bar) is injected by the caller;
// key this component by project id so tree state resets on project switch.
export default function Browser(props: {
  config: ServerConfig;
  apiBase: string;
  route: Route;
  hub: boolean;
  project?: Project;
  projects?: Project[];
  sidebar: { vault: ReactNode; projectsNav?: ReactNode; orgBar?: ReactNode };
  // Admin panels (org admin, hub settings) replace the content pane without
  // touching the URL — matching the classic app, where they were never
  // routes. Any navigation closes them (the caller owns that state).
  panel?: { crumb: string; body: ReactNode } | null;
  onClosePanel?: () => void; // panels are not routes: same-path navigation needs an explicit close
}) {
  const { config, apiBase, route, hub, project } = props;
  const routeKey = useLocationPath(); // scroll memo key, one slot per URL
  const qc = useQueryClient();

  const { tree, flatFiles, dirIndex, loaded } = useTree(apiBase, !hub || !!project);
  const heatMap = useHeat(apiBase, hub && !!project && !!config.reads?.enabled);
  // Dashboard data: the per-device breakdown, plus a fresh heat fetch when
  // a dashboard surface opens (the ambient heat cache may be a minute old).
  const isHome = hub && !!project && !route.path && !route.view;
  const insightsOpen = route.view === "dashboard" || isHome;
  const devices = useInsightsDevices(apiBase, insightsOpen);
  useEffect(() => {
    if (insightsOpen) qc.invalidateQueries({ queryKey: ["heat", apiBase] });
  }, [insightsOpen, apiBase, qc]);

  const path = route.path;
  // ?v= belongs to the file page; a view route or folder ignores it.
  const version = !route.view ? route.version : undefined;
  // On scoped view routes (/dashboard/<p>, /history/<p>) the subject of the
  // page is the target — the tree highlights it, not a menu item.
  const treePath = path || (route.view === "dashboard" || route.view === "history" ? route.viewTarget || "" : "");
  const isDir = !!path && dirIndex.has(path);
  // A file only counts as one when the tree actually contains it — a
  // missing path gets the not-found view, not a broken file view.
  const isFile = !!path && loaded && !isDir && flatFiles.some((f) => f.path === path);
  const isMissing = !!path && loaded && !isDir && !isFile;
  const listingShowing = isDir && !route.view;

  /* ---- an address whose file moved ----
     Files get renamed and dragged into folders, and the old URL is already
     in someone's notes. The server can pair the delete with the put that
     carried the same blob, so ask it — but only once the tree says the path
     is gone, so the happy path costs nothing. It is a separate call rather
     than the X-Bdrive-Canonical-Path header /file answers with, because we
     never fetch a missing file at all, and a moved FOLDER has no content
     fetch to hang a header on. */
  const { data: moved } = useQuery({
    queryKey: ["resolve", apiBase, path],
    queryFn: () =>
      getJSON<{ to: string; kind: string }>(apiBase + "resolve?path=" + encodeURIComponent(path)),
    enabled: isMissing,
    retry: false, // a 404 here is the normal answer, not a flake
    staleTime: 60_000,
  });
  const [movedFrom, setMovedFrom] = useState<{ from: string; to: string } | null>(null);
  useEffect(() => {
    if (!isMissing || !moved?.to) return;
    setMovedFrom({ from: path, to: moved.to });
    navigate(urlForPath(moved.to, project?.id), { replace: true });
  }, [isMissing, moved, path, project?.id]);

  /* ---- tree expansion ---- */
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set());
  const firstLoad = useRef(true);
  useEffect(() => {
    // First render of the tree: every folder starts closed, except a lone
    // root folder — opening it spares the user a single shut folder.
    if (!tree || !firstLoad.current) return;
    firstLoad.current = false;
    const rootDirs = (tree.children || []).filter((c) => c.dir);
    if (rootDirs.length === 1) setExpanded((s) => new Set(s).add(rootDirs[0].path));
  }, [tree]);
  useEffect(() => {
    // Opening any path (tree click, palette, wikilink, deep link — or a
    // scoped dashboard/history view of it) unfolds the way to it; a selected
    // folder itself opens too.
    if (!treePath || !loaded) return;
    setExpanded((s) => {
      const next = new Set(s);
      for (const a of ancestorsOf(treePath)) next.add(a);
      if (dirIndex.has(treePath)) next.add(treePath);
      return next;
    });
  }, [treePath, loaded, dirIndex]);
  const onToggle = useCallback((p: string) => {
    setExpanded((s) => {
      const next = new Set(s);
      if (next.has(p)) next.delete(p);
      else next.add(p);
      return next;
    });
  }, []);

  /* ---- per-route scroll restoration ----
     Back/forward returns to where the reader was; fresh navigations start
     at the top. Views call onRendered when their content lands (and again
     when async sections grow), and we re-apply the target until it fits. */
  const contentRef = useRef<HTMLElement>(null);
  const memo = useRef(new Map<string, number>());
  const scrollGoal = useRef({ key: "", want: 0, attempts: 0 });
  useEffect(() => {
    scrollGoal.current = {
      key: routeKey,
      want: currentNavType() === "POP" ? (memo.current.get(routeKey) ?? 0) : 0,
      attempts: 0,
    };
  }, [routeKey]);
  const onRendered = useCallback(() => {
    const c = contentRef.current;
    const g = scrollGoal.current;
    if (!c || g.key !== routeKey || g.attempts >= 3) return;
    g.attempts++;
    c.scrollTo({ top: g.want, behavior: "instant" });
  }, [routeKey]);
  const onScroll = useCallback(() => {
    if (contentRef.current) memo.current.set(routeKey, contentRef.current.scrollTop);
  }, [routeKey]);

  /* ---- navigation ---- */
  const openPath = useCallback(
    // A version (a history row's content hash) pins the file page to those
    // exact bytes; without one the page is the current file.
    (p: string, v?: string) => {
      navigate(urlForPath(p, project?.id, v));
      closeSidebarOnMobile();
    },
    [project?.id],
  );
  const openHistory = useCallback(
    (target: string) => navigate(urlForView("history", project?.id, target)),
    [project?.id],
  );

  /* ---- topbar state + actions ---- */
  const [meta, setMeta] = useState("");
  const [share, setShare] = useState<{ url: string; copied: boolean } | null>(null);
  const [moreOpen, setMoreOpen] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  useEffect(() => onSearchRequest(() => setPaletteOpen(true)), []);
  const downloadRef = useRef<HTMLAnchorElement>(null);

  const panel = props.panel ?? null;
  // Minting a public link is a write. A read-only member sees no Share
  // button rather than a button that 403s. On desktop every project reads
  // as perm "read" (the local sidecar never writes), but shares are proxied
  // to the hub, which enforces the caller's real permission — so the button
  // shows and a genuinely read-only member gets the hub's 403 as a toast.
  const canShare = !panel && hub && !!project && isFile && (config.desktop || atLeast(project.perm, "write"));
  // The project's live public links, filtered to the open file. One query
  // for the whole project (Settings reads the same cache entry), so opening
  // a file costs no extra request.
  const { data: shares } = useShares(project?.id, hub && !!project);
  const refreshShares = useCallback(
    () => void qc.invalidateQueries({ queryKey: ["shares", project?.id] }),
    [qc, project?.id],
  );
  const fileShares = isFile ? (shares || []).filter((s) => s.path === path) : [];
  // Desktop only: each mount's hub base URL, for "Copy web link". Hub and
  // desktop use identical route paths (projects are keyed by hub id), so the
  // teammate-shareable URL is just the hub origin + the current pathname.
  const { data: desktopStatus } = useQuery({
    queryKey: ["desktop-status"],
    queryFn: () => getJSON<{ mounts: { project: string; server: string }[] }>("/api/desktop/status"),
    enabled: !!config.desktop,
    staleTime: 60_000,
  });
  const webBase = config.desktop && project ? desktopStatus?.mounts.find((m) => m.project === project.id)?.server : undefined;
  const copyWebLink = useCallback(async () => {
    if (!webBase) return;
    const url = webBase + window.location.pathname;
    const copied = await copyText(url);
    toast(copied ? "Web link copied" : url, !copied);
  }, [webBase]);
  const canHistory = !panel && hub && !!project;
  // Browser upload is deliberately absent (for now): content enters through
  // local sync only; the web app is a read/share/history surface.
  const canDownload = !panel && isFile;
  const canMore = !panel && (isFile || (hub && !!project && isDir));
  // Downloading while a version is open gives you THAT version — the ⋯ menu
  // offering the current bytes under a page framed as historical was half of
  // what made old versions unreachable.
  const downloadURL = version
    ? apiBase + "blob?sha=" + version + "&name=" + encodeURIComponent(path) + "&download=1"
    : apiBase + "download?path=" + encodeURIComponent(path);

  const shareNow = useCallback(async () => {
    // Shares are per-file; a selected folder has nothing to mint.
    const post = (confirm: boolean) =>
      fetch(apiBase + "shares", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(confirm ? { path, confirm: true } : { path }),
      });
    try {
      let r = await post(false);
      // 409: the hub found credential-shaped strings and minted nothing.
      // Read the structured body — this is a raw fetch, so it never passes
      // through errorFor() in api/http.ts, which would flatten it to a toast.
      if (r.status === 409) {
        const { findings } = (await r.json()) as { findings?: { rule: string; line: number }[] };
        if (!(await modalConfirm("This file may contain credentials", secretsMessage(findings), "Share anyway", true)))
          return; // Cancel mints nothing, and fires no share_created
        r = await post(true);
      }
      if (!r.ok) throw new Error(await r.text());
      const s = await r.json();
      // Fired here rather than by the table in api/http.ts, because this is
      // the one write in the app that goes out as a raw fetch.
      track("share_created");
      const copied = await copyText(s.url);
      setShare({ url: s.url, copied });
      refreshShares(); // the banner appears (or stays) without a reload
    } catch (err) {
      toast("Share failed: " + (err as Error).message, true);
    }
  }, [apiBase, path, refreshShares]);

  /* ---- restore ----
     Putting an old version back is a write, so a read-only member sees no
     Restore button rather than one that 403s. The restore itself is a new
     change: the tree, the file, and the history feed all move — so it asks
     first, like every other action here that leaves the browser (BEA-129).
     The second line is case-aware because the two cases really differ:
     swapping a live file's content is walk-backable from History, bringing a
     deleted file back is not — the run card's "undo — remove file" is the
     only delete control in the web UI, and a restore produces no run card.
     Non-danger styling on purpose: restore adds content, it takes none away. */
  const [restoring, setRestoring] = useState("");
  const canRestore = hub && !!project && atLeast(project?.perm, "write");
  const onRestore = useCallback(
    async (p: string, sha: string, recreates: boolean) => {
      if (
        !(await modalConfirm(
          "Restore this version of " + p + "?",
          "It syncs to every device as a new change. " +
            (recreates
              ? "The file comes back on every device. Removing it again isn't available from History yet."
              : "You can restore any other version afterwards."),
          "Restore",
        ))
      )
        return;
      setRestoring(p + sha);
      try {
        await postJSON(apiBase + "restore", { path: p, sha });
        qc.invalidateQueries({ queryKey: ["history", apiBase] });
        qc.invalidateQueries({ queryKey: ["tree", apiBase] });
        qc.invalidateQueries({ queryKey: ["render", apiBase, p] });
        qc.invalidateQueries({ queryKey: ["text"] });
        toast("Restored " + p + " — it syncs to every device like any other change.");
      } catch (err) {
        toast("Restore failed: " + (err as Error).message, true);
      } finally {
        setRestoring("");
      }
    },
    [apiBase, qc],
  );

  /* ---- undo a file a run created ----
     The other half of restore: it takes a file away, on every synced device,
     so it always asks first. History keeps it — the DELETED row it leaves
     restores it back — which is what the confirm says. */
  const [removing, setRemoving] = useState("");
  const onRemove = useCallback(
    async (p: string) => {
      if (
        !(await modalConfirm(
          "Remove " + p + "?",
          "It disappears from every synced device. History keeps it — you can restore it from the DELETED row afterwards.",
          "Remove file",
          true,
        ))
      )
        return;
      setRemoving(p);
      try {
        await postJSON(apiBase + "remove", { path: p });
        qc.invalidateQueries({ queryKey: ["history", apiBase] });
        qc.invalidateQueries({ queryKey: ["tree", apiBase] });
        qc.invalidateQueries({ queryKey: ["render", apiBase, p] });
        qc.invalidateQueries({ queryKey: ["text"] });
        toast("Removed " + p + " — it syncs to every device like any other change.");
      } catch (err) {
        toast("Remove failed: " + (err as Error).message, true);
      } finally {
        setRemoving("");
      }
    },
    [apiBase, qc],
  );

  const historyNow = useCallback(() => {
    if (!path) return openHistory("");
    openHistory(isDir ? path + "/" : path);
  }, [path, isDir, openHistory]);

  /* ---- ⌘K palette ---- */
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPaletteOpen((v) => !v);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const paletteCandidates = useCallback((): PaletteItem[] => {
    const items: PaletteItem[] = [];
    const add = (icon: string, label: string, kind: string, run: () => void) =>
      items.push({ icon, label, kind, run });
    // The project's own destinations, first and unconditional: on a path that
    // doesn't resolve the tree-derived entries are gone and the switcher lists
    // only OTHER projects, so without these the palette — the natural escape
    // hatch on a dead route — is the one surface with no way back. An empty
    // query scores everything 0 and the sort is stable, so first here is first
    // on screen. Panels aren't routes and only close on a location CHANGE, so
    // picking the page you're already on needs the explicit close.
    if (hub && project) {
      const pid = project.id;
      const go = (to: string) => () => {
        props.onClosePanel?.();
        navigate(to);
      };
      add("folder", "Go to project root", "action", go("/" + pid));
      add("dashboard", "Dashboard", "action", go(urlForView("dashboard", pid)));
      add("terminal", "Installation", "action", go(urlForView("install", pid)));
      add("gear", "Settings", "action", go(urlForView("settings", pid)));
    }
    if (hub && project && path) {
      if (isFile) add("share", "Share: " + path, "action", shareNow);
      add("hist", "History: " + path, "action", historyNow);
      if (isFile) add("download", "Download: " + path, "action", () => downloadRef.current?.click());
    }
    if (hub && project) add("hist", "History: whole project", "action", () => openHistory(""));
    if (hub) {
      for (const p of props.projects || []) {
        if (!project || p.id !== project.id) {
          add("folder", "Switch to project: " + p.name, "project", () => navigate("/" + p.id));
        }
      }
    }
    if (config.auth?.enabled) {
      add("power", "Sign out", "action", () => (window.location.href = "/auth/logout"));
    }
    for (const d of dirIndex.keys()) add("folder", d, "folder", () => openPath(d));
    for (const f of flatFiles) add("doc", f.path, "file", () => openPath(f.path));
    return items;
  }, [hub, project, path, isFile, config.auth?.enabled, dirIndex, flatFiles, props.projects, props.onClosePanel, shareNow, historyNow, openHistory, openPath]);

  /* ---- "⋯ More" menu (secondary actions on narrow screens) ---- */
  useEffect(() => {
    if (!moreOpen) return;
    const close = () => setMoreOpen(false);
    document.addEventListener("click", close);
    return () => document.removeEventListener("click", close);
  }, [moreOpen]);

  /* ---- content view ---- */
  const isFolderFn = useCallback((p: string) => dirIndex.has(p), [dirIndex]);
  // One column decision per route (see <Page> in shell.tsx). File views also
  // carry the markdown typography class, which used to sit on #content itself
  // — putting the width there made the gutter eat into the reading column, so
  // .md pages ran 80px narrower than every other page.
  let pageWidth: PageWidth = "app";
  let pageClass: string | undefined;
  let view: ReactNode;
  if (panel) {
    view = panel.body;
  } else if (route.view === "dashboard") {
    view = (
      <Insights
        flatFiles={flatFiles}
        heatMap={heatMap}
        devices={devices}
        scope={route.viewTarget || ""}
        loading={!loaded}
        installHref={project ? urlForView("install", project.id) : undefined}
        onOpenFile={openPath}
        onOpenFolder={openPath}
        onOpenHistory={openHistory}
        isFolder={isFolderFn}
      />
    );
  } else if (route.view === "history") {
    // structured view — default app column, like the folder listing it shares rows with
    view = (
      <HistoryView
        apiBase={apiBase}
        target={route.viewTarget || ""}
        isFolder={isFolderFn}
        onOpen={openPath}
        onMeta={setMeta}
        onRendered={onRendered}
        restore={canRestore ? { onRestore, busy: restoring } : undefined}
        remove={canRestore ? { onRemove, busy: removing } : undefined}
        filters={route.filters}
        /* push, not replace: a filter is a navigation, and Back undoes it */
        onFilters={(f) => navigate(urlForView("history", project?.id, route.viewTarget || "", f))}
      />
    );
  } else if (path) {
    if (!loaded) {
      view = <div className="empty">Loading…</div>;
    } else if (isMissing) {
      // The tree polls every few seconds, so a file that's mid-upload (or
      // mid-sync from a teammate's device) appears here on its own.
      view = (
        <div className="notfound">
          <h1>Couldn't find that</h1>
          <p>
            <code>{path}</code> isn't in this project right now.
          </p>
          <p className="nf-sub">
            If it was just created, it may still be uploading or syncing
            from a teammate's device — this page checks again automatically
            every few seconds, so refresh or come back in a moment.
          </p>
          <button
            className="pbtn"
            onClick={() => qc.invalidateQueries({ queryKey: ["tree", apiBase] })}
          >
            Check again
          </button>
        </div>
      );
    } else if (isDir) {
      view = ( // structured view — default app column; read is for rendered files only
        <FolderListing
          node={dirIndex.get(path)!}
          heatMap={heatMap}
          hub={hub && !!project}
          apiBase={apiBase}
          onOpen={openPath}
          onFullHistory={openHistory}
          onRendered={onRendered}
        />
      );
    } else {
      // A PDF page is unreadable squeezed into the 768px reading column.
      pageWidth = HTML_EXT.test(path) || PDF_EXT.test(path) ? "wide" : "read";
      pageClass = "markdown";
      view = (
        <>
          {version && (
            <VersionBanner
              apiBase={apiBase}
              path={path}
              version={version}
              onViewCurrent={() => openPath(path)}
            />
          )}
          <FileView
            apiBase={apiBase}
            path={path}
            version={version}
            heatMap={heatMap}
            flatFiles={flatFiles}
            onOpenFile={openPath}
            onMeta={setMeta}
            onRendered={onRendered}
          />
        </>
      );
    }
  } else if (isHome) {
    // The project's index page: the connect-an-agent guide, with the
    // dashboard below it.
    view = (
      <>
        <ConnectGuide project={project!} existing={route.connect === "existing"} />
        <div className="home-insights">
          {/* No install CTA here: ConnectGuide directly above IS the set-up-a-
              device guide, and a second button six inches under the first
              reads as two different steps. */}
          <Insights
            flatFiles={flatFiles}
            heatMap={heatMap}
            devices={devices}
            loading={!loaded}
            onOpenFile={openPath}
            onOpenFolder={openPath}
            onOpenHistory={openHistory}
            isFolder={isFolderFn}
          />
        </div>
      </>
    );
  } else {
    view = <div className="empty">Select a file to read it.</div>;
  }

  // Arriving here by redirect: say so, or the URL silently changed under a
  // reader who typed the other one. Above whatever the destination renders,
  // so a moved folder gets it too.
  if (movedFrom && movedFrom.to === path) {
    view = (
      <>
        <div className="vbanner" role="status">
          <span className="vb-icon">
            <Icon name="link" />
          </span>
          <div className="vb-text">
            <b>Moved from {movedFrom.from}</b>
            <span>The URL has been updated.</span>
          </div>
        </div>
        {view}
      </>
    );
  }

  const crumb = panel ? (
    panel.crumb
  ) : path ? (
    <Breadcrumbs path={path} onOpenFolder={openPath} />
  ) : route.view === "dashboard" ? (
    "Dashboard — " + (route.viewTarget || project?.name || "")
  ) : route.view === "history" ? (
    "History — " + historyTitle(route.viewTarget || "", isFolderFn)
  ) : isHome ? (
    project!.name
  ) : null;

  const topbar = (
    <Topbar
      // The desktop window has no browser chrome, so back/forward live here
      // (the app menu's ⌘[ / ⌘] drive the same history).
      nav={
        config.desktop ? (
          <span id="nav-btns">
            <button className="nav-btn" title="Back (⌘[)" aria-label="Back" onClick={() => history.back()}>
              <Icon name="chevl" />
            </button>
            <button className="nav-btn" title="Forward (⌘])" aria-label="Forward" onClick={() => history.forward()}>
              <Icon name="chev" />
            </button>
          </span>
        ) : undefined
      }
      crumb={crumb}
      meta={meta}
      actions={
        <>
          {canShare && (
            <Button id="share-btn" variant="toolbar" className="icon-only" title="Share" aria-label="Share" onClick={shareNow}>
              <Icon name="share" />
            </Button>
          )}
          {canHistory && !path && !route.view && (
            <Button id="history-btn" variant="toolbar" onClick={historyNow}>
              <Icon name="hist" /> <span className="lbl">History</span>
            </Button>
          )}
          {canDownload && (
            <a id="download" hidden download href={downloadURL} ref={downloadRef}>
              Download
            </a>
          )}
          {canMore && (
            <Button
              id="more-btn"
              variant="toolbar"
              className="icon-only"
              title="More actions"
              aria-label="More actions"
              onClick={(e) => {
                e.stopPropagation();
                setMoreOpen(!moreOpen);
              }}
            >
              <Icon name="dots" />
            </Button>
          )}
          {moreOpen && (
            <div id="more-menu" role="menu">
              {canHistory && (
                <button className="more-item" onClick={historyNow}>
                  History
                </button>
              )}
              {canDownload && (
                <button className="more-item" onClick={() => downloadRef.current?.click()}>
                  Download
                </button>
              )}
              {webBase && (
                <button className="more-item" onClick={copyWebLink}>
                  Copy web link
                </button>
              )}
              {hub && !!project && (
                <button
                  className="more-item"
                  onClick={() => {
                    props.onClosePanel?.();
                    navigate(urlForView("dashboard", project?.id, path));
                  }}
                >
                  Dashboard
                </button>
              )}
            </div>
          )}
        </>
      }
    />
  );

  return (
    <>
      <AppShell
        vault={props.sidebar.vault}
        projectsNav={props.sidebar.projectsNav}
        orgBar={props.sidebar.orgBar}
        tree={
          <FileTree
            root={tree}
            expanded={expanded}
            onToggle={onToggle}
            currentPath={treePath}
            listingShowing={listingShowing}
            onOpen={openPath}
          />
        }
        topbar={topbar}
        contentRef={contentRef}
        onContentScroll={onScroll}
      >
        <Page width={pageWidth} className={pageClass}>
          {!panel && isFile && (
            <ShareBanner
              shares={fileShares}
              canRevoke={!!project && atLeast(project.perm, "write")}
              onChanged={refreshShares}
            />
          )}
          {view}
        </Page>
      </AppShell>
      {share && (
        <ShareDialog
          url={share.url}
          copied={share.copied}
          onClose={() => {
            setShare(null);
            // Create may have handed back an existing link, in which case
            // shareNow's own invalidation raced a stale cache. Refresh again
            // so the banner agrees with what the dialog just showed.
            refreshShares();
          }}
        />
      )}
      <Palette open={paletteOpen} onClose={() => setPaletteOpen(false)} candidates={paletteCandidates} />
    </>
  );
}
