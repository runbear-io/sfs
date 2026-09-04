import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { FrontmatterPair, HeatMap, Node, RenderDoc } from "../api/types";
import { heatTotal, heatText } from "../hooks/useBrowse";
import { HEAT_DISCLOSURE, staleNote } from "../lib/heat";
import { fileURLFor, useTextAt } from "../hooks/useBlob";
import {
  CSV_EXT,
  HTML_EXT,
  IMG_EXT,
  MD_EXT,
  PDF_EXT,
  TEXT_EXT,
  fmPanelOpen,
  humanSize,
  joinPath,
  rememberFmPanel,
  resolveWiki,
  whoChanged,
} from "../util";
import { urlForPath } from "../router";
import { CSV_ROWS, parseDelimited, type Csv } from "../lib/csv";
import { hasMermaid, renderMermaid } from "../lib/mermaid";
import { secretsBadge, type SecretFinding } from "../lib/secrets";
import { Icon } from "./shell";

export function FileView(props: {
  apiBase: string;
  path: string;
  // Pinned to one past version by content hash (?v=), otherwise current.
  version?: string;
  heatMap: HeatMap | null;
  flatFiles: Node[];
  // Hub mode only; absent in volume mode, where file URLs are "/<path>".
  projectId?: string;
  onOpenFile: (path: string) => void;
  onMeta: (meta: ReactNode) => void;
  onRendered?: () => void;
}) {
  const { apiBase, path, version, onMeta } = props;
  const fileURL = fileURLFor(apiBase, path, version);

  useEffect(() => () => onMeta(""), [path, onMeta]); // leaving a file clears its meta line

  if (MD_EXT.test(path)) return <MarkdownView {...props} />;
  if (HTML_EXT.test(path)) {
    // Rendered, not shown as source — inside a sandboxed iframe so synced
    // HTML never runs with the hub origin's session (the server also
    // stamps the response with a sandbox CSP; this is belt and braces).
    return (
      <iframe
        className="htmlview"
        sandbox="allow-scripts"
        src={fileURL}
        title={path}
        onLoad={props.onRendered}
      />
    );
  }
  if (PDF_EXT.test(path)) {
    // The browser's own viewer, streaming — no byte cap needed, nothing is
    // held in JS memory. Deliberately NOT sandboxed: the PDF viewer is not
    // this page's JS realm, so it can't reach the hub API or its cookies,
    // and sandbox without allow-same-origin breaks Firefox's pdf.js.
    return <iframe className="pdfview" src={fileURL} title={path} onLoad={props.onRendered} />;
  }
  if (IMG_EXT.test(path)) {
    return <ImgView src={fileURL} alt={path} version={version} onRendered={props.onRendered} />;
  }
  // Same component as plain text on purpose: the fallback for a file the
  // parser can't make a table of is then the very JSX it already renders,
  // not a second code path to keep in sync.
  if (CSV_EXT.test(path)) {
    return <TextView {...props} fileURL={fileURL} delim={/\.tsv$/i.test(path) ? "\t" : ","} />;
  }
  if (TEXT_EXT.test(path)) return <TextView {...props} fileURL={fileURL} />;
  // No extension we recognize: decide on the bytes instead of giving up.
  return <SniffView {...props} fileURL={fileURL} />;
}

/* The fallthrough: one fetch, then text / binary / too-large. Only files
   that used to show the dead "No preview" card get here, so nothing that
   already previewed pays for the extra request. */
function SniffView(props: Parameters<typeof FileView>[0] & { fileURL: string }) {
  const { apiBase, path, version, fileURL, onRendered } = props;
  // The ["text", url] family is what a restore invalidates (Browser.tsx);
  // an immutable ["blob", …] key on a live path would go stale after a
  // teammate's edit. A ?v= URL is content-addressed, so it can be pinned.
  const { data, error } = useTextAt(fileURL, ["text", fileURL], true, !!version);
  useEffect(() => {
    if (data) onRendered?.();
  }, [data, onRendered]);
  if (error) return <LoadError version={version} err={error as Error} />;
  if (!data) return null;
  if (data.kind === "text")
    return (
      <pre className="plain" key={path}>
        {data.text}
      </pre>
    );
  return (
    <FileCard apiBase={apiBase} path={path} version={version} fileURL={fileURL}>
      {data.kind === "too-large"
        ? `Too large to preview (${humanSize(data.size)}).`
        : "No preview for this file type."}
    </FileCard>
  );
}

function FileCard(props: {
  apiBase: string;
  path: string;
  version?: string;
  fileURL: string;
  children: React.ReactNode;
}) {
  const { apiBase, path, version, fileURL } = props;
  return (
    <div className="filecard">
      <div className="name">{path.split("/").pop()}</div>
      <p>{props.children}</p>
      <a
        className="btn"
        download
        href={
          version ? fileURL + "&download=1" : apiBase + "download?path=" + encodeURIComponent(path)
        }
      >
        Download
      </a>
    </div>
  );
}

