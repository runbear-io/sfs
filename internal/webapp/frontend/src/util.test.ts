// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Node has no localStorage, which is what makes the hostile-browser branches
// cheap to pin: the bare run IS the "storage missing" case.
import { test } from "node:test";
import assert from "node:assert/strict";
import { lastProject, rememberProject, resolveWiki } from "./util.ts";

// Swap globalThis.localStorage for the length of one call, always putting the
// original back — later tests in the same process must not inherit a stub.
function withStorage(stub: unknown, fn: () => void) {
  const g = globalThis as { localStorage?: unknown };
  const had = "localStorage" in g;
  const prev = g.localStorage;
  g.localStorage = stub;
  try {
    fn();
  } finally {
    if (had) g.localStorage = prev;
    else delete g.localStorage;
  }
}

test("no storage at all: reads empty, writes stay silent", () => {
  assert.equal(lastProject(), "");
  assert.doesNotThrow(() => rememberProject("p1"));
});

test("round-trips the last project through storage", () => {
  const m = new Map<string, string>();
  withStorage(
    {
      getItem: (k: string) => m.get(k) ?? null,
      setItem: (k: string, v: string) => void m.set(k, v),
    },
    () => {
      assert.equal(lastProject(), "");
      rememberProject("p1");
      assert.equal(lastProject(), "p1");
      rememberProject("p2");
      assert.equal(lastProject(), "p2");
    },
  );
});

/* The wikilink match matrix. It used to run at click time inside the file
   view, where a browser was the only way to reach it; the rules did not
   change when they moved (BEA-136), so this is the whole contract. */
const FILES = [
  { path: "guide.md", name: "guide.md" },
  { path: "notes/readme.md", name: "readme.md" },
  { path: "notes/deep/Topic.md", name: "Topic.md" },
  { path: "LICENSE", name: "LICENSE" },
  // Same basename as a file higher up, to pin the path-before-basename rule.
  { path: "archive/guide.md", name: "guide.md" },
];

test("wikilink target resolves by path, then basename", () => {
  const hit = (t: string) => resolveWiki(t, FILES)?.path;
  assert.equal(hit("notes/readme.md"), "notes/readme.md"); // exact path
  assert.equal(hit("notes/readme"), "notes/readme.md"); // path, .md implied
  assert.equal(hit("readme"), "notes/readme.md"); // basename, .md implied
  assert.equal(hit("readme.md"), "notes/readme.md"); // basename
  assert.equal(hit("LICENSE"), "LICENSE"); // no extension at all
  assert.equal(hit("x y"), undefined); // spaces are just characters
  assert.equal(hit("nowhere"), undefined); // a miss is undefined, not a throw
});

test("wikilink matching is case-insensitive on both sides", () => {
  const hit = (t: string) => resolveWiki(t, FILES)?.path;
  assert.equal(hit("GUIDE"), "guide.md");
  assert.equal(hit("NOTES/DEEP/topic"), "notes/deep/Topic.md");
  assert.equal(hit("topic.MD"), "notes/deep/Topic.md");
});

test("an exact path beats a basename anywhere else in the tree", () => {
  assert.equal(resolveWiki("archive/guide", FILES)?.path, "archive/guide.md");
  // Bare "guide" is ambiguous by basename; the top-level path wins.
  assert.equal(resolveWiki("guide", FILES)?.path, "guide.md");
});

test("storage that throws is swallowed on both sides", () => {
  const boom = () => {
    throw new Error("SecurityError");
  };
  withStorage({ getItem: boom, setItem: boom }, () => {
    assert.equal(lastProject(), "");
    assert.doesNotThrow(() => rememberProject("p1"));
  });
});
