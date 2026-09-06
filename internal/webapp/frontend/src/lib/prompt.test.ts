// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
import { test } from "node:test";
import assert from "node:assert/strict";
import { agentPrompt, INSTALL_DOC } from "./prompt.ts";
import type { Project } from "../api/types.ts";

const HUB = "https://hub.example";
const proj = { id: "p-123", name: "wiki" } as Project;

// The three call sites used to build this string by hand, three ways. These
// pin the wording each of them had, so the shared version is a refactor and
// not a quiet copy change.
test("a project's guide names the project and its id", () => {
  assert.equal(
    agentPrompt(HUB, { project: proj }),
    `Follow ${INSTALL_DOC}\nto set up BearDrive project p-123 on ${HUB}. ` +
      'Ask me which folder to sync (the project is named "wiki").',
  );
});

test("the empty state has no project to name yet", () => {
  assert.equal(
    agentPrompt(HUB),
    `Follow ${INSTALL_DOC}\nto set up a new BearDrive project on ${HUB}. Ask me which folder to sync.`,
  );
});

test("the desktop Done frame names the shared folder", () => {
  assert.equal(
    agentPrompt(HUB, { folder: "team" }),
    `Follow ${INSTALL_DOC}\nto set up the shared team/ folder in my project on ${HUB}. ` +
      "Ask me which folder to sync.",
  );
});

// Without this an agent reads an empty project and proposes creating a new
// subfolder — the one recommendation that is wrong for someone who already
// has the files.
test("existing says so, and still asks which folder", () => {
  const out = agentPrompt(HUB, { project: proj, existing: true });
  assert.match(out, /I already have a folder of notes/);
  assert.match(out, /ask me which one to sync/);
});

// The folder question is the runbook's hard gate; no variant may drop it.
test("every variant asks which folder", () => {
  for (const opts of [{}, { project: proj }, { folder: "team" }, { project: proj, existing: true }]) {
    assert.match(agentPrompt(HUB, opts), /which (folder|one) to sync/, JSON.stringify(opts));
  }
});
