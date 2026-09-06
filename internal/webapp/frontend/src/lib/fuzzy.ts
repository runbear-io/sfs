/* The ⌘K palette's matcher. Lives here, not in the component, so it can be
   unit-tested without a DOM (see fuzzy.test.ts). Palette.tsx keeps only
   rendering. */

/* Subsequence fuzzy match. Returns a score (higher = better), plus the
   matched positions for highlighting; null when it doesn't match. */
export function fuzzy(query: string, text: string): { score: number; hits: number[] } | null {
  if (!query) return { score: 0, hits: [] };
  const q = query.toLowerCase();
  const t = text.toLowerCase();
  let ti = 0,
    score = 0,
    streak = 0;
  const hits: number[] = [];
  for (let qi = 0; qi < q.length; qi++) {
    const found = t.indexOf(q[qi], ti);
    if (found === -1) return null;
    streak = found === ti ? streak + 1 : 1;
    score += streak * 3; // consecutive runs
    if (found === 0 || "/ -_.".includes(t[found - 1])) score += 8; // word starts
    hits.push(found);
    ti = found + 1;
  }
  score -= Math.floor(t.length / 8); // mild preference for short targets
  return { score, hits };
}

/* Match a query against a label, tolerating a simple English plural so
   "ideas" still finds idea.md. Tries the raw query first, then a lightly
   de-pluralized form (…ies→…y, …es→…, …s→…). */
export function fuzzyStemmed(query: string, label: string) {
  const m = fuzzy(query, label);
  if (m) return m;
  const q = query.toLowerCase();
  let stem: string | null = null;
  if (q.length > 3 && q.endsWith("ies")) stem = q.slice(0, -3) + "y";
  else if (q.length > 3 && q.endsWith("es")) stem = q.slice(0, -2);
  else if (q.length > 2 && q.endsWith("s")) stem = q.slice(0, -1);
  return stem ? fuzzy(stem, label) : null;
}

/* Flat, per-errored-token. Large enough that no run/word-start bonus can
   climb over it, which makes "exact always beats approximate" a property
   rather than a tuning accident. */
const ERROR_PENALTY = 1000;
/* Paid per token whose match starts in the file name rather than an
   ancestor directory, so docs/install-guide.md beats a path whose letters
   merely scatter across directory names. */
const BASENAME_BONUS = 12;
/* Below this, a one-character error matches nearly everything. */
const MIN_ERROR_LEN = 4;

/* Score a whole query against one candidate label.

   The query is split on whitespace and every token must match (AND) — file
   paths use "/", "-" and "_", never spaces, so before this a query with a
   space in it matched nothing at all. Token order is irrelevant; scores are
   summed. With `allowError`, a token of 4+ characters that missed is retried
   with each of its single-character deletions: subsequence matching already
   tolerates a character missing from the TARGET, so that one rule covers a
   transposition, a substitution and an insertion (fileveiw → fileviw). Hits
   come back sorted and de-duplicated — Highlight slices forward and would
   silently duplicate letters otherwise. */
export function scoreLabel(
  query: string,
  label: string,
  opts?: { allowError?: boolean },
): { score: number; hits: number[] } | null {
  // Empty query: score 0 for everything, no hits. The palette's sort is
  // stable, so this is what keeps the project's own destinations on top of
  // an empty palette (BEA-52, BEA-105). Must return before splitting.
  if (!query.trim()) return { score: 0, hits: [] };
  const base = label.lastIndexOf("/") + 1;
  let score = 0;
  const hits: number[] = [];
  for (const token of query.trim().split(/\s+/)) {
    let m = fuzzyStemmed(token, label);
    let errored = false;
    if (!m && opts?.allowError && token.length >= MIN_ERROR_LEN) {
      for (let i = 0; i < token.length && !m; i++) {
        m = fuzzyStemmed(token.slice(0, i) + token.slice(i + 1), label);
        errored = !!m;
      }
    }
    if (!m) return null;
    score += m.score;
    if (errored) score -= ERROR_PENALTY;
    if (m.hits.length > 0 && m.hits[0] >= base) score += BASENAME_BONUS;
    hits.push(...m.hits);
  }
  return { score, hits: [...new Set(hits)].sort((a, b) => a - b) };
}
