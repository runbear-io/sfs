import { useQuery } from "@tanstack/react-query";
import { getResponse } from "../api/http";
import { MAX_BYTES, sniffBytes, type BlobText } from "../lib/sniff";

// One URL's bytes, decoded as text when they are text. The sniff itself
// lives in lib/sniff.ts (pure, so the unit test can import it without
// dragging in React Query); what stays here is the HTTP part — the
// Content-Length cheap-out — and the caching policy.

export type { BlobText };

export function blobURL(apiBase: string, sha: string, name?: string, download?: boolean): string {
  let u = apiBase + "blob?sha=" + encodeURIComponent(sha);
  if (name) u += "&name=" + encodeURIComponent(name);
  if (download) u += "&download=1";
  return u;
}

export async function fetchBlobText(url: string): Promise<BlobText> {
  const r = await getResponse(url);
  // Cheap out before reading the body when the server tells us the size.
  // Content-Length is a hint, not a guarantee (a chunked or proxied
  // response may omit it), so sniffBytes checks the real length too.
  const len = Number(r.headers.get("Content-Length"));
  if (len > MAX_BYTES) return { kind: "too-large", size: len };
  return sniffBytes(new Uint8Array(await r.arrayBuffer()));
}

// The URL a file page reads its bytes from: content-addressed when a version
// is pinned, the live path otherwise. Exported so Copy builds the same URL
// the view does instead of a second copy of the expression that can drift.
// A version is served by content hash; ?name= is what makes the server set a
// real Content-Type, so images and text render instead of downloading as
// octet-stream. Note `file?path=`, not the `download?path=` an <a download>
// points at — same bytes, but Content-Disposition is meaningless to a fetch.
export function fileURLFor(apiBase: string, path: string, version?: string): string {
  return version ? blobURL(apiBase, version, path) : apiBase + "file?path=" + encodeURIComponent(path);
}

// `immutable` is for content-addressed URLs: a sha's bytes never change, so
// staleness cannot apply and re-expanding a history row costs no request. A
// live path must never be pinned that way — a teammate's edit would keep
// serving the old body.
export function useTextAt(url: string, key: unknown[], enabled: boolean, immutable: boolean) {
  return useQuery({
    queryKey: key,
    queryFn: () => fetchBlobText(url),
    enabled,
    ...(immutable ? { staleTime: Infinity, gcTime: Infinity } : {}),
    retry: false,
  });
}

export function useBlobText(apiBase: string, sha: string | undefined, enabled: boolean) {
  return useTextAt(sha ? blobURL(apiBase, sha) : "", ["blob", apiBase, sha], enabled && !!sha, true);
}
