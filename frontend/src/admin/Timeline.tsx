import FloatingFeedback from "../FloatingFeedback";
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useGallery } from "./GalleryScope";
import type { FileItem } from "../api";
import { fileMediaTime } from "../api";
import SelectAllControl from "./SelectAllControl";
import { galleryViewportTop } from "./galleryViewport";

export type DateGrouping = "day" | "week" | "month" | "year";
export type TimelineEndpoint = "start" | "end";
export type TimelineMonth = { key: string; count: number; start: number; end: number };
export const browserOffset = -new Date().getTimezoneOffset();
const shiftedDate = (stamp: number) => new Date((stamp + browserOffset * 60) * 1000);
const dateKey = (d: Date) => `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, "0")}-${String(d.getUTCDate()).padStart(2, "0")}`;
export const monthKey = (stamp: number) => dateKey(shiftedDate(stamp)).slice(0, 7);
export function bucketKey(stamp: number, grouping: DateGrouping) {
  const date = shiftedDate(stamp);
  if (grouping === "year") return dateKey(date).slice(0, 4);
  if (grouping === "month") return dateKey(date).slice(0, 7);
  if (grouping === "week") date.setUTCDate(date.getUTCDate() - (date.getUTCDay() + 6) % 7);
  return dateKey(date);
}
export function bucketRange(stamp: number, grouping: DateGrouping): TimelineMonth {
  const key = bucketKey(stamp, grouping), date = new Date(key.length === 4 ? `${key}-01-01T00:00:00Z` : key.length === 7 ? `${key}-01T00:00:00Z` : `${key}T00:00:00Z`);
  const start = date.getTime() / 1000 - browserOffset * 60;
  if (grouping === "year") date.setUTCFullYear(date.getUTCFullYear() + 1); else if (grouping === "month") date.setUTCMonth(date.getUTCMonth() + 1); else date.setUTCDate(date.getUTCDate() + (grouping === "week" ? 7 : 1));
  return { key, count: 0, start, end: date.getTime() / 1000 - browserOffset * 60 };
}
const calendarLabel = (stamp: number, year = false) => shiftedDate(stamp).toLocaleDateString("zh-CN", { timeZone: "UTC", year: year ? "numeric" : undefined, month: "long", day: "numeric" });
export function groupLabel(group: TimelineMonth, grouping: DateGrouping, fullYear = false) {
  if (grouping === "year") return `${group.key}年`;
  if (grouping === "month") return `${group.key.slice(0, 4)}年${Number(group.key.slice(5))}月`;
  const year = fullYear || group.key.slice(0, 4) !== String(shiftedDate(Date.now() / 1000).getUTCFullYear());
  if (grouping === "week") return `${calendarLabel(group.start, year)} – ${calendarLabel(group.end - 1, year && monthKey(group.start).slice(0, 4) !== monthKey(group.end - 1).slice(0, 4))}`;
  const today = bucketKey(Date.now() / 1000, "day"), yesterday = bucketKey(Date.now() / 1000 - 86400, "day");
  return !fullYear && group.key === today ? "今天" : !fullYear && group.key === yesterday ? "昨天" : calendarLabel(group.start, year);
}

