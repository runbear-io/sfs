import { useEffect, useMemo, useRef, useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { HistoryEntry, Node } from "../api/types";
import { HistoryRow, NoteText, type RemoveAction, type RestoreAction } from "./HistoryRow";
import { Icon } from "./shell";
import { whoChanged } from "../util";
import { groupRuns, runFileCount, type Run } from "../lib/runs";
import { HistoryFilters, authorsOf } from "./HistoryFilters";
import { historyFilterQuery, hasHistoryFilters, type HistoryFilters as Filters } from "../router";

// Undoing a WHOLE run — the run-wide form of restore/remove, and the only
// action the card header carries. Absent when the viewer can't write, like
// its two per-row siblings, so a read-only member never sees a button that
// 403s. The card hands over the run itself, not a file list: which paths are
// reverted is worked out server-side, because this window is paged and
// filtered and a client-computed list is wrong exactly when the run is old.
export type UndoRunAction = {
  onUndoRun: (run: Run) => void;
  busy?: string; // the session (or note) currently in flight
};

/* ---- history ----
   Every change ever made, straight from the journals: who (account), when,
   from which device (name, OS, IP as the server saw it). The route stores
   one target; the tree says whether it is a folder (subtree feed) or a
   file (version list).

   One agent run is one card. A run identity is already on the wire — the
   sync hook stamps "claude-code session <id>" into the op's note — so
   grouping is a pure group-by here, with no journal or API change: same
   (note, device), one group. Note-less changes (browser uploads, plain
   daemon scans, anything from before the feature) stay bare rows, exactly
   as they were. */
export function HistoryView(props: {
  apiBase: string;
  target: string; // "" = whole project
  isFolder: (p: string) => boolean;
  // Every file the project still has, so a run card can mark a path it read
  // that has since been deleted — the same label the Dashboard uses.
  flatFiles: Node[];
  onOpen: (path: string, version?: string) => void;
  onMeta: (meta: string) => void;
  onRendered?: () => void;
  restore?: RestoreAction;
  remove?: RemoveAction;
  undoRun?: UndoRunAction;
  // Reader filters, straight from the URL. Applied server-side, so they
  // narrow the whole feed and not just the loaded page.
  filters?: Filters;
  onFilters?: (f: Filters) => void;
}) {
  const { apiBase, target, isFolder, onMeta, onRendered, restore, remove, undoRun, filters } = props;
  // One set for the whole feed, not one per card: every run card asks the
  // same question of the same tree.
  const known = useMemo(() => new Set(props.flatFiles.map((f) => f.path)), [props.flatFiles]);
  const q = !target
    ? { prefix: "" }
    : isFolder(target)
      ? { prefix: target + "/" }
      : { path: target };
  const qs =
    ("path" in q && q.path !== undefined
      ? "path=" + encodeURIComponent(q.path)
      : "prefix=" + encodeURIComponent(q.prefix ?? "")) +
    historyFilterQuery(filters).replace("?", "&");
  // Paged: the server hands back a cursor while entries remain, so a project
  // with thousands of changes is reachable to its first one. Pages accumulate
  // into one array — groupRuns and prevBlob both work over the whole window,
  // so a run straddling a page boundary becomes one card when its second page
  // lands, and the oldest loaded row shows no diff base rather than a wrong one.
  const { data, error, isPending, fetchNextPage, hasNextPage, isFetchingNextPage } = useInfiniteQuery({
    queryKey: ["history", apiBase, qs],
    queryFn: ({ pageParam }) =>
      getJSON<{ entries: HistoryEntry[]; next_cursor?: string }>(
        apiBase +
          "history?" +
          qs +
          "&n=100" +
          (pageParam ? "&cursor=" + encodeURIComponent(pageParam) : ""),
      ),
    initialPageParam: "",
    getNextPageParam: (last) => last.next_cursor,
    staleTime: 15_000,
  });

  const seenAuthors = useRef(new Set<string>());

  useEffect(() => {
    if (error) onMeta("History unavailable: " + (error as Error).message);
  }, [error, onMeta]);
  useEffect(() => {
    if (data) onRendered?.();
  }, [data, onRendered]);

  const entries = data ? data.pages.flatMap((p) => p.entries || []) : [];
  // The author list accumulates and never shrinks: filtering BY an author
  // leaves only their rows loaded, so a list rebuilt from the current feed
  // would drop every other name and strand the reader on "Anyone" as the
  // only way out.
  for (const a of authorsOf(entries)) seenAuthors.current.add(a);
  const bar = props.onFilters && (
    <HistoryFilters
      filters={filters}
      authors={[...seenAuthors.current].sort()}
      onChange={props.onFilters}
    />
  );
  // Nothing loaded yet. The filters are in the query key, so every keystroke
  // in the path box drops `data` back to undefined — returning a bare filter
  // bar here made a pending request pixel-identical to "nothing matched", and
  // a reader twice concluded a file had no history when it had three entries
  // (BEA-131). The shell always renders so the bar stays interactive, and the
  // loading row is the same `.empty` one-liner the empty state uses, so the
  // section doesn't jump when the response lands. Not while `error`: failures
  // already report through onMeta above, and a permanent spinner would hide
  // them.
  if (!data)
    return (
      <div className="history">
        {bar}
        {isPending && !error && <div className="empty">Loading…</div>}
      </div>
    );
  // Entries arrive newest-first, so a row's predecessor is the next entry
  // below it on the same path that still has content. This keeps scanning the
  // flat list, never a group: it is a per-path lookup, and grouping must not
  // change what a diff compares against.
  const prevBlob = (i: number) => {
    for (let j = i + 1; j < entries.length; j++) {
      if (entries[j].path === entries[i].path && entries[j].kind !== "delete") return entries[j].blob;
    }
  };
  // A path's current content, from the loaded window: entries are strictly
  // newest-first, so a path's FIRST occurrence decides. A newest-delete leaves
  // the path out — the file is gone, so putting its content back is a real
  // change and its rows stay restorable.
  const headBlob = new Map<string, string>();
  for (const e of entries) {
    if (headBlob.has(e.path)) continue;
    headBlob.set(e.path, e.kind === "delete" ? "" : (e.blob ?? "")); // deleted: nothing is current
  }
  // What a row restores: its own bytes, or — for a delete — the content it
  // removed, which is how a deleted file comes back. Unlike diffs, this is
  // available in every feed, so the predecessor lookup runs regardless.
  // Nothing, when those bytes ARE the file's current content: that restore
  // could only write a +0 −0 change to every device, and the server 409s it
  // (restore.go), so offering the button would only produce an error. The rule
  // is content equality, not row index — a hand-reverted older row is just as
  // much of a no-op — which is exactly what the server checks.
  const restoreSha = (i: number) => {
    const sha = entries[i].kind === "delete" ? prevBlob(i) : entries[i].blob;
    return sha && sha === headBlob.get(entries[i].path) ? undefined : sha;
  };
  // "" is the marker headBlob already uses for a path whose newest op is a
  // delete — so this restore brings the file back, which nothing in the
  // browser can walk back afterwards. The confirm copy says so (BEA-129).
  const recreates = (i: number) => headBlob.get(entries[i].path) === "";
  return (
    <div className="history">
      {bar}
      {entries.length === 0 &&
        (hasHistoryFilters(filters) ? (
          <div className="empty">
            No changes match these filters.
            <br />
            <button type="button" className="btn hf-clear-empty" onClick={() => props.onFilters?.({})}>
              Clear filters
            </button>
          </div>
        ) : (
          <div className="empty">No history yet.</div>
        ))}
      {groupRuns(entries).map((item, n) =>
        item.run ? (
          <RunGroup
            key={"g" + n}
            run={item.run}
            known={known}
            onOpen={props.onOpen}
            apiBase={apiBase}
            prevBlob={prevBlob}
            restoreSha={restoreSha}
            recreates={recreates}
            restore={restore}
            remove={remove}
            undoRun={undoRun}
          />
        ) : (
          <HistoryRow
            key={"r" + item.i}
            entry={entries[item.i]}
            apiBase={apiBase}
            onOpen={props.onOpen}
            /* Every feed, not just the per-file one (BEA-58): prevBlob is a
               per-path lookup, so a mixed-path feed diffs each row against its
               own predecessor. A review-focused reader opens the whole-project
               feed first, and finding no diff there read as "this product has
               none". */
            diff={{ apiBase, prev: prevBlob(item.i) }}
            restore={restore}
            restoreSha={restoreSha(item.i)}
            recreates={recreates(item.i)}
            /* no `remove`: an add outside a run card isn't attributable to a
               run, so the undo has nothing to claim (follow-up issue). */
          />
        ),
      )}
      {/* A button, not an IntersectionObserver: keyboard-reachable, and it
          says out loud that there is more rather than hiding it behind a
          scroll gesture. */}
      {hasNextPage && (
        <button
          type="button"
          className="btn hmore"
          onClick={() => fetchNextPage()}
          disabled={isFetchingNextPage}
        >
          {isFetchingNextPage ? "Loading…" : "Load more"}
        </button>
      )}
    </div>
  );
}

function RunGroup({
  run,
  known,
  onOpen,
  apiBase,
  prevBlob,
  restoreSha,
  recreates,
  restore,
  remove,
  undoRun,
}: {
  run: Run;
  known: Set<string>;
  onOpen: (path: string, version?: string) => void;
  apiBase: string;
  prevBlob: (i: number) => string | undefined;
  restoreSha: (i: number) => string | undefined;
  recreates: (i: number) => boolean;
  restore?: RestoreAction;
  remove?: RemoveAction;
  undoRun?: UndoRunAction;
}) {
  const [open, setOpen] = useState(true);
  const first = run.entries[0];
  const who = whoChanged(first);
  const dev = [first.device.name || first.device.id, first.device.os].filter(Boolean).join(" · ");
  // What this run READ, joined on the session id its own ops carry — never on
  // the note, which anyone can set to anything. Both the session and the
  // device are required by the server, so a card only ever shows reads its
  // own device reported.
  const sid = first.session;
  const did = first.device?.id;
  const { data: reads } = useQuery({
    queryKey: ["session-reads", apiBase, sid, did],
    queryFn: () =>
      getJSON<{ paths: string[] }>(
        apiBase + "heat?session=" + encodeURIComponent(sid!) + "&device=" + encodeURIComponent(did!),
      ),
    enabled: !!sid && !!did,
    staleTime: 30_000,
  });
  const readPaths = new Set(reads?.paths ?? []);
  const written = new Set(run.entries.map((e) => e.path));
  // Read but never written: the half of the run that History could not show
  // before, and usually the half that answers "what did it look at?".
  const readOnly = [...readPaths].filter((p) => !written.has(p)).sort();
  const times = run.entries.map((e) => new Date(e.time).getTime());
  const span = fmtSpan(Math.min(...times), Math.max(...times));
  // Distinct paths, not ops: repeat edits to one file must not inflate the
  // one number that sizes a run (BEA-39). Every op is still a row below.
  const n = runFileCount(run);
  const undoing = !!undoRun?.busy && undoRun.busy === (run.session || run.note);
  return (
    <div className={"hrun" + (open ? " open" : "")}>
      <div className="hrun-head">
        <button
          type="button"
          className="hrun-toggle"
          aria-expanded={open}
          title={open ? "Collapse this run" : "Expand this run"}
          onClick={() => setOpen(!open)}
        >
          <Icon name={open ? "chevd" : "chev"} />
        </button>
        {/* The note is a link when the agent left one — clicking it opens the
            session, so it can't live inside the collapse button. */}
        <span className="hrun-note">
          <NoteText text={run.note} />
        </span>
        <span className="hrun-meta">
          {readPaths.size > 0 ? `read ${readPaths.size} · changed ${n}` : `${n} file${n === 1 ? "" : "s"}`} ·{" "}
          {who}
          {dev ? " · " + dev : ""}
        </span>
        <span className="hrun-time">{span}</span>
        {/* The one action the header carries. Every row inside the card
            already has its own; this is the verb the card was grouped for —
            reverting a run file by file and hoping you got them all is what
            it replaces. onUndoRun confirms before anything is written. */}
        {undoRun && (
          <button
            type="button"
            className="hrun-undo"
            disabled={undoing}
            title="Put every file this run touched back the way it was"
            onClick={() => undoRun.onUndoRun(run)}
          >
            <Icon name="hist" />
            {undoing ? "undoing…" : "undo this run"}
          </button>
        )}
      </div>
      {open && (
        <div className="hrun-body">
          {run.entries.map((e, k) => (
            <HistoryRow
              key={k}
              entry={e}
              apiBase={apiBase}
              onOpen={onOpen}
              /* run.idx[k] indexes back into the flat list, so a row inside a
                 card compares against its own predecessor, not its neighbour
                 in the card. */
              diff={{ apiBase, prev: prevBlob(run.idx[k]) }}
              restore={restore}
              remove={remove}
              restoreSha={restoreSha(run.idx[k])}
              recreates={recreates(run.idx[k])}
              inRun
              read={readPaths.has(e.path)}
            />
          ))}
          {readOnly.length > 0 && (
            <div className="hrun-reads">
              <div className="hrun-reads-head">Read, not changed</div>
              {readOnly.map((p) => (
                <button key={p} type="button" className="hrun-read" onClick={() => onOpen(p)}>
                  <span className="hkind">read</span>
                  <span className="hpath">{p}</span>
                  {/* Word for word what the Dashboard says about the same
                      file (Insights.tsx) — two surfaces reading one ledger
                      must not each invent their own vocabulary for it. */}
                  {!known.has(p) && <span className="in-hp-gone">· no longer in the project</span>}
                </button>
              ))}
            </div>
          )}
          {/* Not decoration: this card is one session on one device, so its
              read count is smaller than the project's totals for the same
              files — and that gap read as a bug. Reads of a deleted file are
              kept and labelled, never dropped: the ledger records what the
              agent did, and an audit surface reports it. */}
          {sid && (
            <div className="hrun-foot">
              Reads shown are what this device reported for this session — a narrower set than the
              project's read totals.
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// "14:02 – 14:04" within a day, the full stamp when a run straddles days.
function fmtSpan(from: number, to: number): string {
  const a = new Date(from);
  const b = new Date(to);
  const t = (d: Date) => d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  if (a.toDateString() !== b.toDateString()) return a.toLocaleString() + " – " + b.toLocaleString();
  const day = b.toLocaleDateString();
  return from === to ? day + " " + t(b) : day + " " + t(a) + " – " + t(b);
}

// The crumb title for a history route target.
export function historyTitle(target: string, isFolder: (p: string) => boolean): string {
  if (!target) return "all changes";
  return isFolder(target) ? target + "/ (folder)" : target;
}
