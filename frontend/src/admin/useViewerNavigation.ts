import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { galleryApi } from "../api";
import { useToast } from "../App";

type Position = { previous: string; next: string; position: number; total: number };
type Options = { id: string | null; query: string; scope: string; api: ReturnType<typeof galleryApi>; onOpen: (id: string) => void };

// The viewer uses the complete server-side filtered order, never the currently
// mounted grid window. Adjacent positions are prefetched in a tiny local cache
// so repeated next/previous actions do not wait for another navigation request.
export default function useViewerNavigation({ id, query, scope, api, onOpen }: Options) {
  const toast = useToast();
  const [position, setPosition] = useState<{ key: string; id: string; revision: number; value: Position } | null>(null);
  const [revision, setRevision] = useState(0);
  const cache = useRef(new Map<string, Position>()), busy = useRef(false), generation = useRef(0);
  const key = `${scope}|${query}`;
  const knownScope = useRef(key);
  const retry = useCallback(() => setRevision(value => value + 1), []);
  useLayoutEffect(() => {
    busy.current = false;
    if (knownScope.current !== key) { cache.current.clear(); knownScope.current = key; generation.current++; }
    const cached = id ? cache.current.get(id) : undefined;
    setPosition(id && cached ? { key, id, revision, value: cached } : null);
  }, [id, key]);
  useEffect(() => {
    const changed = () => { generation.current++; cache.current.clear(); setPosition(null); retry(); };
    window.addEventListener("pimg:files-updated", changed);
    return () => window.removeEventListener("pimg:files-updated", changed);
  }, [retry]);
  useEffect(() => {
    if (!id) return;
    const controller = new AbortController();
    const epoch = generation.current;
    const load = async (fileID: string) => {
      const result = await api.get<Position>(`/files/navigation?${query}&id=${encodeURIComponent(fileID)}`, controller.signal);
      if (controller.signal.aborted || generation.current !== epoch) return null;
      cache.current.delete(fileID); cache.current.set(fileID, result);
      while (cache.current.size > 24) cache.current.delete(cache.current.keys().next().value!);
      return result;
    };
    void (async () => {
      try {
        const result = await load(id);
        if (!result) return;
        setPosition({ key, id, revision, value: result });
        // Warm only the two immediate neighbours; do not recursively walk the
        // library or pull all file metadata into the browser.
        await Promise.allSettled([result.previous, result.next].filter(Boolean).filter(candidate => !cache.current.has(candidate)).map(load));
      } catch (error: any) {
        if (error.name === "AbortError" || controller.signal.aborted || generation.current !== epoch) return;
        // Conversions can retire a source before its receipt switches the
        // viewer. An existing file may also be outside the active filter.
        // Both are ordinary absence from this order, not a failed operation.
        if (error.status === 404) {
          setPosition({key,id,revision,value:{previous:"",next:"",position:0,total:0}});
          return;
        }
        toast(error.message || "读取前后顺序失败", "err", { key: "viewer-navigation", onAction: retry, actionLabel: "重试" });
      }
    })();
    return () => controller.abort();
  }, [id, key, query, api, revision, toast, retry]);
  const value = position?.id === id && position.key === key && position.revision === revision ? position.value : null;
  const go = (target: string) => { if (!target || busy.current) return; busy.current = true; onOpen(target); };
  return {
    position: value ? value.position > 0 ? `${value.position} / ${value.total}` : "不在当前筛选中" : "读取顺序…",
    onPrev: value?.previous ? () => go(value.previous) : undefined,
    onNext: value?.next ? () => go(value.next) : undefined,
  };
}
