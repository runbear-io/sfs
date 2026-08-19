/* The hub's six credential rules, in words — shared by the two surfaces that
   report the same finding: the share dialog's confirm (it refuses to mint)
   and the file view's badge (it only warns). One map, so the two wordings
   cannot drift; that shared vocabulary is the whole reason this lives in
   lib/ rather than beside either caller.

   Mirrors `labels` in internal/secrets/secrets.go, which is what `bdrive
   share` and `bdrive status` print. */

export interface SecretFinding {
  rule: string;
  line: number;
}

const SECRET_LABELS: Record<string, string> = {
  aws_access_key_id: "an AWS access key",
  openai_api_key: "an OpenAI API key",
  github_pat: "a GitHub token",
  slack_token: "a Slack token",
  private_key: "a private key",
  gitlab_pat: "a GitLab token",
};

// A rule with no label renders as its bare id: ugly, but a seventh rule that
// reported nothing would be worse.
function phrase(f: SecretFinding): string {
  return `${SECRET_LABELS[f.rule] ?? f.rule} (line ${f.line})`;
}

function list(findings: SecretFinding[]): string {
  const parts = findings.map(phrase);
  if (parts.length > 1) return parts.slice(0, -1).join(", ") + " and " + parts[parts.length - 1];
  return parts[0] || "something credential-shaped";
}

// secretsMessage phrases the 409 for the confirm dialog. The second sentence
// is not decoration: a link always serves the file's LATEST content, so the
// copy may only ever claim what was true at the moment of sharing — never
// that the file is clean.
export function secretsMessage(findings: SecretFinding[] = []): string {
  return (
    `BearDrive found ${list(findings)} in this file. The check covers the file at the moment you share it — ` +
    `a link always serves the file's latest content, so later changes are never checked. Share anyway?`
  );
}

// secretsBadge phrases the same finding for the file view, where nothing is
// blocked and nothing is redacted — a reader who can open the file could
// already read the key. Same map, so the badge and the dialog name the rule
// and the line identically.
export function secretsBadge(findings: SecretFinding[] = []): string {
  return `This file contains ${list(findings)}.`;
}
