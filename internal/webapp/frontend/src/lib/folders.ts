import type { FolderRule } from "../api/types";

/* Which folder rule governs a path: the longest matching prefix, mirroring
   ruleFor in internal/webapp/folders.go.
   
   DISPLAY ONLY. The hub decides access and already sends its answer for this
   account as `me` on each rule, so nothing here grants or refuses anything —
   it only decides which rule to NAME when telling someone where their access
   comes from. Getting it wrong shows the wrong prefix in a sentence; it
   cannot open a folder.
   
   Prefixes are slash-terminated, which is what keeps "a/" from matching a
   sibling file called "ab.md". A folder is matched on its own key, so callers
   pass `path + "/"` for a directory. */
export function ruleFor(rules: FolderRule[], key: string): FolderRule | undefined {
  let best: FolderRule | undefined;
  for (const r of rules) {
    if (!key.startsWith(r.prefix)) continue;
    if (!best || r.prefix.length > best.prefix.length) best = r;
  }
  return best;
}
