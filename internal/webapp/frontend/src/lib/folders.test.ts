// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
import { test } from "node:test";
import assert from "node:assert/strict";
import { ruleFor } from "./folders.ts";
import type { FolderRule } from "../api/types.ts";

const rule = (prefix: string): FolderRule => ({ prefix, default: "", grants: [], me: "write" });

// Longest prefix wins outright — rules never merge (folders.go says so, and
// the UI must not imply otherwise by naming the broader one).
test("the deepest matching rule is the one named", () => {
  const rules = [rule("a/"), rule("a/b/"), rule("z/")];
  assert.equal(ruleFor(rules, "a/b/c.md")?.prefix, "a/b/");
  assert.equal(ruleFor(rules, "a/x.md")?.prefix, "a/");
  assert.equal(ruleFor(rules, "q/x.md"), undefined);
});

// The slash is the whole reason prefixes are stored slash-terminated: without
// it "a/" would claim a sibling called "ab.md".
test("a prefix does not match a sibling that merely starts the same", () => {
  const rules = [rule("a/")];
  assert.equal(ruleFor(rules, "ab.md"), undefined);
  assert.equal(ruleFor(rules, "a/b.md")?.prefix, "a/");
});

// A folder is matched on its own key, so a caller passes path + "/".
test("a folder matches its own rule", () => {
  const rules = [rule("designs/")];
  assert.equal(ruleFor(rules, "designs"), undefined, "unterminated key must not match");
  assert.equal(ruleFor(rules, "designs/")?.prefix, "designs/");
});
