// Run with `npm test` (node's built-in runner; node >= 23 strips the types).
// Excluded from tsconfig's include — it imports node: builtins, which the
// app's DOM-only lib set does not know about.
import { test } from "node:test";
import assert from "node:assert/strict";
import { parseConflict } from "./conflict.ts";

test("parses the original, the device and the moment out of the name", () => {
  const c = parseConflict("notes/plan.md.bdrive-conflict-laptop-20260814T060945Z");
  assert.ok(c);
  assert.equal(c.original, "notes/plan.md");
  assert.equal(c.device, "laptop");
  assert.equal(c.when.toISOString(), "2026-08-14T06:09:45.000Z");
});

test("a device name with dashes still splits on the anchored timestamp", () => {
  const c = parseConflict("plan.md.bdrive-conflict-mira-laptop-20260814T060945Z");
  assert.ok(c);
  assert.equal(c.device, "mira-laptop");
  assert.equal(c.original, "plan.md");
});

test("the match is an anchored suffix, not a substring", () => {
  // a folder named after a conflict copy holds ordinary files
  assert.equal(parseConflict("a.md.bdrive-conflict-x-20260814T060945Z/b.md"), null);
  assert.equal(parseConflict("plan.md.bdrive-conflict-x-20260814T060945Z.bak"), null);
});

test("a malformed or truncated suffix is not a conflict copy", () => {
  for (const p of [
    "plan.md",
    "plan.md.bdrive-conflict",
    "plan.md.bdrive-conflict-laptop",
    "plan.md.bdrive-conflict-laptop-20260814",
    "plan.md.bdrive-conflict-laptop-20260814T0609Z",
    "plan.md.bdrive-conflict-laptop-2026-08-14T06:09:45Z",
    "plan.md.bdrive-conflict-way-too-long-a-device-name-to-have-survived-clip-20260814T060945Z",
  ]) {
    assert.equal(parseConflict(p), null, p);
  }
});

test("an impossible date degrades to not-a-conflict rather than a bogus Date", () => {
  assert.equal(parseConflict("plan.md.bdrive-conflict-laptop-20261314T060945Z"), null);
  assert.equal(parseConflict("plan.md.bdrive-conflict-laptop-20260230T060945Z"), null);
  assert.equal(parseConflict("plan.md.bdrive-conflict-laptop-20260814T256945Z"), null);
});

test("a conflict copy of a conflict copy resolves to the inner one", () => {
  const outer = parseConflict(
    "plan.md.bdrive-conflict-a-20260101T000000Z.bdrive-conflict-b-20260102T000000Z",
  );
  assert.ok(outer);
  assert.equal(outer.device, "b");
  assert.equal(outer.original, "plan.md.bdrive-conflict-a-20260101T000000Z");
  const inner = parseConflict(outer.original);
  assert.ok(inner);
  assert.equal(inner.original, "plan.md");
});

test("an empty device name (sanitize kept nothing) still parses", () => {
  const c = parseConflict("plan.md.bdrive-conflict--20260814T060945Z");
  assert.ok(c);
  assert.equal(c.device, "");
  assert.equal(c.original, "plan.md");
});
