import { useMemo, useState } from "react";
import {
  createColumnHelper,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type SortingState,
} from "@tanstack/react-table";
import { api } from "../api/http";
import type { ShareInfo } from "../api/types";
import { modalConfirm } from "../modal";
import { toast } from "../toast";
import { linkProps } from "../nav";
import { urlForPath } from "../router";
import { copyText } from "../util";
import { AdminTable } from "./AdminTable";
import { Icon } from "./shell";

/* The one live-public-links table in the product. The org panel passes
   org-wide rows (showProject) as the cross-project audit; a project's own
   Settings passes just its rows. Revoking is DELETE /api/shares/{token}
   either way — the server does its own PermWrite check, so canRevoke only
   decides whether to offer the control. */

// The one place a share's lifetime gets worded — the audit row and the share
// dialog must not drift on what "no expiry" looks like.
export function expiryLabel(expires?: string): string {
  return expires ? "expires " + new Date(expires).toLocaleDateString() : "no expiry";
}

// The receipt: did anyone actually open this? The server already recorded
// every /s/<token> hit — this is the only place the number gets read back.
// `undefined` means the hub has read telemetry off, and then we say nothing
// at all; `0` is a real answer and gets one. Hence the explicit undefined
// check — `!s.opens` would collapse "not measured" into "never opened".
function opensLabel(s: ShareInfo): string | null {
  if (s.opens === undefined) return null;
  if (s.opens === 0) return "not opened yet";
  const n = `${s.opens} open${s.opens === 1 ? "" : "s"}`;
  return s.last_opened ? `${n} · last opened ${new Date(s.last_opened).toLocaleDateString()}` : n;
}

export function shareDetail(s: ShareInfo, showProject: boolean): string {
  const bits: string[] = [];
  if (showProject && s.project_name) bits.push(s.project_name);
  if (s.creator) bits.push("by " + s.creator);
  if (s.created) bits.push(new Date(s.created).toLocaleDateString());
  bits.push(expiryLabel(s.expires));
  const opens = opensLabel(s);
  if (opens) bits.push(opens);
  return bits.join(" · ");
}

// The honesties about the number, worded once per section rather than once
// per row: opens are debounced VISITS (readDebounce, reads.go) keyed by
// browser+network rather than by person (shareActor, shares.go) — so the note
// states the residual instead of promising precision the signal lacks — and
// the count is per FILE not per link (heat is keyed by path), so two tokens
// on one file report the same number.
export const OPENS_NOTE =
  "Opens count how many times a file has been read through a public link. " +
  "Repeat opens from the same browser and network within 10 minutes count once — " +
  "two people on one network using the same browser still count as one.";

export function SharesTable({
  shares,
  onChanged,
  showProject = false,
  canRevoke = true,
  empty = "No public shares.",
  loading = false,
}: {
  shares: ShareInfo[];
  onChanged: () => void;
  showProject?: boolean;
  canRevoke?: boolean;
  empty?: string;
  // An empty array is not a settled answer while the request is in flight, and
  // "nothing is public" is the one wrong answer nobody should read for a frame.
  loading?: boolean;
}) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const col = useMemo(() => createColumnHelper<ShareInfo>(), []);
  const columns = useMemo(
    () => [
      col.accessor("path", {
        header: "Path",
        // Links to the file in the hub, not to /s/<token>: from an audit row
        // the question is always "what is this?", and the public URL is one
        // click away on the file itself.
        cell: (c) => (
          <a
            className="ai-main mono"
            title={c.getValue()}
            {...linkProps(urlForPath(c.getValue(), c.row.original.project))}
          >
            {c.getValue()}
          </a>
        ),
      }),
      col.accessor((s) => shareDetail(s, showProject), {
        id: "detail",
        header: showProject ? "Project" : "Shared",
        cell: (c) => <span className="ai-tag">{c.getValue()}</span>,
      }),
      col.display({
        id: "actions",
        header: "",
        // Copy is offered to readers too, not just to whoever may revoke:
        // ShareBanner already prints the whole /s/<token> URL as text to
        // anyone with read (BEA-69), so gating the clipboard shortcut here
        // would only make Settings stricter than the file page.
        cell: (c) => (
          <span className="share-acts">
            <button
              className="ai-btn"
              aria-label={`Copy the public link to ${c.row.original.path}`}
              title="Copy link"
              onClick={() =>
                // The server composed this URL through requestBaseURL, which is
                // what makes it right behind a proxy — never rebuild it here
                // out of origin + token.
                copyText(c.row.original.url).then((ok) =>
                  toast(ok ? "Copied." : "Select and copy the link."),
                )
              }
            >
              <Icon name="copy" />
            </button>
            {canRevoke && (
              <button
                className="ai-del"
                aria-label={`Revoke the share of ${c.row.original.path}`}
                onClick={() => revokeShare(c.row.original, onChanged)}
              >
                Revoke
              </button>
            )}
          </span>
        ),
      }),
    ],
    [col, onChanged, showProject, canRevoke],
  );

  const table = useReactTable({
    data: shares,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  });

  // Same shell and metrics as the empty state, so the section is one row tall
  // either way and doesn't jump when the data lands.
  if (loading)
    return (
      <div className="admin-list">
        <div className="admin-empty">Loading…</div>
      </div>
    );

  if (shares.length === 0)
    return (
      <div className="admin-list">
        <div className="admin-empty">{empty}</div>
      </div>
    );

  return <AdminTable table={table} className="shares-table" />;
}

// Shared by the table and the file-page banner so "revoke" means the same
// thing (confirm, delete, tell the caller to refetch) wherever it is offered.
export async function revokeShare(sh: ShareInfo, onChanged: () => void) {
  if (
    !(await modalConfirm(
      "Revoke share link",
      `Revoke the public link to “${sh.path}”? Anyone with the URL will lose access.`,
      "Revoke",
      true,
    ))
  )
    return;
  try {
    await api("DELETE", "/api/shares/" + sh.token);
    toast("Share revoked.");
    onChanged();
  } catch (e) {
    toast((e as Error).message, true);
  }
}
