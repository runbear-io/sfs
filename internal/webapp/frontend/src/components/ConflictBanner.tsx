import type { Conflict } from "../lib/conflict";
import { Icon } from "./shell";

/* A conflict copy is an ordinary file with an alarming name, and the README
   is the only place that ever explained it. This banner is what makes the
   guarantee visible where a user actually meets the file: two devices edited
   at once, nothing was dropped, and the other version is one click away.

   Pure presentation over a path the caller already parsed — no request, no
   server flag; the name carries everything (see lib/conflict.ts). Reuses
   VersionBanner's .vbanner styles: same shape, same job. */
export function ConflictBanner(props: {
  conflict: Conflict;
  /* The recovered original only when the project actually holds it:
     conflictName truncates a long base name before appending the suffix, so
     the name we recover can be one that never existed. The explanation is
     the point; the link is the bonus. */
  originalHref?: () => void;
}) {
  const { conflict, originalHref } = props;
  const who = conflict.device || "another device";
  return (
    <div className="vbanner" role="status">
      <span className="vb-icon">
        <Icon name="alert" />
      </span>
      <div className="vb-text">
        <b>Conflict copy — a concurrent edit, preserved</b>
        <span>
          {who} edited this file at the same time as someone else on{" "}
          {conflict.when.toLocaleString()}. Rather than drop either version,
          beardrive kept that one here.{" "}
          {originalHref ? (
            <>The other version lives at <code>{conflict.original}</code></>
          ) : (
            <>The other version kept the original name.</>
          )}
        </span>
      </div>
      {originalHref && (
        <div className="vb-actions">
          <button className="ai-btn" onClick={originalHref}>
            Open the other version
          </button>
        </div>
      )}
    </div>
  );
}