function MarkdownView(props: Parameters<typeof FileView>[0]) {
  const { apiBase, path, version, heatMap, flatFiles, projectId, onOpenFile, onMeta, onRendered } =
    props;
  const { data: doc, error } = useQuery({
    queryKey: ["render", apiBase, path, version || ""],
    queryFn: () =>
      getJSON<RenderDoc>(
        apiBase + "render?path=" + encodeURIComponent(path) + (version ? "&sha=" + version : ""),
      ),
    // A blob that isn't there will not appear on a retry, and the retry's
    // delay is a blank pane the reader has no explanation for.
    retry: version ? false : undefined,
  });

  // Rewrite the HTML BEFORE rendering (relative image sources, external
  // link targets) rather than patching the live DOM afterwards: React owns
  // the dangerouslySetInnerHTML subtree and may re-apply the markup on any
  // update, silently discarding post-commit DOM patches. Link navigation
  // is delegated on the container for the same reason.
  const html = useMemo(
    () => (doc ? transformHTML(doc.html, path, apiBase, flatFiles, projectId) : ""),
    [doc, path, apiBase, flatFiles, projectId],
  );

  // Diagrams are rendered into a NEW html string and fed back through state,
  // so the one dangerouslySetInnerHTML below re-mounts with the SVG already
  // in it — the same reason transformHTML runs before the mount and not after
  // it. Runs on transformHTML's output, so a diagram's SVG never goes through
  // the img/link rewriting pass.
  const [diagrams, setDiagrams] = useState<string | null>(null);
  useEffect(() => {
    setDiagrams(null);
    if (!hasMermaid(html)) return; // no fence: mermaid is never downloaded
    let cancelled = false;
    renderMermaid(html).then((out) => {
      // A slow render of the file we just left must not paint over this one.
      if (!cancelled) setDiagrams(out);
    });
    return () => {
      cancelled = true;
    };
  }, [html]);

  useEffect(() => {
    if (!doc) return;
    const parts: string[] = [];
    // Read counts belong to the path, not to one version — showing them
    // beside content the banner just called historical reads as if they
    // counted views of these bytes.
    const he = version ? null : heatMap && heatMap[doc.path];
    // The Dashboard's danger verdict, on the screen the document is actually
    // read (BEA-119). It leads the line rather than trailing it because #meta
    // is nowrap + ellipsis (style.css:381) — a warning appended after the
    // author and the timestamp is the first thing a narrow window eats.
    const stale = staleNote(he || null, doc.time);
    // Guard on the raw fields, not on whoChanged's result: it answers
    // "unknown" rather than "" , and plain-folder mode (no identity at
    // all) has always printed nothing here.
    if (doc.user_name || doc.user || doc.author) {
      parts.push(whoChanged(doc) + (doc.device ? " on " + doc.device : ""));
    }
    if (doc.time) parts.push(new Date(doc.time).toLocaleString());
    // The count says what is in it, but #meta is nowrap + ellipsis
    // (style.css:381) — visible text appended here is the first thing a
    // narrow window truncates away, so the disclosure rides along as hover
    // text and a screen-reader-only span instead.
    const heat = he && heatTotal(he) ? heatText(he) + " / 30d" : "";
    const warn = stale ? (
      <span className="meta-stale" title={stale}>
        <span aria-hidden="true">⚠ </span>
        {stale}
      </span>
    ) : null;
    onMeta(
      heat ? (
        <>
          {warn}
          {warn ? " · " : ""}
          {parts.length ? parts.join(" · ") + " · " : ""}
          <span title={HEAT_DISCLOSURE}>
            {heat}
            <span className="sr-only"> — {HEAT_DISCLOSURE}</span>
          </span>
        </>
      ) : warn ? (
        <>
          {warn}
          {parts.length ? " · " + parts.join(" · ") : ""}
        </>
      ) : (
        parts.join(" · ")
      ),
    );
  }, [doc, version, heatMap, onMeta]);

  // Rendered content, and nothing else, counts as "rendered" — the scroll
  // restorer re-applies its goal on this call, and a read-count refresh
  // (heatMap, polled every 60s) used to ride along on the meta effect above
  // and yank the reader back to the top mid-read (BEA-155). html and diagrams
  // are exactly what the mounted subtree is made of; a diagram landing late
  // now reports itself too, which is the case the retries were written for.
  useEffect(() => {
    if (html) onRendered?.();
  }, [html, diagrams, onRendered]);

  if (error) return <LoadError version={version} err={error as Error} />;
  if (!doc) return null;
  // The panel is a SIBLING of the prose, not a wrapper around it: that is
  // what lets plain CSS hang it in the right margin (style.css, .page.read)
  // without a third column in AppShell or new props through Browser.
  // Server-rendered, server-sanitized markdown — same trust model as the
  // classic app assigning innerHTML.
  return (
    <>
      <SecretBadge findings={doc.findings} />
      {doc.frontmatter?.length ? <FrontmatterPanel pairs={doc.frontmatter} /> : null}
      <div
        dangerouslySetInnerHTML={{ __html: diagrams ?? html }}
        onClick={(e) => handleLinkClick(e, path, onOpenFile)}
      />
    </>
  );
}

