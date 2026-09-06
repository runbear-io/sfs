// The agent paste-prompt, in the one place it is built.
//
// It used to be assembled by hand in ConnectGuide, EmptyState and Setup's
// Done frame — three wordings of the single most important string in
// onboarding, drifting independently. The three shapes differ only in what is
// being set up, so they are one function with one clause that varies.
//
// Pure and origin-injected so it can be tested at all: the frontend suite is
// node --test over plain TS with no DOM, and the callers all have an origin
// to hand.

import type { Project } from "../api/types.ts";

export const INSTALL_DOC =
  "https://raw.githubusercontent.com/runbear-io/beardrive/main/INSTALL_FOR_AGENTS.md";

export function agentPrompt(
  origin: string,
  opts: { project?: Project; folder?: string; existing?: boolean } = {},
): string {
  const { project, folder, existing } = opts;
  const target = project
    ? "BearDrive project " + project.id
    : folder
      ? `the shared ${folder}/ folder in my project`
      : "a new BearDrive project";
  // `existing` says the person already has a folder: without it an agent reads
  // an empty project and proposes creating a new subfolder, the one
  // recommendation that is wrong for them. Every variant still ASKS which
  // folder — that question is the runbook's hard gate and nothing here may
  // weaken it.
  const ask = existing
    ? "I already have a folder of notes — ask me which one to sync"
    : "Ask me which folder to sync";
  const named = project ? ` (the project is named "${project.name}")` : "";
  return `Follow ${INSTALL_DOC}\nto set up ${target} on ${origin}. ${ask}${named}.`;
}
