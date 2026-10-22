import { useCallback, useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { IconArrowUp, IconChevronRight, IconFile, IconFolder, IconPhoto, IconPlayerPlay, IconRefresh, IconSearch, IconStar, IconStarFilled, IconX } from "@tabler/icons-react";
import { api, fmtSize } from "../api";
import { useGallery } from "./GalleryScope";
import { useToast } from "../App";
import SelectAllControl from "./SelectAllControl";
import FilterSelect from "./FilterSelect";
import LocalImport from "./LocalImport";
import BrowserViewer from "./BrowserViewer";
import FileBookmarks, { useFileBookmarks } from "./FileBookmarks";
import useDragSelection from "./useDragSelection";
import { browserBase, browserContent, compatibleBrowserImage } from "./browserMedia";
import type { BrowserEntry, BrowserListing } from "./browserMedia";
import "./file-browser.css";

const sortOptions = [
  { value: "name", label: "名称升序" }, { value: "name_desc", label: "名称降序" },
  { value: "size_desc", label: "文件大小从大到小", section: true }, { value: "size_asc", label: "文件大小从小到大" },
  { value: "type", label: "文件类型升序", section: true }, { value: "type_desc", label: "文件类型降序" },
];
const sortLabels: Record<string, string> = { name: "名称 ↑", name_desc: "名称 ↓", size_desc: "大小 ↓", size_asc: "大小 ↑", type: "类型 ↑", type_desc: "类型 ↓" };

function SourceImage({ entry }: { entry: BrowserEntry }) {
  const ref = useRef<HTMLDivElement>(null);
  const [visible, setVisible] = useState(false), [fallback, setFallback] = useState(false);
  const [source, setSource] = useState(""), [error, setError] = useState("");
  useEffect(() => {
    const observer = new IntersectionObserver(items => { if (items.some(item => item.isIntersecting)) { setVisible(true); observer.disconnect(); } }, { rootMargin: "240px" });
    if (ref.current) observer.observe(ref.current);
    return () => observer.disconnect();
  }, []);
  useEffect(() => {
    if (!visible || !fallback) return;
    const controller = new AbortController();
    let objectURL = "";
    const load = async () => {
      if (/^(heic|heif)$/i.test(entry.ext)) {
        try {
          const { decodeHEIC } = await import("../media/heic");
          objectURL = await decodeHEIC(browserContent(entry), controller.signal);
          if (controller.signal.aborted) { URL.revokeObjectURL(objectURL); return; }
          setSource(objectURL); return;
        } catch { controller.signal.throwIfAborted(); }
      }
      const url = await compatibleBrowserImage(entry, controller.signal);
      if (!controller.signal.aborted) setSource(url);
    };
    void load().catch(cause => { if (!controller.signal.aborted) setError(cause.message); });
    return () => { controller.abort(); if (objectURL) URL.revokeObjectURL(objectURL); };
  }, [entry, visible, fallback]);
  return <div ref={ref} className="browser-image">
    {visible && (!fallback || source) && !error ? <img src={fallback ? source : browserContent(entry)} alt={entry.name} loading="lazy" decoding="async" draggable={false} onError={() => { if (fallback) setError("图片无法显示，点开重试"); else setFallback(true); }} /> : <span className="browser-placeholder"><IconPhoto size={28} stroke={1.4} /><span>{error || (visible && fallback ? "正在准备兼容图片…" : "图片")}</span></span>}
  </div>;
}

export default function FileBrowser() {
  const { actor } = useGallery(), toast = useToast();
  const storageKey = `pimg:file-locations:${actor.owner_id}`;
  const [params, setParams] = useSearchParams();
  const [initialPath] = useState(() => { try { return sessionStorage.getItem(storageKey + ":last") || ""; } catch { return ""; } });
  const path = params.get("path") ?? initialPath, query = params.get("q") || "";
  const requestedSort = params.get("sort") || "name", sort = sortOptions.some(option => option.value === requestedSort) ? requestedSort : "name";
  const [search, setSearch] = useState(query);
  const [data, setData] = useState<BrowserListing | null>(null), [loading, setLoading] = useState(true), [error, setError] = useState("");
  const bookmarks = useFileBookmarks(storageKey, data ? data.breadcrumbs[data.breadcrumbs.length - 1] || { path: data.path, name: data.path } : null);
  const [revision, setRevision] = useState(0), [more, setMore] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set()), [detail, setDetail] = useState<string | null>(null);
  const [importing, setImporting] = useState<{ paths: string[]; directory: boolean } | null>(null);
  const sequence = useRef(0), root = useRef<HTMLDivElement>(null), breadcrumbs = useRef<HTMLElement>(null), anchor = useRef<string | null>(null);
  useEffect(() => {
    const nav = breadcrumbs.current;
    if (!nav) return;
    const reveal = () => { nav.scrollLeft = nav.scrollWidth; };
    reveal(); const observer = new ResizeObserver(reveal); observer.observe(nav);
    return () => observer.disconnect();
  }, [data?.path]);
  const visit = useCallback((next: string) => { setSearch(""); setParams({ path: next, ...(sort !== "name" ? { sort } : {}) }); }, [setParams, sort]);
  useEffect(() => {
    const controller = new AbortController(), request = ++sequence.current;
    setLoading(true); setError(""); setData(null); setSelected(new Set()); setDetail(null); anchor.current = null; setMore(false);
    void api.get<BrowserListing>(`${browserBase}?${new URLSearchParams({ path, q: query, sort })}`, controller.signal).then(value => {
      if (request !== sequence.current || controller.signal.aborted) return;
      setData(value);
      try { sessionStorage.setItem(storageKey + ":last", value.path); } catch { /* Storage may be unavailable. */ }
    }).catch(cause => { if (!controller.signal.aborted && request === sequence.current) setError(cause.message); })
      .finally(() => { if (!controller.signal.aborted && request === sequence.current) setLoading(false); });
    return () => controller.abort();
  }, [path, query, sort, revision, storageKey]);
  useEffect(() => { setSearch(query); }, [query]);
  useEffect(() => {
    if (search === query) return;
    const timer = setTimeout(() => setParams({ ...(path ? { path } : {}), ...(search.trim() ? { q: search.trim() } : {}), ...(sort !== "name" ? { sort } : {}) }, { replace: true }), 300);
    return () => clearTimeout(timer);
  }, [search, query, path, sort, setParams]);
  const loadMore = async () => {
    if (!data?.next_offset || more) return;
    const request = sequence.current;
    setMore(true);
    try {
      const result = await api.get<BrowserListing>(`${browserBase}?${new URLSearchParams({ path: data.path, q: query, sort, offset: String(data.next_offset) })}`);
      if (request === sequence.current) { setData(previous => previous && { ...result, items: [...previous.items, ...result.items.filter(item => !previous.items.some(old => old.path === item.path))] }); return result; }
    } catch (cause: any) { if (request === sequence.current) toast(cause.message, "err"); }
    finally { if (request === sequence.current) setMore(false); }
  };
  const items = data?.items || [], folders = items.filter(entry => entry.directory), files = items.filter(entry => !entry.directory);
  const selectable = files.filter(entry => entry.importable), media = files.filter(entry => entry.kind === "image" || entry.kind === "video");
  const allSelected = selectable.length > 0 && selectable.every(entry => selected.has(entry.path));
  const toggle = (entry: BrowserEntry, range = false) => {
    if (!entry.importable) return;
    const start = range && anchor.current ? selectable.findIndex(value => value.path === anchor.current) : -1;
    const end = selectable.findIndex(value => value.path === entry.path);
    const targets = start >= 0 ? selectable.slice(Math.min(start, end), Math.max(start, end) + 1).map(value => value.path) : [entry.path];
    setSelected(previous => { const next = new Set(previous); if (start >= 0) targets.forEach(value => next.add(value)); else next.has(entry.path) ? next.delete(entry.path) : next.add(entry.path); return next; });
    anchor.current = entry.path;
  };
  const drag = useDragSelection({ root, enabled: !loading && !detail && !importing && selectable.length > 0, scope: `${path}:${query}:${sort}:${revision}`, selected,
    onHit: paths => setSelected(previous => { const next = new Set(previous); paths.forEach(value => next.has(value) ? next.delete(value) : next.add(value)); return next; }), onRestore: setSelected });
  const active = media.findIndex(entry => entry.path === detail), current = media[active];
  const importPaths = (paths: string[], directory = false) => {
    if (!paths.length) return;
    if (paths.length > 200) { toast("一次最多选择 200 项；也可以使用“导入当前目录”。", "err"); return; }
    setDetail(null); setImporting({ paths, directory });
  };
  const folderNav = data?.breadcrumbs || [];
  return <section className={`file-browser${selected.size ? " has-selection" : ""}`}>
    <h1 className="sr-only">文件</h1>
    <div className="file-browser-tools">
      <div className="file-browser-navigation">
        <button type="button" className="btn btn-ghost file-browser-icon file-browser-up" aria-label="上一级" title="上一级" disabled={!data || data.parent === data.path || loading} onClick={() => data && visit(data.parent)}><IconArrowUp size={17} /></button>
        <FileBookmarks locations={bookmarks.locations} currentPath={data?.path} onVisit={visit} onRemove={bookmarks.remove} />
        <nav ref={breadcrumbs} className="file-browser-breadcrumbs" aria-label="文件夹路径">{folderNav.map((item, index) => <span key={item.path}>{index > 0 && <IconChevronRight size={13} />}<button type="button" className="text-button" title={item.path} aria-current={index === folderNav.length - 1 ? "location" : undefined} onClick={() => visit(item.path)}>{item.name}</button></span>)}</nav>
        <button type="button" className="btn btn-ghost file-browser-icon" aria-label={bookmarks.saved ? "取消收藏当前目录" : "收藏当前目录"} title={bookmarks.saved ? "取消收藏当前目录" : "收藏当前目录"} aria-pressed={bookmarks.saved} disabled={!data || loading} onClick={bookmarks.toggle}>{bookmarks.saved ? <IconStarFilled size={17} /> : <IconStar size={17} />}</button>
        <button type="button" className="btn btn-ghost file-browser-icon" aria-label="刷新目录" title="刷新目录" disabled={loading} onClick={() => setRevision(value => value + 1)}><IconRefresh size={17} /></button>
      </div>
      <div className="file-browser-actions">
        <SelectAllControl checked={allSelected} indeterminate={selected.size > 0 && !allSelected} disabled={!selectable.length || loading} scope="全选已加载的可导入文件" onChange={() => setSelected(allSelected ? new Set() : new Set(selectable.map(entry => entry.path)))} />
        <label className="file-browser-search"><IconSearch size={16} /><input className="input" aria-label="搜索当前目录" placeholder="搜索当前目录" value={search} onChange={event => setSearch(event.target.value)} />{search && <button type="button" className="btn btn-ghost" aria-label="清除文件搜索" onClick={() => setSearch("")}><IconX size={15} /></button>}</label>
        <FilterSelect label="文件排序" value={sort} displayLabel={sortLabels[sort]} options={sortOptions} active={sort !== "name"} width={208} onChange={value => setParams({ path: data?.path || path, ...(search.trim() ? { q: search.trim() } : {}), ...(value !== "name" ? { sort: value } : {}) })} />
        <span className="file-browser-count" role="status">{data ? `${data.total} 项` : loading ? "读取中…" : ""}</span>
        <span className="file-browser-readonly">源文件只读</span>
        <button type="button" className="btn" disabled={!data || loading || !!query} title={query ? "清除搜索后可导入当前目录" : "选择相册后导入，可包含子目录"} onClick={() => data && importPaths([data.path], true)}>导入当前目录</button>
      </div>
    </div>
    <div className="file-browser-content" aria-busy={loading}>
      {error ? <div className="empty" role="alert"><p>{error}</p><p className="detail-hint">请检查目录的读取权限及 Docker 挂载，或返回默认目录继续浏览。</p><button type="button" className="btn" onClick={() => setRevision(value => value + 1)}>重新读取</button><button type="button" className="btn btn-ghost" onClick={() => visit("")}>返回默认目录</button></div> : loading ? <div className="empty" role="status">正在读取目录…</div> : !items.length ? <div className="empty"><IconFolder size={32} stroke={1.3} /><p>{query ? "当前目录没有匹配的文件或文件夹" : "这个文件夹是空的"}</p>{query && <button type="button" className="btn" onClick={() => setSearch("")}>清除搜索</button>}</div> : <>
        {!!folders.length && <div className="file-browser-folders" aria-label="子文件夹">{folders.map(entry => <button type="button" className="file-browser-folder" key={entry.path} title={entry.path} onClick={() => visit(entry.path)}><IconFolder size={23} stroke={1.5} /><span>{entry.name}</span><IconChevronRight size={15} /></button>)}</div>}
        <div ref={root} className={`file-browser-grid${drag.active ? " is-drag-selecting" : ""}`} {...drag.bind}>
          {files.map(entry => <article key={entry.path + entry.version} data-file-id={entry.path} data-selection-disabled={!entry.importable ? "true" : undefined} className={`fcard browser-file${selected.has(entry.path) ? " selected" : ""}`}>
            {entry.importable && <button type="button" className="card-check" role="checkbox" aria-checked={selected.has(entry.path)} aria-label={`选择文件 ${entry.name}`} onClick={event => toggle(entry, event.shiftKey)}><span className="check" data-on={selected.has(entry.path)}><svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true"><path d="m3 8 3 3 5-6" /></svg></span></button>}
            <button type="button" className="card-open" disabled={entry.kind === "other"} aria-label={`查看文件 ${entry.name}`} onClick={event => { if (selected.size && entry.importable) toggle(entry, event.shiftKey); else setDetail(entry.path); }}>
              {entry.kind === "image" ? <SourceImage entry={entry} /> : <div className="browser-placeholder">{entry.kind === "video" ? <IconPlayerPlay size={32} stroke={1.3} /> : <IconFile size={30} stroke={1.3} />}<span>{entry.kind === "video" ? "点击播放" : "暂不支持在线查看"}</span></div>}
            </button>
            <div className="browser-file-caption"><span title={entry.name}>{entry.name}</span><small>{entry.ext.toUpperCase() || "文件"} · {fmtSize(entry.size)}{entry.kind !== "other" && !entry.importable ? " · 未启用导入" : ""}</small></div>
          </article>)}
        </div>
        {!!data?.next_offset && <div className="file-browser-more"><button type="button" className="btn" disabled={more} onClick={() => void loadMore()}>{more ? "正在读取…" : `加载更多（已显示 ${items.length} / ${data.total}）`}</button></div>}
      </>}
    </div>
    {selected.size > 0 && <div className="batch-bar file-browser-batch" aria-label="文件导入操作"><span role="status">已选 {selected.size} 个文件</span><button type="button" className="btn btn-ghost" onClick={() => setSelected(new Set())}>取消选择</button><button type="button" className="btn btn-crayon" onClick={() => importPaths([...selected])}>导入到相册</button></div>}
    {current && <BrowserViewer key={current.path + current.version} entry={current} position={`${active + 1} / ${media.length}${data?.next_offset ? "+" : ""}`} selected={selected.has(current.path)} onSelect={() => toggle(current)} onPrev={active > 0 ? () => setDetail(media[active - 1].path) : undefined} onNext={active + 1 < media.length ? () => setDetail(media[active + 1].path) : data?.next_offset ? () => { void loadMore().then(result => { const next = result?.items.find(entry => entry.kind === "image" || entry.kind === "video"); if (next) setDetail(next.path); }); } : undefined} onClose={() => setDetail(null)} onImport={() => importPaths([current.path])} />}
    {importing && <LocalImport albums={[]} paths={importing.paths} directoryImport={importing.directory} onClose={() => setImporting(null)} />}
  </section>;
}
