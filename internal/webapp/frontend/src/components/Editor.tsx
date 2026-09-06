import { useEffect, useRef } from "react";
import { EditorState } from "@codemirror/state";
import {
  EditorView,
  keymap,
  lineNumbers,
  highlightActiveLine,
} from "@codemirror/view";
import {
  defaultKeymap,
  history,
  historyKeymap,
  indentWithTab,
} from "@codemirror/commands";
import { markdown } from "@codemirror/lang-markdown";
import {
  syntaxHighlighting,
  defaultHighlightStyle,
} from "@codemirror/language";
import { yCollab } from "y-codemirror.next";
import { putText } from "../api/http";
import { CollabDoc, type CollabStatus } from "../lib/collab";

/* Editing a text file in the browser, with everyone else who has it open.

   This reverses a stated product decision — the web app was a read/share/
   history surface and content entered only through local sync (Browser.tsx).
   The server side needed nothing new for the write itself: upload/content has
   existed, PermWrite and quota-checked, with no caller.

   Two layers, and the split is the whole design:

   - The DOCUMENT is a CRDT (Yjs) shared through the hub's relay. That is what
     makes two people typing in one paragraph merge instead of clobber. It
     lives only between browsers and only while they are editing.
   - The FILE is what everyone else sees, and it is unchanged: an ordinary
     blob written by an ordinary upload/content call. The journal never learns
     a CRDT exists, so desktop devices, agents and older clients converge
     exactly as before.

   Snapshotting is therefore a client's job, not the hub's. Whoever stops
   typing last writes the file; the relay's log is deliberately not durable —
   the file is. */

// Idle time before the document is written to the file.
const SAVE_IDLE_MS = 700;

export type SaveState = "clean" | "dirty" | "saving" | "error";

export function Editor({
  apiBase,
  path,
  initial,
  onSaved,
  onStateChange,
  onCollab,
  onPeers,
  me,
}: {
  apiBase: string;
  path: string;
  initial: string;
  onSaved?: (text: string) => void;
  onStateChange?: (s: SaveState) => void;
  onCollab?: (s: CollabStatus) => void;
  // Who this editor is, for the label and colour on a remote caret.
  me?: { name: string; colour: string };
  // Reports whether other editors are in the document, so the caller can
  // tell a co-editor's snapshot from an outside write.
  onPeers?: (n: number) => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const saved = useRef(initial);

  // Everything the effect needs from props goes through a ref, and the effect
  // depends on the DOCUMENT ALONE (apiBase, path). Two reasons, both of which
  // cost real bugs before they were understood:
  //
  //  - the callbacks are inline arrows at the call site, so they change
  //    identity every render, and reporting a save state re-renders. With them
  //    in the deps CodeMirror was torn down and rebuilt on every keystroke,
  //    which reset the cursor to 0 and wrote the typed text back to front.
  //  - `initial` changes whenever the seed query refetches, and a peer's write
  //    invalidates exactly that query — so it would reset the buffer under the
  //    typist's cursor, which is the one thing this component must never do.
  const cb = useRef({ onSaved, onStateChange, onCollab, onPeers });
  cb.current = { onSaved, onStateChange, onCollab, onPeers };
  const seed = useRef(initial);
  const meRef = useRef(me);
  meRef.current = me;

  useEffect(() => {
    if (!host.current) return;
    const setState = (s: SaveState) => cb.current.onStateChange?.(s);
    let view: EditorView | null = null;

    const save = async (text: string) => {
      if (text === saved.current) return;
      setState("saving");
      try {
        await putText(
          apiBase + "upload/content?path=" + encodeURIComponent(path),
          text,
        );
        saved.current = text;
        setState("clean");
        cb.current.onSaved?.(text);
      } catch {
        // Keep the document: the CRDT is the truth until a save lands, and
        // the next edit schedules another attempt.
        setState("error");
      }
    };

    // The extensions both mounts share. Only the collaborative binding and
    // the change listener differ between them.
    const baseExtensions = [
      lineNumbers(),
      highlightActiveLine(),
      history(),
      markdown(),
      syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
      keymap.of([...defaultKeymap, ...historyKeymap, indentWithTab]),
      EditorView.lineWrapping,
    ];

    // Without the CRDT there is no shared document to observe, so the save
    // timer hangs off CodeMirror's own updates instead.
    const soloListener = EditorView.updateListener.of((u) => {
      if (!u.docChanged) return;
      setState("dirty");
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(
        () => save(u.state.doc.toString()),
        SAVE_IDLE_MS,
      );
    });

    // The fallback surface: the same editor without the Yjs binding, seeded
    // from the file. Its edits still reach everyone through the ordinary save.
    const mountSolo = () => {
      if (view || !host.current) return;
      view = new EditorView({
        parent: host.current,
        state: EditorState.create({
          doc: seed.current,
          extensions: [...baseExtensions, soloListener],
        }),
      });
      view.focus();
    };

    // Mounted only once the relay has answered, so CodeMirror binds to a
    // document that already holds either the file's text or the room's —
    // never to an empty one that would then have text appear underneath it.
    const mount = () => {
      if (view || !host.current) return;
      view = new EditorView({
        parent: host.current,
        state: EditorState.create({
          doc: collab.text.toString(),
          extensions: [
            ...baseExtensions,
            // The binding: every local edit becomes a Yjs update, every
            // remote update becomes a CodeMirror transaction, and remote
            // cursors are drawn from awareness.
            yCollab(collab.text, collab.awareness),
          ],
        }),
      });
      view.focus();
    };

    const collab = new CollabDoc(
      apiBase + "collab?path=" + encodeURIComponent(path),
      seed.current,
      (s) => cb.current.onCollab?.(s),
      () => mount(),
      // No relay: an older hub, or a desktop build that does not proxy the
      // route. Editing is single-writer then — exactly what it was before
      // co-editing existed — rather than a pane that never appears.
      () => mountSolo(),
      meRef.current,
    );

    // Any change to the shared document — mine or a peer's — restarts the
    // idle timer. Whoever stops typing last writes the file, and because the
    // content is identical for everyone, a second writer is a no-op put of a
    // blob the store already has.
    const onDocChange = () => {
      setState("dirty");
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(
        () => save(collab.text.toString()),
        SAVE_IDLE_MS,
      );
    };
    collab.text.observe(onDocChange);
    const onPeerChange = () => cb.current.onPeers?.(collab.peerCount());
    collab.awareness.on("change", onPeerChange);
    collab.connect();

    return () => {
      if (timer.current) clearTimeout(timer.current);
      // Whichever surface is mounted holds the truth: the CRDT when the relay
      // is live, the view itself when it never was.
      const text = collab.text.length
        ? collab.text.toString()
        : (view?.state.doc.toString() ?? "");
      // Closing the tab mid-word must not lose the word.
      if (text && text !== saved.current) void save(text);
      collab.awareness.off("change", onPeerChange);
      collab.text.unobserve(onDocChange);
      view?.destroy();
      collab.destroy();
    };
    // The document, and nothing else. Opening another file rebuilds the
    // editor (correct); a re-render must not.
  }, [apiBase, path]);

  return <div ref={host} id="editor" className="cm-host" />;
}
