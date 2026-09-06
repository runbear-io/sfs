import { useEffect, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";

/* Live change notification.
   The hub streams "these paths changed" over server-sent events, so a
   teammate's edit lands in about a second instead of on the tree's 15s poll —
   and an OPEN file updates at all, which it previously never did: file
   content has no refetch interval (see useBlob), so a body fetched once
   stayed on screen until the reader navigated away.

   The poll stays exactly where it is. This is an accelerator on top of it:
   EventSource reconnects on its own, but a proxy that buffers the stream, or
   a hub too old to serve the route, simply leaves the poll doing what it
   always did. Nothing here is load-bearing. */

type ChangeEvent = {
  type: "change" | "resync" | "presence";
  paths?: string[];
  more?: boolean;
  people?: { name: string; path?: string }[];
};

export function useProjectEvents(
  apiBase: string,
  enabled = true,
  // Presence rides the same stream rather than a second connection: it is the
  // same fan-out, the same permission, and one EventSource per project is
  // already one more than zero.
  onPresence?: (people: { name: string; path?: string }[]) => void,
) {
  const qc = useQueryClient();
  // Read through a ref so a caller passing an inline arrow does not tear the
  // stream down and rebuild it on every render.
  const onPresenceRef = useRef(onPresence);
  onPresenceRef.current = onPresence;

  useEffect(() => {
    if (!enabled || typeof EventSource === "undefined") return;
    const es = new EventSource(apiBase + "events");

    // The tree gains and loses entries on any change; heat and history are
    // derived from the same journal, so they go stale at the same moment.
    const invalidateProject = () => {
      qc.invalidateQueries({ queryKey: ["tree", apiBase] });
      qc.invalidateQueries({ queryKey: ["history", apiBase] });
      qc.invalidateQueries({ queryKey: ["heat", apiBase] });
    };

    es.onmessage = (e) => {
      let ev: ChangeEvent;
      try {
        ev = JSON.parse(e.data);
      } catch {
        return; // a frame we can't read is not a reason to tear the stream down
      }
      // Presence is not a file change: it invalidates nothing.
      if (ev.type === "presence") {
        onPresenceRef.current?.(ev.people ?? []);
        return;
      }
      // An open editor must know a peer wrote its file, but must NOT be
      // re-seeded from the server: that would reset the buffer under the
      // typist's cursor. It listens for this and shows a banner instead.
      // A DOM event rather than a prop chain — the editor is several levels
      // down and this is the only thing it needs from up here.
      window.dispatchEvent(
        new CustomEvent("bdrive:changed", { detail: ev.paths ?? [] }),
      );
      invalidateProject();
      // "resync" means frames were dropped and what was missed is unknowable,
      // and "more" means the frame was truncated. Either way the only honest
      // move is to drop every cached body rather than guess at a path list.
      if (ev.type === "resync" || ev.more || !ev.paths?.length) {
        qc.invalidateQueries({ queryKey: ["render", apiBase] });
        qc.invalidateQueries({ queryKey: ["text"] });
        return;
      }
      for (const p of ev.paths) {
        qc.invalidateQueries({ queryKey: ["render", apiBase, p] });
      }
      // Blob queries are keyed by content hash when the caller pinned one
      // (those can never go stale) and by path otherwise — the live ones are
      // exactly what a peer's write invalidates.
      qc.invalidateQueries({ queryKey: ["text"] });
    };

    // Errors are expected and self-healing: EventSource retries on its own,
    // and the poll covers the gap. Logging here would mean a line per hub
    // restart, per sleeping laptop, forever.
    es.onerror = () => {};

    return () => es.close();
  }, [apiBase, enabled, qc]);
}
