import { useEffect } from "react";
import type { HeatMap, Node } from "../api/types";
import { heatFor, heatLevel, heatText, useFolderHistory } from "../hooks/useBrowse";
import { parseConflict } from "../lib/conflict";
import { HEAT_DISCLOSURE, staleNote } from "../lib/heat";
import { humanSize } from "../util";
import { Icon } from "./shell";
import { HistoryRow } from "./HistoryRow";

export function FolderListing(props: {
  node: Node;
  heatMap: HeatMap | null;
  hub: boolean; // hub feeds exist; a plain-folder viewer has no journals
  apiBase: string;
  onOpen: (path: string, version?: string) => void;
  onFullHistory: (prefix: string) => void;
  onRendered?: () => void; // scroll restoration: content height just grew
}) {
  const { node, heatMap, onOpen } = props;
  const kids = (node.children || [])
    .slice()
    .sort((a, b) => Number(b.dir || false) - Number(a.dir || false) || a.name.localeCompare(b.name));
  const dirs = kids.filter((c) => c.dir).length;
  const files = kids.length - dirs;
  const counts: string[] = [];
  if (dirs) counts.push(dirs + (dirs === 1 ? " folder" : " folders"));
  if (files) counts.push(files + (files === 1 ? " file" : " files"));
  const folderHeat = heatFor(heatMap, node.path, true);
  if (folderHeat) counts.push(heatText(folderHeat) + " in 30 days");
  // One sentence per page covers the summary count and every row count at
  // once; a full disclosure repeated down fifty dense rows would not fit and
  // would not be read. The rows keep it on the dot for anyone who meets a row
  // on its own (hover, screen reader).
  const anyHeat = !!folderHeat || kids.some((c) => heatFor(heatMap, c.path, !!c.dir));

  return (
    <div className="dirlist">
      <h1 className="dl-title">
        <span className="dl-title-icon">
          <Icon name="folder" />
        </span>
        <span>{node.name}</span>
      </h1>
      <p className="dl-sub">{counts.join(" · ") || "Empty folder"}</p>
      {anyHeat && <p className="dl-heatnote">{HEAT_DISCLOSURE}</p>}
      {kids.length === 0 ? (
        <div className="dl-empty">Nothing in this folder yet.</div>
      ) : (
        <div className="dl-items">
          {kids.map((c) => {
            let meta = "";
            if (c.dir) {
              const n = (c.children || []).length;
              meta = n + (n === 1 ? " item" : " items");
            } else {
              meta = [c.size ? humanSize(c.size) : "", c.time ? new Date(c.time).toLocaleDateString() : ""]
                .filter(Boolean)
                .join(" · ");
            }
            const he = heatFor(heatMap, c.path, !!c.dir);
            if (he) meta = heatText(he) + (meta ? " · " + meta : "");
            const conflict = c.dir ? null : parseConflict(c.path);
            // Files only: a folder's heat is a subtree sum and it has no one
            // mtime to be stale against, which is also why the Dashboard
            // plots files only (BEA-119).
            const stale = c.dir ? "" : staleNote(he, c.time);
            return (
              <div
                key={c.path}
                className="dl-row"
                tabIndex={0}
                role="button"
                title={c.path}
                onClick={() => onOpen(c.path)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    onOpen(c.path);
                  }
                }}
              >
                <span className="ticon">
                  <Icon name={c.dir ? "folder" : "doc"} />
                </span>
                <span className="dl-name">{c.name}</span>
                {conflict && (
                  /* The one thing a strangely-named file needs at a glance:
                     that beardrive put it there on purpose. The page itself
                     explains what it is — same reasoning as the heat dot
                     below, the label has to travel without hover. */
                  <span
                    className="dl-conflict"
                    aria-label={"Conflict copy: a concurrent edit from " + (conflict.device || "another device") + " that beardrive preserved instead of dropping."}
                    title={"A concurrent edit from " + (conflict.device || "another device") + " that beardrive preserved instead of dropping."}
                  >
                    conflict copy
                  </span>
                )}
                {stale && (
                  /* Same reasoning as the dot below: the glyph carries a real
                     aria-label, because title= needs a hover that touch and
                     screen readers never give. */
                  <span
                    className="stalemark"
                    role="img"
                    aria-label={"Warning: " + stale}
                    title={"Read often, but " + stale}
                  >
                    ⚠
                  </span>
                )}
                {he && (
                  /* title= needs hover, which touch never gives and screen
                     readers never see — the dot carries its own name. */
                  <span
                    className={"heatdot lvl" + heatLevel(he)}
                    role="img"
                    aria-label={heatText(he) + " in 30 days. " + HEAT_DISCLOSURE}
                    title={heatText(he) + " in 30 days. " + HEAT_DISCLOSURE}
                  />
                )}
                <span className="dl-meta">{meta}</span>
              </div>
            );
          })}
        </div>
      )}
      {props.hub && (
        <FolderHistory
          apiBase={props.apiBase}
          prefix={node.path + "/"}
          onOpen={onOpen}
          onFullHistory={() => props.onFullHistory(node.path + "/")}
          onRendered={props.onRendered}
        />
      )}
    </div>
  );
}

/* The folder's change feed, straight from the journals: files added,
   edited, and deleted anywhere under it, newest first. */
function FolderHistory(props: {
  apiBase: string;
  prefix: string;
  onOpen: (path: string, version?: string) => void;
  onFullHistory: () => void;
  onRendered?: () => void;
}) {
  const entries = useFolderHistory(props.apiBase, props.prefix, true);
  const { onRendered } = props;
  useEffect(() => {
    // The feed adds height after the listing rendered; a restored scroll
    // position (back/forward) may only fit now.
    if (entries && entries.length && onRendered) onRendered();
  }, [entries, onRendered]);
  if (!entries || entries.length === 0) return null;
  return (
    <div className="dl-history">
      <h3 className="dl-h3">Recent changes</h3>
      <div className="history dl-hlist">
        {entries.map((e, i) => (
          <HistoryRow key={i} entry={e} apiBase={props.apiBase} onOpen={props.onOpen} />
        ))}
      </div>
      <button className="ai-btn dl-more" onClick={props.onFullHistory}>
        Full history
      </button>
    </div>
  );
}
