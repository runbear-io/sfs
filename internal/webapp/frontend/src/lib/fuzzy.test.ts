// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Excluded from tsconfig's include — it imports node: builtins, which the
// app's DOM-only lib set does not know about.
import { test } from "node:test";
import assert from "node:assert/strict";
import { fuzzy, fuzzyStemmed } from "./fuzzy.ts";

test("fuzzy subsequence-matches a path", () => {
  assert.ok(fuzzy("fileview", "webapp/frontend/src/components/FileView.tsx"));
  assert.equal(fuzzy("zzz", "webapp/components/FileView.tsx"), null);
});

test("empty query scores 0 with no hits", () => {
  assert.deepEqual(fuzzy("", "anything"), { score: 0, hits: [] });
});

test("the plural stemmer still finds the singular file", () => {
  assert.ok(fuzzyStemmed("ideas", "launch/idea.md"));
});
