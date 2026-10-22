import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { MutableRefObject } from "react";
import type { FileItem } from "../api";
import type { DateGrouping, TimelineMonth, TimelineEndpoint } from "./Timeline";
import { galleryViewportTop } from "./galleryViewport";

export type PhotoPageRange = { query: string; first: number; count: number };
type ScrollTarget = PhotoPageRange & ({ group: string; galleryStart:boolean;galleryEnd:boolean } | { file: string; top: number });
type Options = {
  params: URLSearchParams; setParams: (params: URLSearchParams, options?: { replace?: boolean }) => void;
  query: string; first: number; count: number; pageSize: number; total: number;
  files: FileItem[]; groups: TimelineMonth[]; grouping: DateGrouping;
  loading: boolean; indexLoading: boolean; error: string; chronological: boolean; paused: boolean;
  dataRange: MutableRefObject<PhotoPageRange | null>; onJump: (rangeChanges: boolean) => void;
};

const groupElement = (key: string) => Array.from(document.querySelectorAll<HTMLElement>(".photo-group[data-group]")).find(element => element.dataset.group === key);

export default function usePhotoNavigation({ params, setParams, query, first, count, pageSize, total, files, groups, grouping, loading, indexLoading, error, chronological, paused, dataRange, onJump }: Options) {
  const top = useRef<HTMLDivElement>(null), bottom = useRef<HTMLDivElement>(null);
  const pending = useRef<ScrollTarget | null>(null), handled = useRef(""), extending = useRef(false);
  const [seekRevision, setSeekRevision] = useState(0);
  const pageCount = Math.max(1, Math.ceil(total / pageSize));
  const matches = (range: PhotoPageRange) => range.query === query && range.first === first && range.count === count;

  const jump = (group: TimelineMonth,endpoint?:TimelineEndpoint) => {
    const index = groups.findIndex(item => item.key === group.key);
    if (index < 0) return;
    const rank = groups.slice(0, index).reduce((sum, item) => sum + item.count, 0);
    const galleryStart=endpoint==="start"||endpoint!=="end"&&index===0,galleryEnd=endpoint==="end"||!galleryStart&&index===groups.length-1;
    const allPages = Math.max(1, Math.ceil(groups.reduce((sum, item) => sum + item.count, 0) / pageSize));
    const loaded = (galleryEnd?first+count>allPages:rank >= (first - 1) * pageSize) && dataRange.current && matches(dataRange.current) && groupElement(group.key);
    const targetPage = galleryEnd?allPages:Math.floor(rank / pageSize) + 1;
    const start = loaded ? first : Math.max(1, targetPage - 1);
    const size = loaded ? count : Math.min(allPages, targetPage + 1) - start + 1;
    pending.current = { query, first: start, count: size, group: group.key, galleryStart,galleryEnd };
    setSeekRevision(revision => revision + 1);
    handled.current = `${query}|${grouping}|${group.start}`;
    extending.current = false;
    const next = new URLSearchParams(params);
    for (const key of ["before", "after", "file"]) next.delete(key);
    next.set("seek", String(group.start));
    if (start === 1) next.delete("start_page"); else next.set("start_page", String(start));
    if (size === 1) next.delete("pages"); else next.set("pages", String(size));
    onJump(start !== first || size !== count);
    setParams(next, { replace: true });
  };

  // Old date-bound links become navigation cues, never media filters. A new
  // seek loads only the target page and its neighbours, without fetching the
  // entire history between the first picture and the selected date.
  useEffect(() => {
    if (!chronological || indexLoading || !groups.length) return;
    const seek = Number(params.get("seek")), before = Number(params.get("before")), after = Number(params.get("after"));
    const token = seek > 0 ? String(seek) : before > 0 ? `before:${before}` : after > 0 ? `after:${after}` : "";
    if (!token) { handled.current = ""; return; }
    if (handled.current === `${query}|${grouping}|${token}`) return;
    const stamp = seek > 0 ? seek : before > 0 ? before - 1 : after;
    const target = groups.find(group => group.start <= stamp && group.end > stamp) || [...groups].sort((a, b) => Math.abs(a.start - stamp) - Math.abs(b.start - stamp))[0];
    if (target) jump(target);
  }, [params, query, grouping, chronological, indexLoading, groups]);

  useLayoutEffect(() => {
    pending.current = null;
    extending.current = false;
  }, [query, grouping]);

  useLayoutEffect(() => {
    const target = pending.current;
    if (!target || loading || !dataRange.current || !matches(target) || !matches(dataRange.current)) return;
    const element = "group" in target ? groupElement(target.group) : Array.from(document.querySelectorAll<HTMLElement>(".fcard[data-file-id]")).find(card => card.dataset.fileId === target.file);
    if (!element) { pending.current = null; return; }
    if("group" in target&&target.galleryStart)window.scrollTo({top:0,behavior:"instant" as ScrollBehavior});
    else if("group" in target&&target.galleryEnd)window.scrollTo({top:document.documentElement.scrollHeight,behavior:"instant" as ScrollBehavior});
    else {
      const position = "group" in target ? galleryViewportTop() + 14 : target.top;
      window.scrollBy({ top: element.getBoundingClientRect().top - position, behavior: "instant" as ScrollBehavior });
    }
    pending.current = null;
  }, [files, loading, query, first, count, params, seekRevision]);

  useEffect(() => {
    if (error || !loading && dataRange.current && matches(dataRange.current)) extending.current = false;
  }, [loading, error, query, first, count, files]);

  const extend = (newer: boolean) => {
    if (extending.current || loading || error || pending.current || paused || !files.length || count >= 1000 || newer && first <= 1 || !newer && first + count > pageCount) return;
    extending.current = true;
    const next = new URLSearchParams(params);
    if (newer) {
      const cards = Array.from(document.querySelectorAll<HTMLElement>(".fcard[data-file-id]"));
      const anchor = cards.find(card => card.getBoundingClientRect().bottom > galleryViewportTop()) || cards[0];
      if (anchor?.dataset.fileId) pending.current = { query, first: first - 1, count: count + 1, file: anchor.dataset.fileId, top: anchor.getBoundingClientRect().top };
      if (first === 2) next.delete("start_page"); else next.set("start_page", String(first - 1));
    }
    next.set("pages", String(count + 1));
    setParams(next, { replace: true });
  };

  useEffect(() => {
    if (!window.IntersectionObserver || loading || error || paused || pending.current || !files.length || count >= 1000) return;
    const observer = new IntersectionObserver(entries => {
      for (const entry of entries) if (entry.isIntersecting) extend(entry.target === top.current);
    }, { rootMargin: "160px 0px 240px" });
    if (top.current && first > 1) observer.observe(top.current);
    if (bottom.current && first + count <= pageCount) observer.observe(bottom.current);
    return () => observer.disconnect();
  }, [loading, error, paused, files, first, count, pageCount, params]);

  return { top, bottom, jump, loadNewer: () => extend(true), loadOlder: () => extend(false), hasNewer: first > 1, hasOlder: first + count <= pageCount };
}
