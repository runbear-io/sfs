import { useEffect, useMemo, useState } from "react";
import { Icon } from "./shell";
import { Command, CommandInput, CommandItem, CommandList } from "@/components/ui/command";
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog";
import { fuzzyStemmed } from "@/lib/fuzzy";

/* ---- command palette (⌘K / Ctrl+K) ----
   One box for everything: fuzzy-jump to any file, switch projects, and run
   quick actions (history always, share and download on a file, sign out). */

export interface PaletteItem {
  icon: string;
  label: string;
  kind: string; // action | project | folder | file
  run: () => void;
}

function Highlight({ text, hits }: { text: string; hits: number[] }) {
  const out: React.ReactNode[] = [];
  let last = 0;
  hits.forEach((h, i) => {
    if (h > last) out.push(text.slice(last, h));
    out.push(<b key={i}>{text[h]}</b>);
    last = h + 1;
  });
  out.push(text.slice(last));
  return <span className="plabel">{out}</span>;
}

export function Palette({
  open,
  onClose,
  candidates,
}: {
  open: boolean;
  onClose: () => void;
  candidates: () => PaletteItem[];
}) {
  const [query, setQuery] = useState("");

  const items = useMemo(() => {
    if (!open) return [];
    const scored: Array<PaletteItem & { score: number; hits: number[] }> = [];
    for (const c of candidates()) {
      const m = fuzzyStemmed(query, c.label);
      if (m) scored.push({ ...c, score: m.score, hits: m.hits });
    }
    scored.sort((a, b) => b.score - a.score);
    return scored.slice(0, 40);
  }, [open, query, candidates]);

  useEffect(() => {
    if (open) setQuery("");
  }, [open]);

  const run = (item: PaletteItem) => {
    onClose();
    item.run();
  };

  // cmdk owns keyboard nav/selection/aria; our fuzzy scorer owns matching
  // and order (shouldFilter=false), so search behavior is unchanged.
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent id="palette" className="palette" showCloseButton={false} aria-describedby={undefined}>
        <DialogTitle className="sr-only">Search and quick actions</DialogTitle>
        <Command shouldFilter={false} loop>
          <div id="palette-inputwrap">
            <Icon name="search" />
            {/* No id here (nor on CommandList): cmdk writes its own after our
                props, so one never reaches the DOM. Style off #palette /
                #palette-inputwrap or [cmdk-*] instead. (BEA-54) */}
            <CommandInput
              placeholder="Search file names, projects, actions…"
              autoComplete="off"
              spellCheck={false}
              value={query}
              onValueChange={setQuery}
            />
          </div>
          <CommandList>
            {items.length === 0 ? (
              <div className="pempty">No matches — search covers file names, projects, and actions</div>
            ) : (
              items.map((item) => (
                <CommandItem
                  key={item.kind + ":" + item.label}
                  value={item.kind + ":" + item.label}
                  onSelect={() => run(item)}
                >
                  <span className="picon">
                    <Icon name={item.icon} />
                  </span>
                  <Highlight text={item.label} hits={item.hits} />
                  <span className="pkind">{item.kind}</span>
                </CommandItem>
              ))
            )}
          </CommandList>
          <footer id="palette-hint">↑↓ navigate · ⏎ select · esc close</footer>
        </Command>
      </DialogContent>
    </Dialog>
  );
}
