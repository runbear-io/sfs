import { useState } from "react";
import type { Project } from "../api/types";
import { copyText } from "../util";
import { ProjectIcon } from "./shell";
import { projColor } from "./ProjectNav";

/* ---- project home guide ----
   One paste sets up any coding agent: the prompt points at the canonical
   INSTALL_FOR_AGENTS.md with this hub's URL and this project's id and
   name filled in (the name is what the agent recommends as the folder
   name). The agent fetches the doc and handles every deviation — already
   installed, no Homebrew, sign-in, wrong folder — so the page itself
   stays to one line of prose; detail lives in the collapsed sections. */

export const INSTALL_DOC = "https://raw.githubusercontent.com/runbear-io/beardrive/main/INSTALL_FOR_AGENTS.md";

export function ConnectGuide({ project, existing }: { project: Project; existing?: boolean }) {
  const origin = window.location.origin;
  // When the creator said they already have a folder, say so in the prompt.
  // Without it an agent reads an empty project and proposes creating a new
  // subfolder — the one recommendation that is wrong for this person. It
  // still asks which folder: that question is the runbook's hard gate and
  // nothing here may weaken it.
  const ask = existing
    ? '. I already have a folder of notes — ask me which one to sync (the project is named "'
    : '. Ask me which folder to sync (the project is named "';
  const prompt =
    "Follow " +
    INSTALL_DOC +
    "\nto set up BearDrive project " +
    project.id +
    " on " +
    origin +
    ask +
    project.name +
    '").';
  const manual =
    "brew install runbear-io/tap/beardrive" +
    "\nbdrive login " +
    origin +
    "\nbdrive init --project " +
    project.id;

  return (
    <div className="guide">
      <h1 className="in-title gd-head">
        <span
          className="proj-mark"
          aria-hidden="true"
          style={{ background: projColor(project.name) }}
        >
          <ProjectIcon name={project.icon} />
        </span>
        {project.name}
      </h1>
      {project.description && <p className="in-desc">{project.description}</p>}
      <div className="gd-body">
        <p className="gd-desc">
          {existing
            ? "Paste into your coding agent — Claude Code, Cowork, Codex, Gemini CLI, Hermes — in the folder you already have:"
            : "Paste into your coding agent — Claude Code, Cowork, Codex, Gemini CLI, Hermes — in the folder where you want the files:"}
        </p>
        {existing && (
          <p className="gd-note">
            Your files stay exactly where they are. Connecting a folder never moves, renames or
            overwrites anything in it — it uploads what is there and keeps it in sync.
          </p>
        )}
        <GuideCode code={prompt} />
        <p className="gd-desc">
          The agent installs the CLI, signs this machine in, and registers the sync hooks — asking
          before anything it changes.
        </p>
        <p className="gd-desc">Runs on macOS and Linux. Windows is not supported yet.</p>
        <details className="gd-manual">
          <summary>What exactly happens</summary>
          <ul className="gd-desc gd-list">
            <li>
              Sign-in uses a device code you approve in this browser — the folder itself never
              holds credentials.
            </li>
            <li>
              Sync hooks pull the latest before every agent turn, push edits seconds after they
              happen, and stamp each change with the session that made it; agent reads feed
              Insights. They register once per machine in your agent's own config, so every
              session is covered and nothing is written into the synced folder.
            </li>
            <li>Codex hooks are off by default: set [features] codex_hooks = true in ~/.codex/config.toml.</li>
          </ul>
        </details>
        <details className="gd-manual">
          <summary>Or run it yourself</summary>
          <p className="gd-desc">
            Same result, in the folder you want the files. Install the CLI, point it at this hub,
            then bdrive init registers the sync hooks and starts syncing.
          </p>
          <GuideCode code={manual} />
          <p className="gd-desc">
            <a href="https://docs.beardrive.ai/manual/install/" target="_blank" rel="noreferrer">
              Full manual setup guide →
            </a>
          </p>
        </details>
      </div>
    </div>
  );
}

export function GuideCode({ code }: { code: string }) {
  const [label, setLabel] = useState("Copy");
  return (
    <pre className="gd-code">
      <code>{code}</code>
      <button
        className="gd-copy"
        onClick={async () => {
          setLabel((await copyText(code)) ? "Copied" : "Copy failed");
          setTimeout(() => setLabel("Copy"), 1400);
        }}
      >
        {label}
      </button>
    </pre>
  );
}
