import { useEffect, useRef, useState } from "react";
import { postJSON } from "../api/http";

/* Who else is looking at this project.

   A heartbeat every 10s says which path you are on; the hub keeps that for 15s
   and fans the roster out on the change stream. Nothing is persisted, so a
   dropped tab simply stops mattering — and the roster the hub sends carries
   display names and paths only, never the account it keys them by.

   The roster ARRIVES on the SSE stream (see useProjectEvents, which routes
   "presence" frames here through onRoster). The POST's own response is used
   only for the first paint, so a tab that opens into a quiet project still
   sees who is there without waiting for someone else to move. */

export type Person = { name: string; path?: string };

const BEAT_MS = 10_000;

export function usePresence(apiBase: string, path: string, enabled = true) {
  const [people, setPeople] = useState<Person[]>([]);
  // The latest path, read by the interval without making it a dependency —
  // otherwise every navigation would tear down and rebuild the timer.
  const pathRef = useRef(path);
  pathRef.current = path;

  useEffect(() => {
    if (!enabled) return;
    let live = true;
    const beat = async (leave = false) => {
      try {
        const out = await postJSON<{ people: Person[] }>(apiBase + "presence", {
          path: pathRef.current,
          ...(leave ? { leave: true } : {}),
        });
        if (live && !leave) setPeople(out.people ?? []);
      } catch {
        // A hub too old to know the route, or a blip. Presence is decoration:
        // it must never surface an error or retry loudly.
      }
    };
    beat();
    const id = setInterval(beat, BEAT_MS);
    return () => {
      live = false;
      clearInterval(id);
      // Say goodbye so teammates lose you now rather than in 15s. Fire and
      // forget — the TTL is the real guarantee, this is just courtesy.
      void beat(true);
    };
  }, [apiBase, enabled]);

  return { people, setPeople };
}
