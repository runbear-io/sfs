import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HeatMap, Node } from "../api/types";
import { heatTotal, hotPathSplit } from "../hooks/useBrowse";
import {
  HEAT_DISCLOSURE,
  HOT_READS,
  STALE_DAYS,
  ageRange,
  ageSpanLabel,
  isDanger,
  isFlatRange,
  orphanPaths,
  placeLabels,
} from "../lib/heat";
import { linkProps } from "../nav";

/* ---- the project Dashboard: the read×write matrix ----
   Every file plotted by how much it is read (30 days, from the heat API)
   against how long since it last changed (from the tree). The hot-but-stale
   quadrant is the danger zone: knowledge people still rely on that nobody
   maintains. Every project member sees this — /heat is gated on membership
   and returns counts only, never actor identities (reads.go). */

interface DeviceHeat {
  id?: string;
  name?: string;
  folders?: Record<string, number>;
}

// /heat?by=device breakdown; older servers lack it — the coverage section
// simply doesn't render.
export function useInsightsDevices(apiBase: string, enabled: boolean) {
  const q = useQuery({
    queryKey: ["heatDevices", apiBase],
    queryFn: () =>
      getJSON<{ devices: DeviceHeat[] }>(apiBase + "heat?by=device&days=30"),
    enabled,
    retry: false,
    staleTime: 60_000,
  });
  return q.data?.devices ?? null;
}

interface Pt {
  path: string;
  reads: number;
  // The three readers, stored rather than derived: the bar paints each from
  // its own count (hotPathSplit), so share reads never land in human.
  agent: number;
  human: number;
  share: number;
  total: number;
  days: number;
  danger: boolean;
  // A heat row whose path is no longer in the tree: it has reads but no
  // mtime, so it is ranked but never plotted (any position would be invented).
  orphan?: boolean;
}

type Lens = "all" | "human" | "agent" | "share";

const LENS_ORDER = ["all", "human", "agent", "share"] as const;
const LENS_LABEL: Record<Lens, string> = {
  all: "All reads",
  human: "Human reads",
  agent: "Agent reads",
  share: "Shared reads",
};

/* A pure lens paints one color by definition — its own. Without a share
   entry the bar would fall back to the real split and show share-only reads
   in somebody else's color. */
const PURE: Partial<Record<Lens, { agent: number; human: number; share: number }>> = {
  agent: { agent: 1, human: 0, share: 0 },
  human: { agent: 0, human: 1, share: 0 },
  share: { agent: 0, human: 0, share: 1 },
};

