export const MD_EXT = /\.(md|markdown)$/i;
export const IMG_EXT = /\.(png|jpe?g|gif|svg|webp|ico|bmp|avif)$/i;
export const HTML_EXT = /\.html?$/i;
export const PDF_EXT = /\.pdf$/i;
// Checked BEFORE TEXT_EXT (csv is in both): a parse failure falls back to
// the same plain-text view TEXT_EXT would have given. Delimiter comes from
// the extension, never from sniffing the bytes.
export const CSV_EXT = /\.(csv|tsv)$/i;
export const TEXT_EXT =
  /\.(txt|log|json|ya?ml|toml|csv|go|py|js|ts|jsx|tsx|sh|bash|zsh|rb|rs|c|h|cpp|java|kt|swift|sql|css|xml|ini|conf|env|mod|sum|jsonl)$/i;

export function humanSize(n: number): string {
  if (n < 1024) return n + " B";
  const units = ["KB", "MB", "GB", "TB"];
  let i = -1;
  do {
    n /= 1024;
    i++;
  } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(1) + " " + units[i];
}

// Resolve a relative link against a directory, folding "." and "..".
export function joinPath(dir: string, rel: string): string {
  const parts = (dir ? dir.split("/") : []).concat(rel.split("/"));
  const out: string[] = [];
  for (const s of parts) {
    if (s === "" || s === ".") continue;
    if (s === "..") out.pop();
    else out.push(s);
  }
  return out.join("/");
}

/* Obsidian-style wikilink target -> file. Exact path first, then basename;
   ".md" is optional on both; everything case-insensitive. The rules are the
   product decision, so they live in one place and get tested without a
   browser (node --test has no DOM, so this cannot live in FileView). */
export function resolveWiki(target: string, files: { path: string; name: string }[]) {
  const want = target.toLowerCase();
  return (
    files.find((f) => f.path.toLowerCase() === want || f.path.toLowerCase() === want + ".md") ||
    files.find((f) => {
      const n = f.name.toLowerCase();
      return n === want || n === want + ".md";
    })
  );
}

/* clipboard copy that never throws on a non-HTTPS origin (where
   navigator.clipboard is undefined). Returns true on success. */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    /* fall through */
  }
  return false;
}

/* Last project opened on this browser. localStorage throws in Safari's
   private mode and wherever storage is disabled, so both sides swallow — a
   preference is never worth a broken page. Origin-scoped already, so two
   hubs never share an answer. */
const LAST_PROJECT = "bdrive.lastProject";

export function lastProject(): string {
  try {
    return localStorage.getItem(LAST_PROJECT) || "";
  } catch {
    return "";
  }
}

export function rememberProject(id: string) {
  try {
    localStorage.setItem(LAST_PROJECT, id);
  } catch {
    /* preference only */
  }
}

/* Whether the frontmatter panel is expanded, remembered for this browser
   the same way and with the same caveats as lastProject above. Unset (the
   first visit) is not "closed": the rail only exists on a wide window, so
   the default follows the width — expanded on desktop, a closed disclosure
   on a phone. */
const FM_PANEL = "bdrive.fmPanel";
export const FM_RAIL = "(min-width: 1400px)"; // must match style.css

export function fmPanelOpen(): boolean {
  try {
    const v = localStorage.getItem(FM_PANEL);
    if (v !== null) return v === "1";
  } catch {
    /* fall through to the width default */
  }
  return window.matchMedia(FM_RAIL).matches;
}

export function rememberFmPanel(open: boolean) {
  try {
    localStorage.setItem(FM_PANEL, open ? "1" : "0");
  } catch {
    /* preference only */
  }
}

/* Who made a change, as history renders it everywhere: the account, with
   the display name in front when the server knows one, falling back to the
   git/OS identity of an offline device. */
export function whoChanged(e: { user?: string; user_name?: string; author?: string }): string {
  return e.user_name ? `${e.user_name} <${e.user}>` : e.user || e.author || "unknown";
}
