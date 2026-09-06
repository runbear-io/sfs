import { useEffect, useMemo, useRef } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import type { Node } from "../api/types";
import { Icon, closeSidebarOnMobile } from "./shell";

// The sidebar file tree, virtualized: only the visible window of rows is in
// the DOM (collapsed subtrees aren't rendered at all), so 10k-file projects
// scroll smoothly. The chevron only folds; the row selects (opens a
// folder's listing / a file). Clicking the folder whose listing is already
// showing folds/unfolds it, like a plain tree.

interface FlatRow {
  node: Node;
  depth: number;
}

function flatten(root: Node | undefined, expanded: Set<string>): FlatRow[] {
  const out: FlatRow[] = [];
  const walk = (nodes: Node[], depth: number) => {
    for (const n of nodes) {
      out.push({ node: n, depth });
      if (n.dir && expanded.has(n.path)) walk(n.children || [], depth + 1);
    }
  };
  walk(root?.children || [], 0);
  return out;
}

export function FileTree(props: {
  root: Node | undefined;
  expanded: Set<string>;
  onToggle: (path: string) => void;
  currentPath: string;
  listingShowing: boolean; // current view is a folder listing
  /* Prefixes of folders carrying an access rule. The tree is the ONLY place a
     top-level folder appears — the project root renders the connect guide and
     the dashboard, not a listing — so without a mark here a restricted
     top-level folder has nowhere to announce itself. A glyph, not the listing's
     text pill: these rows are 28px and already carry chevron, guides, icon and
     label. */
  restricted: Set<string>;
  onOpen: (path: string) => void;
}) {
  const { root, expanded, onToggle, currentPath, listingShowing, restricted, onOpen } = props;
  const scrollRef = useRef<HTMLElement>(null);

  const rows = useMemo(() => flatten(root, expanded), [root, expanded]);

  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => (window.matchMedia("(max-width: 768px)").matches ? 44 : 28),
    overscan: 12,
    getItemKey: (i) => rows[i].node.path,
  });

  // Keep the subject visible: when the selection changes (tree click, deep
  // link, scoped view), scroll its row into the window.
  useEffect(() => {
    if (!currentPath) return;
    const i = rows.findIndex((r) => r.node.path === currentPath);
    if (i >= 0) virtualizer.scrollToIndex(i, { align: "auto" });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [currentPath, rows]);

  return (
    <nav id="tree" aria-label="Files" ref={scrollRef}>
      <div style={{ height: virtualizer.getTotalSize(), position: "relative" }}>
        {virtualizer.getVirtualItems().map((vi) => {
          const { node: n, depth } = rows[vi.index];
          const open = n.dir ? expanded.has(n.path) : false;
          const click = () => {
            if (n.dir && currentPath === n.path && listingShowing) {
              // Folding beats re-opening when this folder's listing is up.
              onToggle(n.path);
              return;
            }
            onOpen(n.path);
            if (!n.dir) closeSidebarOnMobile();
          };
          return (
            <div
              key={vi.key}
              className={"row " + (n.dir ? "dir" : "file") + (currentPath === n.path ? " active" : "") + (n.dir && !open ? " collapsed" : "")}
              data-path={n.path}
              tabIndex={0}
              role="button"
              title={n.name}
              aria-expanded={n.dir ? open : undefined}
              style={{
                position: "absolute",
                top: 0,
                left: 0,
                right: 0,
                transform: `translateY(${vi.start}px)`,
                paddingLeft: 8 + depth * 13,
              }}
              onClick={click}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  click();
                }
              }}
            >
              {/* nesting guide lines, matching the old ul borders */}
              {Array.from({ length: depth }, (_, i) => (
                <span key={i} className="tguide" style={{ left: 8 + i * 13 + 5 }} aria-hidden="true" />
              ))}
              <span
                className="chev"
                onClick={(e) => {
                  if (!n.dir) return;
                  e.stopPropagation();
                  onToggle(n.path);
                }}
              >
                <Icon name="chevd" />
              </span>
              <span className="ticon">
                <Icon name={n.dir ? "folder" : "doc"} />
              </span>
              <span className="label">{n.name}</span>
              {n.dir && restricted.has(n.path) && (
                <span
                  className="trestricted"
                  role="img"
                  aria-label={n.name + " is a restricted folder"}
                  title="Restricted — not everyone in the workspace has the same access here"
                >
                  <Icon name="lock" />
                </span>
              )}
            </div>
          );
        })}
      </div>
    </nav>
  );
}

/* Every ancestor folder of a path (for unfolding the way to it). */
export function ancestorsOf(filePath: string): string[] {
  const parts = filePath.split("/");
  const out: string[] = [];
  let acc = "";
  for (let i = 0; i < parts.length - 1; i++) {
    acc = acc ? acc + "/" + parts[i] : parts[i];
    out.push(acc);
  }
  return out;
}