/* A document's YAML frontmatter, beside the prose instead of on top of it.
   Native <details>, so the disclosure, its keyboard handling and its a11y
   semantics come from the element rather than from us — and values are
   ordinary React text children, which is what makes "a value containing
   markup renders as text" true by construction rather than by a rule. */
function FrontmatterPanel({ pairs }: { pairs: FrontmatterPair[] }) {
  const [open, setOpen] = useState(fmPanelOpen);
  return (
    <details className="fmpanel" open={open}>
      {/* The click, not the element's own `toggle` event: `toggle` is
          dispatched asynchronously, so collapsing the panel and immediately
          opening another file lost the preference — the navigation started
          before the handler ran. A summary click is also what Enter and
          Space produce, so the keyboard path is the same one. */}
      <summary
        onClick={(e) => {
          e.preventDefault();
          setOpen(!open);
          rememberFmPanel(!open); // every file and every reload, until changed
        }}
      >
        Properties
      </summary>
      <dl>
        {pairs.map((p) => (
          <div key={p.key}>
            <dt>{p.key}</dt>
            <dd>{p.code ? <code>{p.value}</code> : p.value}</dd>
          </div>
        ))}
      </dl>
    </details>
  );
}

/* The share gate could already name the rule and the line well enough to
   refuse to publish this file, while the file view rendered the same key as
   ordinary prose (BEA-147). Advisory only, in VersionBanner's shape: a strip
   above the content, role="status", no actions. Nothing is blocked and
   nothing is redacted — a reader who can open the file could already read
   the key, and the point is that they now know it is in there. */
function SecretBadge({ findings }: { findings?: SecretFinding[] }) {
  if (!findings?.length) return null;
  return (
    <div className="sbadge" role="status">
      <span className="sb-icon">
        <Icon name="shield" />
      </span>
      <div className="sb-text">
        <b>{secretsBadge(findings)}</b>
        <span>
          Checked when this page loaded. Sharing the file asks you to confirm first.
        </span>
      </div>
    </div>
  );
}

/* Delegated click handling for rendered-markdown links: wikilinks (already
   carrying a real in-app href, see transformHTML) and relative links route
   in-app on a plain click, everything else keeps its native behavior. */