export function Insights(props: {
  flatFiles: Node[];
  heatMap: HeatMap | null;
  devices: DeviceHeat[] | null;
  scope?: string; // "" = whole project; a folder scopes to its subtree, a file to itself
  loading?: boolean; // tree not in yet — "this project has no files" would be a lie for a frame
  installHref?: string; // omitted on the project home, which already leads with ConnectGuide
  onOpenFile: (path: string) => void;
  onOpenFolder: (path: string) => void;
  onOpenHistory: (path: string) => void;
  isFolder: (path: string) => boolean;
}) {
  const [lens, setLens] = useState<Lens>("all");
  const { flatFiles, heatMap, devices, scope } = props;

  const inScope = (p: string) => !scope || p === scope || p.startsWith(scope + "/");
  const scoped = scope ? flatFiles.filter((f) => inScope(f.path)) : flatFiles;

  /* Two zero states, only one of them broken. No files at all draws ~840px of
     empty bordered frames with the quadrant labels — which come from the
     HOT_READS/STALE_DAYS constants, not from data — floating over nothing.
     Files-but-no-reads is already right: Treemap pads every file to reads + 1
     so unread files keep a sliver, and HotPath says so itself. So gate on
     "no files", never on "no reads". BEA-49 adds orphaned heat rows to the
     tree; when it lands they join this condition. */
  if (!props.loading && !scoped.length)
    return (
      <div className="insights">
        <h1 className="in-title">
          Knowledge insights{scope ? <span className="in-scope"> · {scope}</span> : null}
        </h1>
        <div className="dl-empty in-blank">
          <p>{scope ? `Nothing in ${scope} to chart yet.` : "Nothing to chart yet."}</p>
          <p>
            {scope
              ? `No files under ${scope} are syncing here yet.`
              : "This project has no files. Once a device syncs files here, the map, the reads × freshness plot and the hot path fill in on their own."}
          </p>
          {props.installHref && (
            <a className="pbtn" {...linkProps(props.installHref)}>
              Set up a device →
            </a>
          )}
        </div>
      </div>
    );

  const scopedDevices =
    devices && scope
      ? devices
          .map((d) => {
            // Object.create(null), not {}: the keys are FOLDER NAMES off a peer's
            // journal. `folders["__proto__"] = 3` on a plain object hits the
            // prototype setter, stores nothing, and Object.keys() then comes back
            // empty — so a folder named __proto__ erased the whole device from
            // this matrix. A null-prototype bag has no such keys to collide with.
            const folders: Record<string, number> = Object.create(null);
            for (const [f, n] of Object.entries(d.folders || {})) if (inScope(f)) folders[f] = n;
            return { ...d, folders };
          })
          .filter((d) => Object.keys(d.folders).length > 0)
      : devices;

  const now = Date.now();
  const pts: Pt[] = scoped.map((f) => {
    const e = (heatMap && heatMap[f.path]) || {};
    const days = f.time ? Math.max(0, (now - new Date(f.time).getTime()) / 86400000) : 0;
    const reads = lens === "all" ? heatTotal(e) : e[lens] || 0;
    return {
      path: f.path,
      reads,
      agent: e.agent || 0,
      human: e.human || 0,
      share: e.share || 0,
      total: heatTotal(e),
      days,
      danger: isDanger(reads, days),
    };
  });

  /* Reads whose file left the project. They can't be plotted — both plots
     position by freshness and a path with no tree entry has no mtime — but
     dropping them is how "the doc everyone was reading just vanished" became
     invisible, so Hot path ranks them alongside the tree and the plots carry
     a count. */
  const orphans: Pt[] = orphanPaths(heatMap, new Set(flatFiles.map((f) => f.path)))
    .filter(inScope)
    .map((path) => {
      const e = heatMap![path];
      return {
        path,
        reads: lens === "all" ? heatTotal(e) : e[lens] || 0,
        agent: e.agent || 0,
        human: e.human || 0,
        share: e.share || 0,
        total: heatTotal(e),
        days: 0,
        danger: false,
        orphan: true,
      };
    })
    // Nothing to say about a path with no reads under the current lens: Hot
    // path wouldn't list it, so the footnote must not count it either.
    .filter((p) => p.reads > 0);
  const orphanNote =
    orphans.length > 0 ? (
      <p className="in-legend in-orphan-note">
        {plural(orphans.length, "file")} with reads {orphans.length === 1 ? "is" : "are"} no
        longer in the project — see Hot path.
      </p>
    ) : null;

  return (
    <div className="insights">
      <h1 className="in-title">Knowledge insights{scope ? <span className="in-scope"> · {scope}</span> : null}</h1>
      <p className="dl-sub">
        {scope
          ? `Reads over the last 30 days × freshness, for ${scope} and everything in it. ${HEAT_DISCLOSURE}`
          : "Reads over the last 30 days × how long since each file changed. Hot but stale knowledge — read a lot, maintained by nobody — is the danger zone. " +
            HEAT_DISCLOSURE}
      </p>
      <div className="in-lens">
        {LENS_ORDER.map((l) => (
          <button
            key={l}
            className={"in-lens-btn" + (l === lens ? " active" : "")}
            onClick={() => setLens(l)}
          >
            {LENS_LABEL[l]}
          </button>
        ))}
      </div>

      <h3 className="dl-h3">Map — cell size = reads, color = freshness (scale below)</h3>
      <Treemap pts={pts} onOpenFile={props.onOpenFile} onOpenFolder={props.onOpenFolder} isFolder={props.isFolder} />
      {orphanNote}

      {/* The size caption belongs in the header, not in the plot: inside the
          <svg> it was drawn on top of the "hot + stale" quadrant label. */}
      <h3 className="dl-h3 in-h3-row">
        Reads × freshness
        <span className="in-cap">dot size = agent share of reads</span>
      </h3>
      <Scatter pts={pts} onOpenFile={props.onOpenFile} />
      {orphanNote}

      <h3 className="dl-h3">Hot path — top files by reads</h3>
      <HotPath
        pts={[...pts, ...orphans]}
        lens={lens}
        onOpenFile={props.onOpenFile}
        onOpenHistory={props.onOpenHistory}
      />

      {scopedDevices && scopedDevices.length > 0 && (
        <>
          <h3 className="dl-h3">Agent coverage — which agents read which areas</h3>
          <CoverageMatrix devices={scopedDevices} />
        </>
      )}
    </div>
  );
}

