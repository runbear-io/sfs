// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Excluded from tsconfig's include — it imports node: builtins, which the
// app's DOM-only lib set does not know about.
import { test } from "node:test";
import assert from "node:assert/strict";
import { secretsBadge, secretsMessage } from "./secrets.ts";

test("one finding names the rule in words and the line", () => {
  const s = secretsBadge([{ rule: "aws_access_key_id", line: 3 }]);
  assert.equal(s, "This file contains an AWS access key (line 3).");
});

test("several findings read as a list", () => {
  const s = secretsBadge([
    { rule: "aws_access_key_id", line: 3 },
    { rule: "github_pat", line: 9 },
    { rule: "private_key", line: 40 },
  ]);
  assert.equal(
    s,
    "This file contains an AWS access key (line 3), a GitHub token (line 9) and a private key (line 40).",
  );
});

test("an unknown rule id falls back to the raw id rather than reporting nothing", () => {
  assert.match(secretsBadge([{ rule: "nomad_token", line: 1 }]), /nomad_token \(line 1\)/);
});

test("the badge and the share dialog name the same finding identically", () => {
  const f = [{ rule: "openai_api_key", line: 12 }];
  assert.match(secretsBadge(f), /an OpenAI API key \(line 12\)/);
  assert.match(secretsMessage(f), /an OpenAI API key \(line 12\)/);
});

test("the share dialog never claims the file stays clean", () => {
  const s = secretsMessage([{ rule: "aws_access_key_id", line: 3 }]);
  assert.match(s, /latest content/);
  assert.match(s, /Share anyway\?$/);
});

test("no findings still phrases something rather than an empty sentence", () => {
  assert.match(secretsMessage(), /something credential-shaped/);
});
