import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../api";
import type { HomeItem } from "../api";

type Page = { items: HomeItem[]; total: number; per_page?: number };
export default function useHomeFeed(perPage?: number) {
  const [items, setItems] = useState<HomeItem[]>([]), [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true), [error, setError] = useState("");
  const pages = useRef(1), sequence = useRef(0), controller = useRef<AbortController | null>(null);
  const inFlight = useRef(false), queued = useRef(false), lastRequest = useRef(0);
  const request = useCallback(async function run(more: boolean, force = true) {
    if (inFlight.current) { if (!more && force) queued.current = true; return; }
    if (!more && !force && Date.now() - lastRequest.current < 1000) return;
    lastRequest.current = Date.now(); inFlight.current = true;
    const serial = ++sequence.current, abort = new AbortController(); controller.current = abort;
    setLoading(true);
    try {
      const wanted = more ? pages.current + 1 : pages.current;
      let next: HomeItem[] = [], count = 0, completed = more ? pages.current : 0;
      for (let p = more ? wanted : 1; p <= wanted; p++) {
        const result = await api.get<Page>(`/api/home?page=${p}${perPage ? `&per_page=${perPage}` : ""}`, abort.signal);
        if (serial !== sequence.current) return;
        next.push(...result.items); count = result.total; completed = p;
        if (p * (result.per_page || perPage || 24) >= count) break;
      }
      if (serial !== sequence.current) return;
      pages.current = Math.max(1, completed);
      setItems(previous => { const all = more ? [...previous, ...next] : next; const seen = new Set<string>(); return all.filter(item => { if (seen.has(item.id)) return false; seen.add(item.id); return true; }); });
      setTotal(count); setError("");
    } catch (cause: any) {
      if (serial === sequence.current && cause.name !== "AbortError") setError(cause.message || "首页暂时无法读取，请重试。");
    } finally {
      if (serial === sequence.current) {
        inFlight.current = false; setLoading(false);
        if (queued.current) { queued.current = false; void run(false); }
      }
    }
  }, [perPage]);
  useEffect(() => {
    pages.current = 1; inFlight.current = false; queued.current = false; void request(false);
    const foreground = () => { if (document.visibilityState === "visible") void request(false, false); };
    const changed = () => { if (document.visibilityState === "visible") void request(false); };
    const stored = (event: StorageEvent) => { if (event.key === "pimg:files-updated") changed(); };
    window.addEventListener("focus", foreground); window.addEventListener("pageshow", foreground);
    window.addEventListener("pimg:files-updated", changed); window.addEventListener("storage", stored);
    document.addEventListener("visibilitychange", foreground);
    // Metadata only. Hidden tabs never poll or fetch image bytes.
    const timer = window.setInterval(foreground, 15000);
    return () => {
      sequence.current++; controller.current?.abort(); inFlight.current = false; queued.current = false;
      clearInterval(timer); window.removeEventListener("focus", foreground); window.removeEventListener("pageshow", foreground);
      window.removeEventListener("pimg:files-updated", changed); window.removeEventListener("storage", stored);
      document.removeEventListener("visibilitychange", foreground);
    };
  }, [request]);
  return { items, total, loading, error, refresh: () => request(false), loadMore: () => request(true) };
}
