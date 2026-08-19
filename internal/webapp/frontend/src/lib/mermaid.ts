/* Mermaid fences -> SVG, shared by the hub viewer and the /s/ share page.

   HTML string in, HTML string out: it never mutates a live DOM. The viewer
   feeds the result back through state into its single dangerouslySetInnerHTML
   (CLAUDE.md: React re-applies that markup on unrelated updates and silently
   discards post-commit DOM patches), and the share page assigns the result
   itself.

   Failure is the common path, not the edge case — hand-written diagrams in a
   wiki fail to parse often. A fence that doesn't parse, a render that throws,
   and a chunk that never loads all end up at the same place: today's
   <pre><code> block, plus a small note. One bad fence never stops the good
   ones beside it. */

const SEL = "pre > code.language-mermaid";

// Colours come from the surrounding surface rather than mermaid's stock
// palette. The hub app is dark-only; the share page follows the OS.
export type Palette = {
  bg: string; // diagram node fill
  line: string; // node borders and edges
  text: string; // labels
  accent: string; // the one highlight (cluster borders, note edges)
};

export const DARK: Palette = { bg: "#15171b", line: "#9aa0a9", text: "#eef0f3", accent: "#f5a623" };
export const LIGHT: Palette = { bg: "#f6f8fa", line: "#57606a", text: "#24292f", accent: "#b26a00" };

/** True when html contains at least one mermaid fence — cheap enough to gate
 *  the dynamic import on, so a document without one downloads no mermaid. */
export function hasMermaid(html: string): boolean {
  return html.includes('class="language-mermaid"');
}

/** Renders every mermaid fence in html and returns the new HTML. Returns html
 *  unchanged (importing nothing) when there are no fences, or when mermaid
 *  itself fails to load. */
export async function renderMermaid(html: string, palette: Palette = DARK): Promise<string> {
  if (!hasMermaid(html)) return html;
  const doc = new DOMParser().parseFromString(html, "text/html");
  const blocks = [...doc.querySelectorAll(SEL)];
  if (!blocks.length) return html;

  let mermaid;
  try {
    mermaid = (await import("mermaid")).default;
    mermaid.initialize({
      startOnLoad: false,
      // The source is teammate-authored content on a page that carries a
      // signed-in hub session. Don't relax this for htmlLabels.
      securityLevel: "strict",
      theme: "base",
      fontFamily: "inherit",
      themeVariables: {
        background: "transparent",
        primaryColor: palette.bg,
        primaryTextColor: palette.text,
        primaryBorderColor: palette.line,
        secondaryColor: palette.bg,
        tertiaryColor: palette.bg,
        lineColor: palette.line,
        textColor: palette.text,
        mainBkg: palette.bg,
        nodeBorder: palette.line,
        clusterBkg: "transparent",
        clusterBorder: palette.accent,
        titleColor: palette.text,
        edgeLabelBackground: palette.bg,
      },
    });
  } catch {
    return html; // chunk blocked or offline: today's code blocks, untouched
  }

  for (const [i, code] of blocks.entries()) {
    const pre = code.parentElement!;
    try {
      // Unique per call as well as per block: two renders of the same
      // document must not collide on an element id.
      const id = "mmd-" + Math.random().toString(36).slice(2) + "-" + i;
      const { svg } = await mermaid.render(id, code.textContent || "");
      const wrap = doc.createElement("div");
      wrap.className = "mermaid-diagram";
      wrap.innerHTML = svg;
      pre.replaceWith(wrap);
    } catch (err) {
      const note = doc.createElement("div");
      note.className = "mermaid-err";
      note.textContent = "Couldn't render this diagram.";
      pre.after(note);
      // mermaid's parse error IS the diagnostic: a line number, the offending
      // source, a caret column and the expected tokens. It lands in a sibling
      // so .mermaid-err's text stays exactly what it has always been.
      //
      // textContent, never innerHTML: the message quotes the author's source
      // verbatim, and what this helper returns is mounted through
      // dangerouslySetInnerHTML. The line number is the DIAGRAM's — the helper
      // only ever sees rendered HTML, never the .md around it.
      const msg = err instanceof Error ? err.message.trim() : "";
      if (msg) {
        const detail = doc.createElement("div");
        detail.className = "mermaid-err-detail";
        detail.textContent = msg.slice(0, 2000);
        note.after(detail);
      }
    }
  }
  // mermaid.render measures in a temporary `d<id>` element on the live body
  // and normally removes it itself; a throw mid-render can leave one behind.
  // Deliberately `dmmd-` ONLY — sweeping `mmd-` would match the SVGs already
  // MOUNTED from an earlier file and delete them, which is the post-commit
  // DOM patch this whole helper exists to avoid.
  for (const stray of document.querySelectorAll("[id^='dmmd-']")) stray.remove();
  return doc.body.innerHTML;
}
