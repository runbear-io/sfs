import { useCallback, useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { getJSON } from "../api/http";
import { navigate } from "../nav";
import { GuideCode, INSTALL_DOC } from "./ConnectGuide";

// The desktop app's onboarding (storyboard 2026-08-21, frames 2 and 5-9):
// welcome → connect a shared folder inside the user's own project → first
// sync → what to do next. Desktop-only: the hub renders none of this, and
// every step is a real URL (/setup, /setup/connect, /setup/syncing,
// /setup/done) so reload and back/forward behave.
//
// All the logic lives in the sidecar (cmd/bdrive/desktop_onboard.go); this
// file is the view over /api/desktop/{inspect,init,init/status}.

export type SetupStep = "welcome" | "connect" | "syncing" | "done";

const post = (path: string, body?: unknown) =>
  fetch(path, {
    method: "POST",
    headers: { "X-Bdrive-Desktop": "1", "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });

type Inspect = {
  path: string;
  name: string;
  root_name?: string;
  target?: string;
  error?: string;
  conflict?: string;
  markers?: string[];
  is_claude_project?: boolean;
  entries?: string[];
  entries_truncated?: boolean;
  target_exists?: boolean;
  join?: { project: string; name: string };
};

type InitStatus = {
  phase: "idle" | "creating" | "connecting" | "syncing" | "done" | "error";
  detail?: string;
  error?: string;
  project?: string;
  name?: string;
  root?: string;
  mount?: string;
  joined?: boolean;
  files?: number;
};

/** Frame 2: the first thing a brand-new install shows. */
function Welcome({ onStart }: { onStart: () => void }) {
  return (
    <div className="onboard setup-welcome">
      <h1>Welcome to BearDrive</h1>
      <p>
        One shared drive for your team and your AI agents — every folder you connect stays in sync,
        with full history.
      </p>
      <Button variant="primary" id="setup-start" onClick={onStart}>
        Get started
      </Button>
      <p className="setup-foot">Takes about two minutes · works offline afterwards</p>
    </div>
  );
}

/** The live tree preview — the whole point of this screen (Option A). */
function TreePreview({ data, name }: { data: Inspect | null; name: string }) {
  const entries = data?.entries ?? [];
  const rootName = data?.root_name || "your project";
  return (
    <div className="setup-tree" aria-label="How it will look">
      <div className="setup-tree-head">HOW IT WILL LOOK</div>
      <div className="setup-tree-root">
        {rootName}/ <span className="setup-dim">· private</span>
      </div>
      {entries.map((e) => (
        <div key={e} className="setup-dim">
          ├── {e}
        </div>
      ))}
      {data?.entries_truncated && <div className="setup-dim">├── …</div>}
      <div className="setup-tree-shared">
        └── {name || "team"}/ <span className="setup-shared-tag">shared</span>
      </div>
      <p className="setup-tree-foot">
        Only {name || "team"}/ syncs. Everything else never leaves this Mac. Teammates get the same{" "}
        {name || "team"}/ inside their own projects.
      </p>
    </div>
  );
}

/** Frames 5-7: pick the root, name the shared folder, one commit. */
function Connect({ onStarted }: { onStarted: () => void }) {
  const [root, setRoot] = useState("");
  const [name, setName] = useState("team");
  const [hooks, setHooks] = useState(true);
  const [data, setData] = useState<Inspect | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [why, setWhy] = useState(false);

  // Debounced: the preview follows what is typed, without a request per key.
  useEffect(() => {
    if (!root) {
      setData(null);
      return;
    }
    const t = setTimeout(() => {
      getJSON<Inspect>(`/api/desktop/inspect?path=${encodeURIComponent(root)}&name=${encodeURIComponent(name)}`)
        .then(setData)
        .catch(() => setData(null));
    }, 200);
    return () => clearTimeout(t);
  }, [root, name]);

  const choose = useCallback(async () => {
    const r = await post("/api/desktop/choose-folder");
    if (!r.ok) return;
    const out = (await r.json()) as { path?: string; canceled?: boolean };
    if (out.path) setRoot(out.path);
  }, []);

  const start = useCallback(async () => {
    setBusy(true);
    setErr("");
    const r = await post("/api/desktop/init", { root, name, hooks });
    setBusy(false);
    if (!r.ok) {
      setErr((await r.text()).trim() || "could not connect that folder");
      return;
    }
    onStarted();
  }, [root, name, hooks, onStarted]);

  const joining = !!data?.join;
  const blocked = !root || !!data?.error || !!data?.conflict || busy;

  return (
    <div className="setup-connect">
      <header>
        <h2>
          Add a shared folder to your project
          {!joining && <span className="setup-badge">RECOMMENDED</span>}
        </h2>
        <p>
          Your project stays yours. One folder inside it is shared — your agent reads it in every
          session.{" "}
          <a href="#why" onClick={(e) => (e.preventDefault(), setWhy(!why))}>
            Why this layout?
          </a>
        </p>
        {why && (
          <ul className="setup-why">
            <li>Claude sessions here read and write it automatically — shared memory, no setup.</li>
            <li>Everything outside it stays on this Mac. Your code never syncs.</li>
            <li>Teammates get the same folder inside their own projects — one shared space.</li>
          </ul>
        )}
      </header>

      <div className="setup-body">
        <div className="setup-form">
          <label className="setup-field">
            <span>Your project folder</span>
            <div className="setup-root">
              <input
                id="setup-root"
                value={root}
                spellCheck={false}
                placeholder="/Users/you/work/your-project"
                onChange={(e) => setRoot(e.target.value)}
              />
              <button type="button" id="setup-choose" onClick={choose}>
                Choose…
              </button>
            </div>
          </label>

          <label className="setup-field">
            <span>Shared folder name</span>
            <input id="setup-name" value={name} spellCheck={false} onChange={(e) => setName(e.target.value)} />
          </label>

          <label className="setup-toggle">
            <input type="checkbox" id="setup-hooks" checked={hooks} onChange={(e) => setHooks(e.target.checked)} />
            <span>Claude Code integration</span>
          </label>

          {data?.is_claude_project && !data?.error && (
            <p className="setup-ok">Claude Code project detected — {(data.markers ?? []).join(", ")}</p>
          )}
          {joining && (
            <p className="setup-ok">
              Your team already shares a “{data!.join!.name}” space — you'll join it.
            </p>
          )}
          {(data?.error || data?.conflict || err) && (
            <p className="setup-err">{err || data?.conflict || data?.error}</p>
          )}

          <Button variant="primary" id="setup-go" disabled={blocked} onClick={start}>
            {joining ? `Join ${name}/ and start syncing` : `Create ${name}/ and start syncing`}
          </Button>
          <p className="setup-foot">
            Prefer to share the whole folder?{" "}
            <a href="https://docs.beardrive.ai/manual/setup-by-hand/" target="_blank" rel="noreferrer">
              Advanced
            </a>
          </p>
        </div>

        <TreePreview data={data} name={name} />
      </div>
    </div>
  );
}

/** Frame 8: honest progress, and permission to walk away. */
function Syncing({ onDone }: { onDone: (s: InitStatus) => void }) {
  const [st, setSt] = useState<InitStatus>({ phase: "creating" });
  const done = useRef(false);
  useEffect(() => {
    const id = setInterval(async () => {
      try {
        const s = await getJSON<InitStatus>("/api/desktop/init/status");
        setSt(s);
        if (!done.current && (s.phase === "done" || s.phase === "error")) {
          done.current = true;
          clearInterval(id);
          if (s.phase === "done") onDone(s);
        }
      } catch {
        /* the sidecar is local; a blip resolves on the next tick */
      }
    }, 400);
    return () => clearInterval(id);
  }, [onDone]);

  const phases: InitStatus["phase"][] = ["creating", "connecting", "syncing", "done"];
  const at = Math.max(0, phases.indexOf(st.phase));
  return (
    <div className="setup-syncing">
      <h2>Syncing {st.name ? st.name + "/" : "your folder"}</h2>
      <div className="setup-bar">
        <div style={{ width: `${((at + 1) / phases.length) * 100}%` }} />
      </div>
      <ul className="setup-log">
        <li className={at > 0 ? "ok" : ""}>created the shared folder</li>
        <li className={at > 1 ? "ok" : ""}>{st.joined ? "joined the project" : "created the project"}</li>
        <li className={at > 2 ? "ok" : ""}>first sync</li>
      </ul>
      {st.phase === "error" ? (
        <p className="setup-err" id="setup-error">
          {st.error}
        </p>
      ) : (
        <p className="setup-foot">You can close this window — syncing continues from the menu bar.</p>
      )}
    </div>
  );
}

/** Frame 9: the payoff and exactly three next moves. */
function Done({ st }: { st: InitStatus }) {
  const [copied, setCopied] = useState("");
  const name = st.name || "team";
  const prompt =
    `Follow ${INSTALL_DOC}\nto set up the shared ${name}/ folder in my project on ` +
    window.location.origin +
    ". Ask me which folder to sync.";
  const copy = async (what: string, text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(what);
    } catch {
      setCopied("");
    }
  };
  return (
    <div className="setup-done">
      <h2>{name}/ is live</h2>
      <p className="setup-foot">
        shared inside {st.root ? st.root.split("/").pop() : "your project"} · history from here on
      </p>
      {st.error && <p className="setup-err">{st.error}</p>}
      <div className="setup-cards">
        <div className="setup-card">
          <h3>Open the dashboard</h3>
          <p>Browse files, history, and who reads what.</p>
          <Button id="setup-open" onClick={() => navigate(st.project ? "/" + st.project : "/")}>
            Open
          </Button>
        </div>
        <div className="setup-card setup-card-lead">
          <h3>Tell your agent</h3>
          <p>Claude sessions in this folder now share context with your team.</p>
          <GuideCode code={prompt} />
        </div>
        <div className="setup-card">
          <h3>Invite teammates</h3>
          <p>Send them the same prompt — their agent connects their machine.</p>
          <Button id="setup-copy-prompt" onClick={() => copy("prompt", prompt)}>
            {copied === "prompt" ? "Copied" : "Copy prompt"}
          </Button>
        </div>
      </div>
    </div>
  );
}

export function Setup({ step, signedIn, onSignIn }: { step: SetupStep; signedIn: boolean; onSignIn: () => void }) {
  const [finished, setFinished] = useState<InitStatus | null>(null);

  // The step is the URL; sign-in state decides where a bare /setup lands.
  useEffect(() => {
    if (step === "welcome" && signedIn) navigate("/setup/connect");
    if (step !== "welcome" && !signedIn) navigate("/setup");
  }, [step, signedIn]);

  return (
    <div className="setup">
      {step === "welcome" && <Welcome onStart={onSignIn} />}
      {step === "connect" && <Connect onStarted={() => navigate("/setup/syncing")} />}
      {step === "syncing" && (
        <Syncing
          onDone={(s) => {
            setFinished(s);
            navigate("/setup/done");
          }}
        />
      )}
      {step === "done" && <Done st={finished ?? {} as InitStatus} />}
    </div>
  );
}
