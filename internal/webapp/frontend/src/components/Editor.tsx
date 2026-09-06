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
import { putText } from "../api/http";

/* Editing a text file in the browser.

   This reverses a stated product decision — the web app was a read/share/
   history surface and content entered only through local sync (Browser.tsx).
   The server side needed nothing new: upload/content has existed, PermWrite
   and quota-checked, with no caller.

   Saving is debounced autosave, not a Save button, because that is what makes
   the change stream worth having: a teammate watching the file sees it fill in
   while you type rather than in one lump when you remember to press a key.

   What this is NOT is co-editing. Two people typing into the same file still
   resolve last-writer-wins and one of them gets a conflict copy — the same as
   two laptops editing between syncs. The CRDT layer is what fixes that; this
   is the surface it will attach to. */

// Idle time before a save. Short enough that a watcher sees you type, long
// enough that a burst of keystrokes is one journal op rather than forty.
const SAVE_IDLE_MS = 600;

export type SaveState = "clean" | "dirty" | "saving" | "error";

export function Editor({
  apiBase,
  path,
  initial,
  onSaved,
  onStateChange,
}: {
  apiBase: string;
  path: string;
  initial: string;
  onSaved?: (text: string) => void;
  onStateChange?: (s: SaveState) => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // The text of the last save that succeeded, so an unmount or a re-save can
  // tell "nothing changed" from "changed back to what it was".
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
  const cb = useRef({ onSaved, onStateChange });
  cb.current = { onSaved, onStateChange };
  const seed = useRef(initial);

  useEffect(() => {
    if (!host.current) return;
    const setState = (s: SaveState) => cb.current.onStateChange?.(s);

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
        // Keep the buffer: the user's text is the thing that matters, and the
        // next keystroke schedules another attempt.
        setState("error");
      }
    };

    const v = new EditorView({
      parent: host.current,
      state: EditorState.create({
        doc: seed.current,
        extensions: [
          lineNumbers(),
          highlightActiveLine(),
          history(),
          markdown(),
          syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
          keymap.of([...defaultKeymap, ...historyKeymap, indentWithTab]),
          EditorView.lineWrapping,
          EditorView.updateListener.of((u) => {
            if (!u.docChanged) return;
            setState("dirty");
            if (timer.current) clearTimeout(timer.current);
            timer.current = setTimeout(
              () => save(u.state.doc.toString()),
              SAVE_IDLE_MS,
            );
          }),
        ],
      }),
    });
    view.current = v;
    v.focus();

    return () => {
      // Closing the tab mid-word must not lose the word: flush whatever is in
      // the buffer instead of waiting out the debounce.
      if (timer.current) clearTimeout(timer.current);
      const text = v.state.doc.toString();
      if (text !== saved.current) void save(text);
      v.destroy();
      view.current = null;
    };
    // The document, and nothing else. Opening another file rebuilds the
    // editor (correct); a re-render must not.
  }, [apiBase, path]);

  return <div ref={host} id="editor" className="cm-host" />;
}