/* The fill when the scope has no usable age range: a grey, deliberately not a
   point on the freshness ramp. An all-0–3d project is "not enough range to
   rank", not "all clear" — painting the ramp's healthy green and then
   disclaiming it in the legend told the reader the opposite of the truth. */
const TM_FLAT = "rgb(150,156,164)";

/* Staleness color: fresh green → amber → red over 0..300 days. */
function staleColor(days: number): string {
  const stops = [
    [76, 195, 138],
    [232, 196, 84],
    [224, 93, 93],
  ];
  const t = Math.min(1, Math.max(0, days / 300)) * (stops.length - 1);
  const i = Math.min(stops.length - 2, Math.floor(t)),
    f = t - i;
  const c = stops[i].map((v, k) => Math.round(v + (stops[i + 1][k] - v) * f));
  return `rgb(${c[0]},${c[1]},${c[2]})`;
}

/* Squarified treemap (Bruls et al.), dependency-free: items sorted by value
   fill a rect in rows along the shorter side, keeping cells near-square. */
interface TmItem {
  value: number;
}
function squarify<T extends TmItem>(
  items: T[],
  x: number,
  y: number,
  w: number,
  h: number,
): Array<{ item: T; x: number; y: number; w: number; h: number }> {
  const total = items.reduce((s, it) => s + it.value, 0);
  if (!total || w <= 0 || h <= 0) return [];
  const rest = items
    .slice()
    .sort((a, b) => b.value - a.value)
    .map((it) => ({ it, a: (it.value / total) * w * h }));
  const worst = (row: Array<{ a: number }>, side: number) => {
    const sum = row.reduce((t, r) => t + r.a, 0);
    const d = sum / side;
    let m = 0;
    for (const r of row) {
      const l = r.a / d;
      m = Math.max(m, l / d, d / l);
    }
    return m;
  };
  const out: Array<{ item: T; x: number; y: number; w: number; h: number }> = [];
  while (rest.length) {
    const horiz = w >= h; // row = a strip along the shorter side
    const side = horiz ? h : w;
    const row = [rest.shift()!];
    while (rest.length && worst(row.concat(rest[0]), side) <= worst(row, side)) {
      row.push(rest.shift()!);
    }
    const d = row.reduce((t, r) => t + r.a, 0) / side;
    let off = 0;
    for (const r of row) {
      const l = r.a / d;
      if (horiz) out.push({ item: r.it, x, y: y + off, w: d, h: l });
      else out.push({ item: r.it, x: x + off, y, w: l, h: d });
      off += l;
    }
    if (horiz) {
      x += d;
      w -= d;
    } else {
      y += d;
      h -= d;
    }
  }
  return out;
}

const TM_HEADER = 15; // group label strip height

