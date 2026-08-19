import { type ReactNode, useRef, useState, useSyncExternalStore } from "react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogTitle,
} from "@/components/ui/dialog";

// In-app modal prompt/confirm replacing native prompt()/confirm().
// Imperative promise-based API (awaitable from event handlers, like the
// classic app); <ModalHost/> (mounted once in App) renders the active one
// inside a Radix Dialog, which owns Escape/overlay dismissal and focus.

type Prompt = {
  kind: "prompt";
  title: string;
  label: string;
  value: string;
  okLabel: string;
  // Irreversible actions: `match` holds OK disabled until the user types the
  // thing's name back exactly, `danger` paints the button red.
  match?: string;
  danger?: boolean;
  resolve: (v: string | null) => void;
};
type Confirm = {
  kind: "confirm";
  title: string;
  // A node, not a string: the run-wide undo has to SHOW the file list and the
  // "changed after this run" warning it is asking about, and a confirm whose
  // text can't hold them would push that list somewhere the user has to go
  // find. Every existing caller passes a string, which is a ReactNode.
  message: ReactNode;
  confirmLabel: string;
  danger: boolean;
  resolve: (v: boolean) => void;
};
type Modal = Prompt | Confirm;

let current: Modal | null = null;
let listeners: Array<() => void> = [];
function emit(next: Modal | null) {
  current = next;
  listeners.forEach((l) => l());
}

export function modalPrompt(
  title: string,
  label: string,
  value = "",
  okLabel = "OK",
  opts: { match?: string; danger?: boolean } = {},
): Promise<string | null> {
  return new Promise((resolve) =>
    emit({ kind: "prompt", title, label, value, okLabel, ...opts, resolve }),
  );
}

export function modalConfirm(
  title: string,
  message: ReactNode,
  confirmLabel = "Confirm",
  danger = false,
): Promise<boolean> {
  return new Promise((resolve) =>
    emit({ kind: "confirm", title, message, confirmLabel, danger, resolve }),
  );
}

export function ModalHost() {
  const m = useSyncExternalStore(
    (l) => {
      listeners.push(l);
      return () => {
        listeners = listeners.filter((x) => x !== l);
      };
    },
    () => current,
  );
  if (!m) return null;

  const cancel = () => {
    emit(null);
    if (m.kind === "prompt") m.resolve(null);
    else m.resolve(false);
  };

  return (
    <Dialog open onOpenChange={(open) => !open && cancel()}>
      <DialogContent className="modal" showCloseButton={false}>
        {m.kind === "prompt" ? <PromptBody m={m} /> : <ConfirmBody m={m} />}
      </DialogContent>
    </Dialog>
  );
}

function PromptBody({ m }: { m: Prompt }) {
  const input = useRef<HTMLInputElement>(null);
  const done = (v: string | null) => {
    emit(null);
    m.resolve(v);
  };
  // Return the raw string: null means cancelled, "" means the user submitted
// nothing. Collapsing the two here made every caller's blank-input branch
// unreachable.
  const [err, setErr] = useState("");
  // Controlled so the OK button can react to typing (the `match` gate). The
  // no-match path behaves exactly as the uncontrolled version did.
  const [val, setVal] = useState(m.value);
  const armed = m.match === undefined || val.trim() === m.match;
  const ok = () => {
    const v = val;
    if (!armed) return;
    if (!v.trim()) {
      // Keep the dialog open and say so where the user is looking, the way
      // the org-rename form does — closing and toasting loses their context.
      setErr("Give it a name.");
      input.current!.focus();
      return;
    }
    done(v);
  };
  return (
    <>
      <DialogTitle asChild>
        <h3>{m.title}</h3>
      </DialogTitle>
      <label className="modal-label" htmlFor="modal-input">{m.label}</label>
      <input
        className="modal-input"
        type="text"
        autoComplete="off"
        value={val}
        ref={input}
        id="modal-input"
        autoFocus
        onFocus={(e) => e.currentTarget.select()}
        aria-invalid={!!err}
        aria-describedby={err ? "modal-input-err" : undefined}
        onChange={(e) => {
          setVal(e.currentTarget.value);
          if (err) setErr("");
        }}
        onKeyDown={(e) => e.key === "Enter" && ok()}
      />
      {err && (
        <span id="modal-input-err" role="alert" className="field-err">
          {err}
        </span>
      )}
      <div className="modal-actions">
        <Button variant="subtle" onClick={() => done(null)}>
          Cancel
        </Button>
        <Button
          variant={m.danger ? "danger" : "primary"}
          onClick={ok}
          disabled={!armed}
        >
          {m.okLabel}
        </Button>
      </div>
    </>
  );
}

function ConfirmBody({ m }: { m: Confirm }) {
  const done = (v: boolean) => {
    emit(null);
    m.resolve(v);
  };
  return (
    <>
      <DialogTitle asChild>
        <h3>{m.title}</h3>
      </DialogTitle>
      {/* a div, not a p: a p may not legally contain the path list */}
      <div className="modal-msg">{m.message}</div>
      <div className="modal-actions">
        <Button variant="subtle" onClick={() => done(false)} autoFocus={m.danger}>
          Cancel
        </Button>
        <Button variant={m.danger ? "danger" : "primary"} onClick={() => done(true)} autoFocus={!m.danger}>
          {m.confirmLabel}
        </Button>
      </div>
    </>
  );
}
