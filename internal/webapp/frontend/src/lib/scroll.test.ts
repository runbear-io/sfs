// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
import { test } from "node:test";
import assert from "node:assert/strict";
import { armGoal, applyGoal, noteScroll, MAX_APPLY } from "./scroll.ts";

const KEY = "/proj/notes/guide.md";
// A tall page: plenty of room below any offset these tests use.
const TALL = 10000;

test("a fresh navigation resets to the top", () => {
  const g = armGoal(KEY, 0);
  assert.equal(applyGoal(g, KEY), 0);
});

test("back/forward restores the remembered offset", () => {
  const g = armGoal(KEY, 820);
  assert.equal(applyGoal(g, KEY), 820);
});

test("content that grows re-applies the goal, up to MAX_APPLY", () => {
  const g = armGoal(KEY, 820);
  for (let i = 0; i < MAX_APPLY; i++) assert.equal(applyGoal(g, KEY), 820);
  assert.equal(applyGoal(g, KEY), null, "budget spent");
});

test("a goal armed for another route never fires", () => {
  const g = armGoal(KEY, 820);
  assert.equal(applyGoal(g, "/proj/other.md"), null);
});

// The reported bug: a read-count poll calls onRendered 60s after the reader
// scrolled, and the restorer yanks them back to the top.
test("onRendered after a user scroll moves nothing", () => {
  const g = armGoal(KEY, 0);
  assert.equal(applyGoal(g, KEY), 0, "the one legitimate reset");
  noteScroll(g, KEY, 1400, TALL); // the reader reads
  assert.equal(applyGoal(g, KEY), null, "the poll cannot move them");
});

test("once retired, the goal stays retired", () => {
  const g = armGoal(KEY, 0);
  applyGoal(g, KEY);
  noteScroll(g, KEY, 1400, TALL);
  noteScroll(g, KEY, 1600, TALL); // still reading
  for (let i = 0; i < 5; i++) assert.equal(applyGoal(g, KEY), null);
});

// scrollTo fires a scroll event of its own; measuring against zero instead of
// against `want` would make every restore retire its own goal.
test("our own scrollTo does not retire the goal", () => {
  const g = armGoal(KEY, 820);
  assert.equal(applyGoal(g, KEY), 820);
  noteScroll(g, KEY, 820, TALL); // the event scrollTo just fired
  noteScroll(g, KEY, 821, TALL); // ...and a pixel of rounding
  assert.equal(applyGoal(g, KEY), 820, "still armed for late-growing content");
});

// A page shorter than the one before it makes the browser clamp the carried-
// over offset. Both clamp shapes must survive, or two acceptance criteria
// regress at once.
test("a clamp before the first apply does not retire the goal", () => {
  const g = armGoal(KEY, 0); // fresh nav, arriving from a taller page
  noteScroll(g, KEY, 600, 600); // carried-over offset, clamped to the bottom
  assert.equal(applyGoal(g, KEY), 0, "the fresh nav still lands at the top");
});

test("a clamp below want does not retire the goal", () => {
  const g = armGoal(KEY, 820); // POP into a folder whose feed loads late
  assert.equal(applyGoal(g, KEY), 820);
  noteScroll(g, KEY, 400, 400); // does not fit yet: clamped at the bottom
  assert.equal(applyGoal(g, KEY), 820, "restores once the feed lands");
});

test("a scroll past the goal is the reader, clamped or not", () => {
  const g = armGoal(KEY, 820);
  applyGoal(g, KEY);
  noteScroll(g, KEY, 1200, 1200); // below the goal is the test, not "at max"
  assert.equal(applyGoal(g, KEY), null);
});

test("a scroll on another route does not retire this goal", () => {
  const g = armGoal(KEY, 820);
  applyGoal(g, KEY);
  noteScroll(g, "/proj/other.md", 1400, TALL);
  assert.equal(applyGoal(g, KEY), 820);
});