/* A cell label with its read count, sized to the cell. The count is part of
   the string the fit is measured against, so it can never overflow; when it
   won't fit, the label falls back to the bare name truncated as before rather
   than to a chopped-off number. Returns `fit` so callers can still drop the
   label entirely on very narrow cells. */
function tmLabel(name: string, reads: number, w: number): { label: string; fit: number } {
  const fit = Math.floor((w - 8) / 6);
  const full = `${name} · ${reads}`;
  if (full.length <= fit) return { label: full, fit };
  return { label: name.length > fit ? name.slice(0, Math.max(1, fit - 1)) + "…" : name, fit };
}

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? "" : "s"}`;

function Treemap({
  pts,
  onOpenFile,
  onOpenFolder,
  isFolder,
}: {
  pts: Pt[];
  onOpenFile: (p: string) => void;
  onOpenFolder: (p: string) => void;
  isFolder: (p: string) => boolean;
}) {
  const W = 720,
    H = 480;
  // Whether the colour says anything at all, decided once here and shared with
  // the legend below — the cells used to paint the ramp while the legend under
  // them said the colour carried no signal. `pts` is every file in scope, read
  // or not, so this is per-scope and independent of the read-type lens.
  const range = ageRange(pts.map((p) => p.days));
  const flat = !!range && isFlatRange(range.min, range.max);
  // Two levels: top-level folder groups, files within each.
  const groups = new Map<string, { name: string; files: Pt[]; value: number; reads: number }>();
  for (const p of pts) {
    const top = p.path.includes("/") ? p.path.split("/")[0] : "/";
    let g = groups.get(top);
    if (!g) groups.set(top, (g = { name: top, files: [], value: 0, reads: 0 }));
    g.files.push(p);
    g.value += p.reads + 1; // +1: unread files still occupy a sliver
    g.reads += p.reads; // the true total — `value` is padded, so it over-reports
  }
  const cells: React.ReactNode[] = [];
  for (const gc of squarify([...groups.values()], 0, 0, W, H)) {
    const g = gc.item;
    const dir = g.name === "/" ? "" : g.name;
    const gname = g.name === "/" ? "(root)" : g.name;
    cells.push(
      <rect
        key={"g" + g.name}
        x={gc.x + 1}
        y={gc.y + 1}
        width={Math.max(0, gc.w - 2)}
        height={Math.max(0, gc.h - 2)}
        rx={3}
        className="in-tm-group"
        data-dir={dir}
      >
        {/* So a group whose cells are all too small to label is still
            identifiable on hover. */}
        <title>
          {`${g.name === "/" ? "(root)" : g.name + "/"} — ${plural(g.reads, "read")}/30d · ${plural(g.files.length, "file")}`}
        </title>
      </rect>,
    );
    if (gc.w > 46 && gc.h > TM_HEADER + 10) {
      const { label } = tmLabel(gname, g.reads, gc.w);
      cells.push(
        <text key={"gl" + g.name} x={gc.x + 5} y={gc.y + 12} className="in-tm-glabel" data-dir={dir}>
          {label}
        </text>,
      );
    }
    const fcells = squarify(
      g.files.map((f) => ({ ...f, name: f.path.split("/").pop()!, value: f.reads + 1 })),
      gc.x + 2,
      gc.y + TM_HEADER,
      Math.max(0, gc.w - 4),
      Math.max(0, gc.h - TM_HEADER - 2),
    );
    for (const c of fcells) {
      cells.push(
        <rect
          key={c.item.path}
          x={c.x + 0.6}
          y={c.y + 0.6}
          width={Math.max(0.4, c.w - 1.2)}
          height={Math.max(0.4, c.h - 1.2)}
          rx={1.5}
          fill={flat ? TM_FLAT : staleColor(c.item.days)}
          className="in-tm-cell"
          data-path={c.item.path}
        >
          <title>
            {`${c.item.path} — ${c.item.reads} read${c.item.reads === 1 ? "" : "s"}/30d · changed ${Math.round(c.item.days)}d ago`}
          </title>
        </rect>,
      );
      if (c.w > 54 && c.h > 16) {
        const { label, fit } = tmLabel((c.item.danger ? "⚠ " : "") + c.item.name, c.item.reads, c.w);
        if (fit >= 5)
          cells.push(
            <text key={"l" + c.item.path} x={c.x + 4.5} y={c.y + 12.5} className="in-tm-label" data-path={c.item.path}>
              {label}
            </text>,
          );
      }
    }
  }
  return (
    <>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        className="in-chart in-treemap"
        onClick={(e) => {
          // One delegated click handler for thousands of cells.
          const t = (e.target as Element).closest("[data-path], [data-dir]");
          if (!t) return;
          const path = t.getAttribute("data-path");
          if (path) return onOpenFile(path);
          const dir = t.getAttribute("data-dir");
          if (dir && isFolder(dir)) onOpenFolder(dir);
        }}
      >
        {cells}
      </svg>
      <FreshnessLegend range={range} flat={flat} />
    </>
  );
}

/* The colour scale, plus the age span actually present in this scope+lens.
   On a young project the whole scope lands inside one colour stop, and saying
   so is the honest thing — the alternative, a relative scale, would paint a
   3-day-old file the red that means hot-and-stale everywhere else here. */
function FreshnessLegend({ range: r, flat }: { range: { min: number; max: number } | null; flat: boolean }) {
  if (!r) return null;
  const span = ageSpanLabel(r.min, r.max);
  return (
    <p className="in-legend in-tm-legend">
      freshness 0d
      <span
        className={"in-sw in-sw-age" + (flat ? " in-sw-flat" : "")}
        style={{ background: `linear-gradient(to right, ${[0, 60, 150, 300].map(staleColor).join(", ")})` }}
      />
      300d+
      <span className="in-tm-range">
        {flat ? `all files here: ${span} old — colour off, not enough range to rank` : `observed: ${span} old`}
      </span>
    </p>
  );
}

/* Dependency-free SVG scatter: x = days since last write, y = reads, both
   log-scaled; threshold lines split the quadrants. */
function Scatter({ pts, onOpenFile }: { pts: Pt[]; onOpenFile: (p: string) => void }) {
  const W = 720,
    H = 360,
    M = { l: 44, r: 16, t: 20, b: 34 };
  const maxDays = Math.max(STALE_DAYS * 2, ...pts.map((p) => p.days));
  const maxReads = Math.max(HOT_READS * 2, ...pts.map((p) => p.reads));
  const lx = (d: number) => Math.log10(d + 1) / Math.log10(maxDays + 1);
  const ly = (r: number) => Math.log10(r + 1) / Math.log10(maxReads + 1);
  /* Radius encodes the agent share of the file's reads. The plotted box is
     inset by the largest radius the formula can produce, so the busiest file
     — which lands at the very top of the range — sits wholly inside the frame
     instead of straddling its border. Derived, not hardcoded: a future radius
     change can't quietly reintroduce the clipping. The thresholds, the danger
     rect and the dots all read X/Y, so they move together; the axis lines use
     M directly and stay put. */
  const dotR = (share: number) => 3 + 4 * share;
  const rMax = dotR(1);
  const X = (d: number) => M.l + rMax + lx(d) * (W - M.l - M.r - 2 * rMax);
  const Y = (r: number) => H - M.b - rMax - ly(r) * (H - M.t - M.b - 2 * rMax);

  const labels = placeLabels(
    pts
      .filter((p) => p.danger)
      .map((p) => ({
        path: p.path,
        reads: p.reads,
        cx: X(p.days),
        cy: Y(p.reads),
        r: dotR(p.total ? (p.agent || 0) / p.total : 0),
      })),
    { right: W - M.r, top: M.t + 8, bottom: H - M.b - 4 },
  );

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="in-chart">
      <rect
        x={X(STALE_DAYS)}
        y={M.t}
        width={W - M.r - X(STALE_DAYS)}
        height={Y(HOT_READS) - M.t}
        className="in-danger-zone"
      />
      <line x1={X(STALE_DAYS)} y1={M.t} x2={X(STALE_DAYS)} y2={H - M.b} className="in-threshold" />
      <line x1={M.l} y1={Y(HOT_READS)} x2={W - M.r} y2={Y(HOT_READS)} className="in-threshold" />
      <line x1={M.l} y1={H - M.b} x2={W - M.r} y2={H - M.b} className="in-axis" />
      <line x1={M.l} y1={M.t} x2={M.l} y2={H - M.b} className="in-axis" />
      <text x={(M.l + W - M.r) / 2} y={H - 8} className="in-label">
        days since last change →
      </text>
      <text
        x={12}
        y={(M.t + H - M.b) / 2}
        className="in-label"
        transform={`rotate(-90 12 ${(M.t + H - M.b) / 2})`}
      >
        reads / 30d →
      </text>
      <text x={W - M.r - 6} y={M.t + 14} className="in-quad in-quad-danger" textAnchor="end">
        hot + stale
      </text>
      <text x={M.l + 6} y={M.t + 14} className="in-quad">
        hot + fresh
      </text>
      <text x={W - M.r - 6} y={H - M.b - 8} className="in-quad" textAnchor="end">
        cold + stale
      </text>
      <text x={M.l + 6} y={H - M.b - 8} className="in-quad">
        cold + fresh
      </text>
      {pts.map((p) => {
        // Translucent dots keep the cloud readable at hundreds of files.
        const share = p.total ? (p.agent || 0) / p.total : 0;
        return (
          <circle
            key={p.path}
            cx={Number(X(p.days).toFixed(1))}
            cy={Number(Y(p.reads).toFixed(1))}
            r={Number(dotR(share).toFixed(1))}
            className={"in-pt" + (p.danger ? " danger" : p.reads ? "" : " cold")}
            onClick={() => onOpenFile(p.path)}
          >
            <title>
              {`${p.path} — ${p.reads} read${p.reads === 1 ? "" : "s"} / 30d · changed ${Math.round(p.days)}d ago`}
            </title>
          </circle>
        );
      })}
      {/* The danger quadrant's story is unreadable if you have to hover dot by
          dot to learn which file is which; the tooltips stay for the rest. */}
      {labels.map((l) => (
        <text
          key={l.path}
          x={Number(l.x.toFixed(1))}
          y={Number(l.y.toFixed(1))}
          textAnchor={l.anchor}
          className="in-pt-label"
        >
          {l.name}
        </text>
      ))}
    </svg>
  );
}

/* ---- hot path: top-20 files by reads, agent/human/share split ---- */
function HotPath({
  pts,
  lens,
  onOpenFile,
  onOpenHistory,
}: {
  pts: Pt[];
  lens: Lens;
  onOpenFile: (p: string) => void;
  onOpenHistory: (p: string) => void;
}) {
  const top = pts
    .filter((p) => p.reads > 0)
    .sort((a, b) => b.reads - a.reads || b.days - a.days)
    .slice(0, 20);
  if (!top.length) return <div className="dl-empty">No reads in the window yet.</div>;
  const max = top[0].reads;
  const anyShare = top.some((p) => p.share > 0);
  return (
    <>
      <div className="in-hotpath">
        {top.map((p) => {
          // Split of the lens reads: pure lenses are single-color by definition.
          const f = PURE[lens] ?? hotPathSplit(p);
          const pct = (p.reads / max) * 100;
          // The file view would land on the not-found page for a path that
          // left the project; History still has the content.
          const open = () => (p.orphan ? onOpenHistory(p.path) : onOpenFile(p.path));
          return (
            <div
              key={p.path}
              className="in-hp-row"
              tabIndex={0}
              role="button"
              title={
                p.orphan
                  ? `${p.reads} read${p.reads === 1 ? "" : "s"}/30d · no longer in the project — open its history`
                  : p.danger
                    ? `${p.reads} read${p.reads === 1 ? "" : "s"}/30d · unchanged ${Math.round(p.days)}d — review this file`
                    : p.path
              }
              onClick={open}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  open();
                }
              }}
            >
              <span className={"in-hp-name" + (p.danger ? " danger" : "")}>
                {p.path + (p.danger ? " ⚠" : "")}
              </span>
              {p.orphan && <span className="in-hp-gone">· no longer in the project</span>}
              <span className="in-hp-bar">
                <span className="in-hp-agent" style={{ width: (pct * f.agent).toFixed(1) + "%" }} />
                <span className="in-hp-human" style={{ width: (pct * f.human).toFixed(1) + "%" }} />
                <span className="in-hp-share" style={{ width: (pct * f.share).toFixed(1) + "%" }} />
              </span>
              <span className="in-hp-count">{p.reads}</span>
            </div>
          );
        })}
      </div>
      <p className="in-legend">
        <span className="in-sw agent" /> agent reads <span className="in-sw human" /> human reads
        {anyShare && (
          <>
            {" "}
            <span className="in-sw share" /> shared reads
          </>
        )}
      </p>
    </>
  );
}

/* ---- agent coverage matrix: devices × top-level folders ---- */
function CoverageMatrix({ devices }: { devices: DeviceHeat[] }) {
  const totals = new Map<string, number>();
  for (const d of devices) {
    for (const [f, n] of Object.entries(d.folders || {})) totals.set(f, (totals.get(f) || 0) + n);
  }
  const cols = [...totals.entries()]
    .sort((a, b) => b[1] - a[1])
    .slice(0, 12)
    .map((e) => e[0]);
  const rows = devices.slice(0, 12); // server sorts by total desc
  const left = 140,
    top = 6,
    cw = Math.min(76, Math.max(34, (720 - left - 8) / cols.length)),
    ch = 26;
  const W = 720,
    H = top + rows.length * ch + 58;
  const max = Math.max(1, ...rows.flatMap((d) => cols.map((c) => (d.folders || {})[c] || 0)));
  const shade = (t: number) => {
    // #17191f → amber by intensity
    const a = [23, 25, 31],
      b = [245, 166, 35];
    const c = a.map((v, i) => Math.round(v + (b[i] - v) * t));
    return `rgb(${c[0]},${c[1]},${c[2]})`;
  };
  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="in-chart in-matrix">
      {rows.map((d, i) => {
        let label = d.name || d.id || "";
        if (label.length > 20) label = label.slice(0, 19) + "…";
        return (
          <g key={d.id || i}>
            <text x={left - 8} y={top + i * ch + 17} textAnchor="end" className="in-label">
              {label}
            </text>
            {cols.map((c, j) => {
              const v = (d.folders || {})[c] || 0;
              return (
                <rect
                  key={c}
                  x={left + j * cw}
                  y={top + i * ch}
                  width={cw - 4}
                  height={ch - 4}
                  rx={3}
                  fill={shade(Math.sqrt(v / max))}
                >
                  <title>{`${d.name || d.id} × ${c || "(root)"}: ${v} read${v === 1 ? "" : "s"}/30d`}</title>
                </rect>
              );
            })}
          </g>
        );
      })}
      {cols.map((c, j) => {
        const cx = left + j * cw + (cw - 4) / 2,
          cy = top + rows.length * ch + 14;
        return (
          <text
            key={c}
            x={cx}
            y={cy}
            className="in-label"
            textAnchor="end"
            transform={`rotate(-28 ${cx} ${cy})`}
          >
            {c || "(root)"}
          </text>
        );
      })}
    </svg>
  );
}
