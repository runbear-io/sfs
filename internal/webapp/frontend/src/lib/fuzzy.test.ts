// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Excluded from tsconfig's include — it imports node: builtins, which the
// app's DOM-only lib set does not know about.
import { test } from "node:test";
import assert from "node:assert/strict";
import { fuzzy, fuzzyStemmed, scoreLabel } from "./fuzzy.ts";

// A realistic tree: the queries below are the ones a person actually types.
const TREE = [
  "docs/install-guide.md",
  "docs/getting-started.md",
  "specs/hub-config.md",
  "webapp/frontend/src/components/FileView.tsx",
  "design/onboarding.md",
  "guide/other/install.md",
  "launch/idea.md",
];

// Rank the tree the way the palette does: strict first, then the typo pass
// over what strict rejected, then one sort.
function rank(query: string): string[] {
  const scored: Array<{ label: string; score: number }> = [];
  const missed: string[] = [];
  for (const label of TREE) {
    const m = scoreLabel(query, label);
    if (m) scored.push({ label, score: m.score });
    else missed.push(label);
  }
  for (const label of missed) {
    const m = scoreLabel(query, label, { allowError: true });
    if (m) scored.push({ label, score: m.score });
  }
  scored.sort((a, b) => b.score - a.score);
  return scored.map((s) => s.label);
}

test("fuzzy subsequence-matches a path", () => {
  assert.ok(fuzzy("fileview", "webapp/frontend/src/components/FileView.tsx"));
  assert.equal(fuzzy("zzz", "webapp/components/FileView.tsx"), null);
});

test("empty query scores 0 with no hits", () => {
  assert.deepEqual(fuzzy("", "anything"), { score: 0, hits: [] });
  assert.deepEqual(scoreLabel("", "anything"), { score: 0, hits: [] });
  assert.deepEqual(scoreLabel("   ", "anything"), { score: 0, hits: [] });
});

// The bug: a space is just another character to the subsequence walk, and a
// path never contains one, so two words used to match nothing at all.
test("a two-word query finds the file, first", () => {
  for (const [query, want] of [
    ["install guide", "docs/install-guide.md"],
    ["getting started", "docs/getting-started.md"],
    ["hub config", "specs/hub-config.md"],
    ["webapp components", "webapp/frontend/src/components/FileView.tsx"],
  ] as const) {
    assert.equal(rank(query)[0], want, query);
  }
});

test("token order is irrelevant", () => {
  const a = scoreLabel("webapp components", TREE[3]);
  const b = scoreLabel("components webapp", TREE[3]);
  assert.ok(a && b);
  assert.equal(a.score, b.score);
});

test("every token must match — AND, not OR", () => {
  assert.ok(scoreLabel("install guide", "docs/install-guide.md"));
  assert.equal(scoreLabel("install zzz", "docs/install-guide.md"), null);
});

test("one error per token is tolerated", () => {
  for (const [query, want] of [
    ["fileveiw", "webapp/frontend/src/components/FileView.tsx"], // transposition
    ["insatll", "docs/install-guide.md"],
    ["onbaording", "design/onboarding.md"],
  ] as const) {
    assert.equal(rank(query)[0], want, query);
    assert.equal(scoreLabel(query, want), null, query + " must miss the strict pass");
  }
  // The other install file matches too — it just ranks below the one whose
  // BASENAME carries the token.
  assert.ok(rank("insatll").includes("guide/other/install.md"));
});

test("short tokens stay strict — a one-error match on 3 characters matches nearly everything", () => {
  const label = "webapp/frontend/src/components/FileView.tsx";
  // "zil" would match after dropping the "z"; the length gate blocks it.
  assert.equal(scoreLabel("zil", label, { allowError: true }), null);
  // The same rule at 4 characters does pass.
  assert.ok(scoreLabel("zile", label, { allowError: true }));
});

test("exact always beats approximate", () => {
  const strict = scoreLabel("guide", "docs/install-guide.md");
  const errored = scoreLabel("guied", "guide/other/install.md", { allowError: true });
  assert.ok(strict && errored);
  assert.ok(errored.score < strict.score);
});

test("a hit in the file name outranks letters scattered across directories", () => {
  const a = scoreLabel("install guide", "docs/install-guide.md");
  const b = scoreLabel("install guide", "guide/other/install.md");
  assert.ok(a && b);
  assert.ok(a.score > b.score);
  assert.equal(rank("install guide")[0], "docs/install-guide.md");
});

// Highlight slices forward from the last hit: an out-of-order index emits an
// empty slice and then re-emits the character, silently duplicating letters.
test("hits are sorted and de-duplicated", () => {
  const m = scoreLabel("components webapp", TREE[3]);
  assert.ok(m);
  for (let i = 1; i < m.hits.length; i++) assert.ok(m.hits[i] > m.hits[i - 1], "strictly increasing");
  const o = scoreLabel("guide guide", "docs/install-guide.md");
  assert.ok(o);
  assert.deepEqual(o.hits, [...new Set(o.hits)]);
});

test("the plural stemmer still works, and composes with tokenizing", () => {
  assert.ok(fuzzyStemmed("ideas", "launch/idea.md"));
  assert.equal(rank("launch ideas")[0], "launch/idea.md");
});