function handleLinkClick(e: React.MouseEvent, p: string, openFile: (path: string) => void) {
  const a = (e.target as HTMLElement).closest("a");
  if (!a || !(e.currentTarget as HTMLElement).contains(a)) return;
  // Same rule as nav.ts:linkProps — a modified or non-primary click belongs
  // to the browser (new tab, new window, download), which is the entire
  // point of these anchors carrying real URLs.
  if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
  const href = a.getAttribute("href") || "";
  const dir = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
  // data-wiki, not "any root-absolute href": an author's own [x](/somewhere)
  // keeps its native behavior instead of quietly becoming an SPA route, and
  // the target path is read back directly instead of re-parsed out of a URL.
  const wiki = a.getAttribute("data-wiki");
  if (wiki !== null) {
    e.preventDefault();
    openFile(wiki);
  } else if (!/^([a-z]+:|\/|#)/i.test(href)) {
    e.preventDefault();
    openFile(joinPath(dir, decodeURIComponent(href)));
  }
}

/* String-level rewrite of the server's HTML: relative image sources point
   at the file API, external links open in a new tab, and wikilinks get the
   real in-app URL of their target. */
function transformHTML(
  html: string,
  p: string,
  apiBase: string,
  files: Node[],
  projectId?: string,
): string {
  const dir = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
  const fileURL = (path: string) => apiBase + "file?path=" + encodeURIComponent(path);
  const parsed = new DOMParser().parseFromString(html, "text/html");
  for (const img of parsed.querySelectorAll("img")) {
    const src = img.getAttribute("src") || "";
    // An inline SVG is a document, not a picture. goldmark's IsDangerousURL
    // admits any `data:image/…` URL for an image, and svg+xml is the one
    // member of that family a browser parses as markup — the same property
    // the server's sandboxInline walls off. Raster data: URLs stay: they are
    // inert and people do paste them into markdown.
    if (/^\s*data:image\/svg/i.test(src)) img.removeAttribute("src");
    else if (!/^([a-z]+:|\/)/i.test(src)) img.setAttribute("src", fileURL(joinPath(dir, src)));
  }
  for (const a of parsed.querySelectorAll("a")) {
    const href = a.getAttribute("href") || "";
    // The server has no file tree, so it leaves [[target]] as the marker
    // href="wiki:<target>" (markdown.go). Resolving it HERE — before the
    // mount — is what makes the anchor an ordinary link: copy-link,
    // middle-click and open-in-new-tab all read the attribute, and only a
    // plain click ever reaches the handler above.
    if (href.startsWith("wiki:")) {
      const hit = resolveWiki(decodeURIComponent(href.slice(5)), files);
      if (hit) {
        a.setAttribute("href", urlForPath(hit.path, projectId));
        a.setAttribute("data-wiki", hit.path);
      } else {
        // No file matches: an unusable "wiki:" string must not survive the
        // mount, so the anchor loses its href and says why on hover.
        a.removeAttribute("href");
        a.classList.add("wiki-missing");
        a.setAttribute("title", "No file matches this wikilink");
      }
      continue;
    }
    // goldmark's data: allowance exists for IMAGES and is applied to <a> as
    // well, so a rendered document can mount a link whose target is an
    // attacker-authored document. Browsers refuse a top-level data:
    // navigation today, which is the only reason this is defence in depth
    // rather than a live exploit — and it is not a defence this app should be
    // borrowing. A markdown link is never legitimately a data: URL.
    if (/^\s*data:/i.test(href)) a.removeAttribute("href");
    else if (/^https?:/i.test(href)) {
      a.setAttribute("target", "_blank");
      a.setAttribute("rel", "noopener");
    }
  }
  return parsed.body.innerHTML;
}

function ImgView(props: { src: string; alt: string; version?: string; onRendered?: () => void }) {
  const [failed, setFailed] = useState(false);
  if (failed) return <LoadError version={props.version} err={new Error("could not be loaded")} />;
  return (
    <img src={props.src} alt={props.alt} onLoad={props.onRendered} onError={() => setFailed(true)} />
  );
}

/* A missing current file is a server problem worth quoting; a missing
   version is almost always a bad ?v= in a hand-edited or stale URL, which
   the server's "no such version" wording does not explain. */
function LoadError({ version, err }: { version?: string; err: Error }) {
  return (
    <div className="empty">
      {version ? "That version isn't available." : "Could not load file: " + err.message}
    </div>
  );
}

function TextView(props: Parameters<typeof FileView>[0] & { fileURL: string; delim?: string }) {
  const { path, version, fileURL, delim, onRendered } = props;
  const { data, error } = useQuery({
    queryKey: ["text", fileURL],
    queryFn: async () => {
      const r = await fetch(fileURL);
      if (!r.ok) throw new Error(await r.text());
      return r.text();
    },
    retry: version ? false : undefined,
  });
  useEffect(() => {
    if (data != null) onRendered?.();
  }, [data, onRendered]);
  // null = not usefully delimited (or no delimiter asked for): fall through
  // to the plain-text view below.
  const csv = useMemo(
    () => (delim && data != null ? parseDelimited(data, delim, CSV_ROWS) : null),
    [data, delim],
  );
  if (error) return <LoadError version={version} err={error as Error} />;
  if (data == null) return null;
  if (csv) return <CsvTable csv={csv} key={path} />;
  return (
    <pre className="plain" key={path}>
      {data}
    </pre>
  );
}

/* Plain <table> — no sorting, filtering or search, so @tanstack/react-table
   would only be weight. Every row is padded to the widest one so a ragged
   row renders empty trailing cells instead of shifting its neighbours. */
function CsvTable({ csv }: { csv: Csv }) {
  const [head, ...body] = csv.rows;
  const cols = csv.rows.reduce((m, r) => Math.max(m, r.length), 0);
  const idx = Array.from({ length: cols }, (_, i) => i);
  return (
    <>
      <div className="csvbox">
        <table className="csvview">
          <thead>
            <tr>
              {idx.map((i) => (
                <th key={i}>{head[i] ?? ""}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {body.map((r, i) => (
              <tr key={i}>
                {idx.map((j) => (
                  <td key={j}>{r[j] ?? ""}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {csv.truncated > 0 && (
        <p className="csvnote">
          showing {csv.rows.length.toLocaleString()} of{" "}
          {(csv.rows.length + csv.truncated).toLocaleString()} rows — Download for the rest
        </p>
      )}
    </>
  );
}
