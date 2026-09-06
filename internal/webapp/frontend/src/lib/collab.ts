import * as Y from "yjs";
import {
  Awareness,
  applyAwarenessUpdate,
  encodeAwarenessUpdate,
} from "y-protocols/awareness";

/* A Yjs provider over the hub's collab relay.

   Not y-websocket: the transport is the same SSE-down / POST-up pair the rest
   of the app already uses, which needs no new server dependency, no upgrade
   handshake, and nothing special from a proxy that already carries /events.
   Typing latency is one POST, batched — for a document that is well inside
   what a person notices.

   The hub never parses these bytes. It stores them in arrival order and hands
   the log to whoever joins next, which is all a Yjs peer needs to converge.

   The one rule that matters: a Yjs document seeded independently by two
   clients from the same text is NOT the same document — the items carry
   different ids and a merge duplicates every character. So only the client the
   hub calls `seed` builds from the file; everyone else builds from the log. */

export type CollabStatus = "connecting" | "live" | "offline";

// Updates are coalesced into one POST per tick. Long enough to batch a burst
// of keystrokes, short enough that a watcher sees you type.
const FLUSH_MS = 60;

export class CollabDoc {
  readonly doc = new Y.Doc();
  readonly text: Y.Text;
  readonly awareness: Awareness;

  private es: EventSource | null = null;
  private pending: Uint8Array[] = [];
  private timer: ReturnType<typeof setTimeout> | null = null;
  private closed = false;
  // Whether a `hello` has ever arrived. Distinguishes a dropped connection
  // (retry, keep the document) from a relay that does not exist (give up on
  // co-editing and let the editor open solo).
  private everConnected = false;
  // Updates that came FROM the relay must not be echoed back to it.
  private applying = false;

  constructor(
    private readonly url: string,
    private readonly seedText: string,
    private readonly onStatus: (s: CollabStatus) => void,
    private readonly onSeeded: () => void,
    // Called when the relay turns out not to be reachable at all, so the
    // caller can fall back to plain single-writer editing.
    private readonly onUnavailable: () => void,
    private readonly me?: { name: string; colour: string },
  ) {
    this.text = this.doc.getText("body");
    this.awareness = new Awareness(this.doc);
    // y-codemirror.next reads these two fields to label and colour a remote
    // caret. Without a local state this client is invisible to everyone else
    // and its own peer count never moves off zero.
    if (this.me) {
      this.awareness.setLocalStateField("user", {
        name: this.me.name,
        color: this.me.colour,
        colorLight: this.me.colour + "33",
      });
    }
    this.awareness.on("update", this.onAwareness);
    this.doc.on("update", (u: Uint8Array) => {
      if (this.applying) return;
      this.pending.push(u);
      if (!this.timer) this.timer = setTimeout(() => this.flush(), FLUSH_MS);
    });
  }

  // Awareness is relayed, never logged: it says where a caret is this second,
  // so a joiner replaying it would get cursors for people who have gone home.
  private onAwareness = ({
    added,
    updated,
    removed,
  }: {
    added: number[];
    updated: number[];
    removed: number[];
  }) => {
    const changed = added.concat(updated, removed);
    if (!changed.length || this.closed) return;
    const update = encodeAwarenessUpdate(this.awareness, changed);
    void fetch(this.url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ awareness: bytesToB64(update) }),
    }).catch(() => {
      // A lost cursor position corrects itself on the next keystroke.
    });
  };

  connect() {
    this.onStatus("connecting");
    const es = new EventSource(this.url);
    this.es = es;
    es.onmessage = (e) => {
      let f: {
        type: string;
        seed?: boolean;
        log?: string[];
        update?: string;
        awareness?: string;
      };
      try {
        f = JSON.parse(e.data);
      } catch {
        return;
      }
      if (f.type === "hello") {
        this.everConnected = true;
        this.applying = true;
        try {
          for (const u of f.log ?? []) Y.applyUpdate(this.doc, b64ToBytes(u));
        } finally {
          this.applying = false;
        }
        // Empty room: somebody has to put the file's text into the document,
        // and the hub picked us. Done OUTSIDE `applying` so it is broadcast.
        if (f.seed && this.text.length === 0 && this.seedText) {
          this.text.insert(0, this.seedText);
        }
        this.onSeeded();
        this.onStatus("live");
        return;
      }
      if (f.type === "update" && f.update) {
        this.applying = true;
        try {
          Y.applyUpdate(this.doc, b64ToBytes(f.update));
        } finally {
          this.applying = false;
        }
        return;
      }
      if (f.type === "awareness" && f.awareness) {
        applyAwarenessUpdate(this.awareness, b64ToBytes(f.awareness), this);
        return;
      }
      if (f.type === "resync") {
        // We missed updates, so this document is no longer trustworthy.
        // Reconnecting re-reads the whole log.
        this.reconnect();
      }
    };
    es.onerror = () => {
      this.onStatus("offline");
      // EventSource retries on its own. But if it has never once connected,
      // the relay is not there at all — an older hub with no such route, or a
      // 403 — and something has to let the editor open anyway, or the caller
      // waits forever for a `hello` that is not coming.
      if (!this.everConnected) this.onUnavailable();
    };
  }

  private reconnect() {
    this.es?.close();
    if (this.closed) return;
    // A fresh doc, or the replayed log would merge into the one we have and
    // double every character it already contains.
    this.connect();
  }

  private async flush() {
    this.timer = null;
    if (!this.pending.length || this.closed) return;
    const merged = Y.mergeUpdates(this.pending);
    this.pending = [];
    try {
      const res = await fetch(this.url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ update: bytesToB64(merged) }),
      });
      if (res.ok) {
        const out = await res.json().catch(() => ({}));
        // The room filled up and was emptied: everyone rebuilds from the file
        // the snapshotter just wrote.
        if (out.full) this.reconnect();
      }
    } catch {
      // Put it back: an update that never reached the relay is an edit no
      // peer will ever see, which is worse than sending it twice (Yjs
      // updates are idempotent).
      this.pending.unshift(merged);
      this.onStatus("offline");
    }
  }

  /* How many OTHER editors are in this document right now, from awareness.
     It is what tells a write on this path apart: with a co-editor present the
     change is their snapshot of the document I already have, so warning me
     about it is noise. Alone, the same event is somebody editing outside the
     room — a CLI, another device — which I really do need to know about. */
  peerCount(): number {
    return Math.max(0, this.awareness.getStates().size - 1);
  }

  destroy() {
    this.closed = true;
    this.awareness.off("update", this.onAwareness);
    if (this.timer) clearTimeout(this.timer);
    this.es?.close();
    this.awareness.destroy();
    this.doc.destroy();
  }
}

function b64ToBytes(s: string): Uint8Array {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function bytesToB64(b: Uint8Array): string {
  let s = "";
  for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
  return btoa(s);
}