export function useLibraryTimeline(query: string, enabled: boolean, grouping: DateGrouping) {
  const {api} = useGallery();
  const ascending = new URLSearchParams(query).get("sort") === "oldest";
  const scope = useMemo(() => { const q = new URLSearchParams(query); for (const key of ["before", "after", "per_page", "page", "sort"]) q.delete(key); q.set("tz_offset", String(browserOffset)); q.set("group", grouping); return q.toString(); }, [query, grouping]);
  const initial=api.cached<{groups?:TimelineMonth[];months?:TimelineMonth[]}>(`/files/timeline?${scope}`);
  const initialGroups=initial?.groups||(grouping==="month"?initial?.months:[])||[];
  const [groups, setGroups] = useState<TimelineMonth[]>(()=>ascending?[...initialGroups].reverse():initialGroups), [error, setError] = useState("");
  const [loading, setLoading] = useState(enabled&&!initial), [revision, setRevision] = useState(0), [visibleGroup, setVisibleGroup] = useState("");
  const loadedScope = useRef(initial?`${scope}:${ascending}`:"");
  const retry = useCallback(() => setRevision(v => v + 1), []);
  useEffect(() => { window.addEventListener("pimg:files-updated", retry); return () => window.removeEventListener("pimg:files-updated", retry); }, [retry]);
  useEffect(() => {
    const controller = new AbortController(), key = `${scope}:${ascending}`;
    if (!enabled) { loadedScope.current = ""; setGroups([]); setLoading(false); return; }
    if (loadedScope.current !== key) { const cached=api.cached<{groups?:TimelineMonth[];months?:TimelineMonth[]}>(`/files/timeline?${scope}`);const list=cached?.groups||(grouping==="month"?cached?.months:[])||[];setLoading(!cached); setError(""); setGroups(ascending?[...list].reverse():list);if(cached)loadedScope.current=key; }
    void api.get<{ groups?: TimelineMonth[]; months?: TimelineMonth[] }>(`/files/timeline?${scope}`, controller.signal).then(response => {
      if (!controller.signal.aborted) { const items = response.groups || (grouping === "month" ? response.months || [] : []), ordered = ascending ? [...items].reverse() : items; loadedScope.current = key; setGroups(previous => previous.length === ordered.length && previous.every((group,index) => group.key === ordered[index].key && group.start === ordered[index].start && group.end === ordered[index].end && group.count === ordered[index].count) ? previous : ordered); setError(""); }
    }).catch(e => { if (!controller.signal.aborted) setError(e.message || "日期索引读取失败"); }).finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [scope, revision, enabled, ascending, grouping, api]);
  useEffect(() => {
    if (!enabled) return;
    let frame = 0;
    const update = () => { cancelAnimationFrame(frame); frame = requestAnimationFrame(() => {
      const sections = Array.from(document.querySelectorAll<HTMLElement>(".photo-group[data-group]"));
      const top = galleryViewportTop();
      const atEnd=window.scrollY>0&&window.scrollY+window.innerHeight>=document.documentElement.scrollHeight-2;
      const first = atEnd?sections[sections.length-1]:sections.find(section => section.getBoundingClientRect().bottom > top);
      if (first?.dataset.group) setVisibleGroup(first.dataset.group);
    }); };
    update(); window.addEventListener("scroll", update, { passive: true }); window.addEventListener("resize", update);
    const observer = new MutationObserver(update); const grid = document.querySelector(".library"); if (grid) observer.observe(grid, { childList: true, subtree: true });
    return () => { cancelAnimationFrame(frame); observer.disconnect(); window.removeEventListener("scroll", update); window.removeEventListener("resize", update); };
  }, [enabled, query, grouping]);
  return { groups, error, loading, retry, visibleGroup };
}

export function TimeIndex({ groups, grouping, current, loading, error, onRetry, onJump }: { groups: TimelineMonth[]; grouping: DateGrouping; current: string; loading: boolean; error: string; onRetry: () => void; onJump: (group: TimelineMonth,endpoint?:TimelineEndpoint) => void }) {
  const track = useRef<HTMLDivElement>(null), pending = useRef<number>(), dragging = useRef<number | null>(null);
  const queued = useRef<{group:TimelineMonth;endpoint?:TimelineEndpoint} | null>(null), lastJump = useRef("");
  const [active, setActive] = useState(0), [isDragging, setDragging] = useState(false), [height, setHeight] = useState(360), [hovered, setHovered] = useState<number | null>(null);
  const commit = useRef(onJump); commit.current = onJump;
  const max = Math.max(0, groups.length - 1), unit = grouping === "day" ? "日" : grouping === "week" ? "周" : grouping === "year" ? "年" : "月";
  const unavailable = !groups.length || loading;
  const cancelPending = () => { if (pending.current !== undefined) cancelAnimationFrame(pending.current); pending.current = undefined; queued.current = null; };
  const stopDragging = () => { const pointer = dragging.current; dragging.current = null; setDragging(false); if (pointer !== null && track.current?.hasPointerCapture(pointer)) track.current.releasePointerCapture(pointer); };
  useEffect(() => { if (dragging.current === null) setActive(Math.max(0, groups.findIndex(group => group.key === current))); }, [current, groups]);
  useLayoutEffect(() => { cancelPending(); stopDragging(); lastJump.current = ""; setHovered(null); setActive(index => Math.min(max, index)); return cancelPending; }, [groups, grouping, unavailable]);
  useEffect(() => { const el = track.current; if (!el || !window.ResizeObserver) return; const observer = new ResizeObserver(entries => setHeight(entries[0].contentRect.height)); observer.observe(el); return () => observer.disconnect(); }, []);
  const at = (y: number) => {
    const rect = track.current!.getBoundingClientRect(),grip=track.current!.querySelector(".time-grip")?.getBoundingClientRect().height||32;
    // The handle overlaps the first tick before its centre reaches the exact
    // track coordinate. Snap the visible handle's end zone to the real endpoint.
    const edge=Math.min(rect.height/4,grip/2+2);
    if(y<=rect.top+edge)return {index:0,endpoint:"start" as const};
    if(y>=rect.bottom-edge)return {index:max,endpoint:"end" as const};
    return {index:Math.round(Math.min(1, Math.max(0, (y - rect.top) / Math.max(1, rect.height))) * max),endpoint:undefined};
  };
  const flush = () => { pending.current = undefined; const target = queued.current; queued.current = null; if (!target)return;const token=`${target.group.key}:${target.endpoint||"group"}`;if(dragging.current!==null&&lastJump.current===token)return;lastJump.current=token;commit.current(target.group,target.endpoint); };
  const jump = (index: number, immediate = false,endpoint?:TimelineEndpoint) => {
    const group = groups[index]; if (!group) return;
    setActive(index); queued.current = {group,endpoint};
    // A trailing debounce never runs during a continuous drag. Coalesce only
    // events in the same paint frame so the gallery follows the held pointer.
    if (immediate) { if (pending.current !== undefined) cancelAnimationFrame(pending.current); flush(); }
    else if (pending.current === undefined) pending.current = requestAnimationFrame(flush);
  };
  const anchors: { label: string; index: number }[] = [];
  for (let i = 0; i < groups.length; i++) {
    const previous = anchors[anchors.length - 1], year = groups[i].key.slice(0, 4), month = monthKey(groups[i].start);
    const label = !previous || year !== groups[previous.index].key.slice(0, 4) ? year : (grouping === "day" || grouping === "week") && month !== monthKey(groups[previous.index].start) ? `${Number(month.slice(5))}月` : "";
    if (label && (!previous || ((i - previous.index) / Math.max(1, max)) * height >= 38)) anchors.push({ label, index: i });
  }
  const value = groups[Math.min(active, max)];
  return <aside className={`time-index${isDragging ? " scrubbing" : hovered !== null ? " hovering" : ""}`} onPointerLeave={() => setHovered(null)} aria-label={`按上传日期浏览 · 按${unit}`}>
    {loading ? <span className="time-index-message" role="status">读取日期…</span> : error ? <FloatingFeedback className="time-index-message" role="alert"><span>{groups.length ? "日期更新失败" : "日期读取失败"}</span><button className="btn btn-sm" onClick={onRetry}>重试</button></FloatingFeedback> : !groups.length ? <span className="time-index-message">暂无日期</span> : null}
    <div ref={track} className={`time-track${isDragging ? " is-dragging" : ""}`} role="slider" tabIndex={unavailable ? -1 : 0} aria-label={`拖动时间线跳转${unit}分区`} aria-orientation="vertical" aria-valuemin={0} aria-valuemax={max} aria-valuenow={Math.min(active, max)} aria-valuetext={value ? `${groupLabel(value, grouping, true)}，${value.count}项` : "暂无日期"} aria-disabled={unavailable}
      onPointerDown={event => { if (unavailable || event.button !== 0 || dragging.current !== null) return; event.preventDefault(); track.current?.focus({ preventScroll: true }); event.currentTarget.setPointerCapture(event.pointerId); dragging.current = event.pointerId; lastJump.current = ""; setDragging(true);const target=at(event.clientY);jump(target.index,true,target.endpoint); }}
      onPointerMove={event => { const target = at(event.clientY); setHovered(target.index); if (dragging.current === event.pointerId) jump(target.index,false,target.endpoint); }}
      onPointerUp={event => { if (dragging.current !== event.pointerId) return;const target=at(event.clientY);jump(target.index,true,target.endpoint); stopDragging(); }}
      onPointerCancel={event => { if (dragging.current === event.pointerId) { cancelPending(); stopDragging(); } }} onLostPointerCapture={event => { if (dragging.current === event.pointerId) { cancelPending(); stopDragging(); } }}
      onKeyDown={event => { let index = active; if (["ArrowDown", "ArrowRight"].includes(event.key)) index++; else if (["ArrowUp", "ArrowLeft"].includes(event.key)) index--; else if (event.key === "Home") index = 0; else if (event.key === "End") index = max; else if (event.key === "PageDown") index += grouping === "year" ? 1 : grouping === "month" ? 12 : grouping === "week" ? 4 : 7; else if (event.key === "PageUp") index -= grouping === "year" ? 1 : grouping === "month" ? 12 : grouping === "week" ? 4 : 7; else return; event.preventDefault(); jump(Math.max(0, Math.min(max, index)), true,event.key==="Home"?"start":event.key==="End"?"end":undefined); }}>
      {groups.map((group,index) => index % Math.max(1, Math.ceil(groups.length / 80)) === 0 || index === max ? <span key={group.key} className="time-tick" style={{ top: `${index / Math.max(1,max) * 100}%`, ["--tick-pull" as string]: Math.max(0, 1 - Math.abs(index - (isDragging ? active : hovered ?? active)) / Math.max(4, groups.length / 20)) }} /> : null)}
      {anchors.map(({ label, index }) => <span key={index} className="time-year" style={{ top: `${index / Math.max(1, max) * 100}%` }}>{label}</span>)}
      {value && !unavailable && <span className="time-thumb" style={{ top: `${Math.min(active, max) / Math.max(1, max) * 100}%` }}><span className="time-grip"><svg width="12" height="20" viewBox="0 0 12 20" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden="true"><path d="m3 7 3-3 3 3M3 13l3 3 3-3" /></svg></span><span className={`time-bubble time-bubble-${grouping}`}><strong>{groupLabel(value, grouping)}</strong><small>{value.key.slice(0,4)} · 按{unit} · {value.count} 项</small></span></span>}
    </div>
  </aside>;
}

function photoColumns(ratios: number[]) {
  // Tracks totalling less than 1fr leave unused space. Scale the weights so
  // even a row containing only narrow portraits fills its measured width.
  const unit = Math.min(...ratios);
  return ratios.map(ratio => `${ratio / unit}fr`).join(" ");
}

export function PhotoGroups({ files, chronological, grouping, index, selected, selectionFiles, selecting, disabled, onSelectGroup, render }: { files: FileItem[]; chronological: boolean; grouping: DateGrouping; index: TimelineMonth[]; selected: Set<string>; selectionFiles: FileItem[]; selecting: string; disabled: boolean; onSelectGroup: (group: TimelineMonth, clear: boolean) => void; render: (file: FileItem) => ReactNode }) {
  const canvas = useRef<HTMLDivElement>(null), [width, setWidth] = useState(Math.max(260, window.innerWidth - 170));
  useLayoutEffect(() => { const el = canvas.current; if (!el) return; const measured=el.getBoundingClientRect().width;if(measured>0)setWidth(measured);if(!window.ResizeObserver)return; const observer = new ResizeObserver(entries => {if(entries[0].contentRect.width>0)setWidth(entries[0].contentRect.width)}); observer.observe(el); return () => observer.disconnect(); }, []);
  const groups = useMemo(() => {
    const result: { bucket: TimelineMonth; files: FileItem[] }[] = [];
    for (const file of files) { const key = chronological ? bucketKey(fileMediaTime(file), grouping) : "all"; let group = result[result.length - 1]; if (group?.bucket.key !== key) { group = { bucket: chronological ? index.find(item => item.key === key) || bucketRange(fileMediaTime(file), grouping) : { key, start:0, end:0, count:files.length }, files: [] }; result.push(group); } group.files.push(file); }
    return result;
  }, [files, chronological, grouping, index]);
  return <div ref={canvas} className="lib-grid date-grid">{groups.map(group => {
    const rows: { files: FileItem[]; ratios: number[]; height: number }[] = []; const target = width < 600 ? 138 : 180;
    let row: FileItem[] = [], ratios: number[] = [], sum = 0;
    const finish = (full = false) => { if (!row.length) return; rows.push({ files: row, ratios, height: full ? (width - (row.length - 1) * 8) / sum : Math.min(target, (width - (row.length - 1) * 8) / sum) }); row = []; ratios = []; sum = 0; };
    for (const file of group.files) {
      // The tile displays the preview, whose orientation can differ from the
      // source metadata (or be known before that metadata is available).
      const ratio = file.thumb_w > 0 && file.thumb_h > 0 ? file.thumb_w / file.thumb_h : file.width > 0 && file.height > 0 ? file.width / file.height : 4 / 3;
      row.push(file); ratios.push(ratio); sum += ratio;
      if (sum * target + (row.length - 1) * 8 >= width || (width < 600 && row.length === 2)) finish(true);
    } finish();
    const known = selectionFiles.filter(file => bucketKey(fileMediaTime(file), grouping) === group.bucket.key);
    const full = known.length >= group.bucket.count && known.filter(file => !file.purge_state).length > 0 && known.filter(file => !file.purge_state).every(file => selected.has(file.id));
    return <section className="photo-group" data-group={group.bucket.key} data-month={monthKey(group.bucket.start)} key={group.bucket.key}>{chronological && <div className="photo-group-heading"><SelectAllControl className="group-select" checked={full} indeterminate={known.some(file=>!file.purge_state&&selected.has(file.id))} disabled={disabled || !!selecting} busy={selecting === group.bucket.key} scope={`全选${groupLabel(group.bucket, grouping)}分区`} onChange={() => onSelectGroup(group.bucket, full)}/><h2>{groupLabel(group.bucket, grouping)}<span>{group.bucket.count || group.files.length} 项</span></h2></div>}{rows.map((row, i) => <div className="photo-row" key={i} style={{ gridTemplateColumns: photoColumns(row.ratios), maxWidth: row.height * row.ratios.reduce((a, b) => a + b, 0) + (row.files.length - 1) * 8, ["--photo-height" as string]: `${row.height}px` }}>{row.files.map(render)}</div>)}</section>;
  })}</div>;
}
