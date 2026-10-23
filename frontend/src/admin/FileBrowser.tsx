import { useCallback, useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { IconArrowDown, IconArrowUp, IconChevronRight, IconFile, IconFolder, IconFolders, IconPhoto, IconPlayerPlay, IconRefresh, IconSearch, IconStar, IconStarFilled, IconX } from "@tabler/icons-react";
import { api, fmtSize } from "../api";
import { useGallery } from "./GalleryScope";
import { useToast } from "../App";
import SelectAllControl from "./SelectAllControl";
import FilterSelect from "./FilterSelect";
import LocalImport from "./LocalImport";
import BrowserViewer from "./BrowserViewer";
import FileBookmarks, { useFileBookmarks } from "./FileBookmarks";
import useDragSelection from "./useDragSelection";
import { browserBase } from "./browserMedia";
import type { BrowserEntry, BrowserListing } from "./browserMedia";
import "./file-browser.css";

const sortOptions = [
  { value: "name", label: "名称升序" }, { value: "name_desc", label: "名称降序" },
  { value: "size_desc", label: "文件大小从大到小", section: true }, { value: "size_asc", label: "文件大小从小到大" },
  { value: "type", label: "文件类型升序", section: true }, { value: "type_desc", label: "文件类型降序" },
  { value: "created_desc", label: "创建时间从新到旧", section: true }, { value: "created_asc", label: "创建时间从旧到新" },
  { value: "modified_desc", label: "修改时间从新到旧", section: true }, { value: "modified_asc", label: "修改时间从旧到新" },
];
const sortLabels: Record<string, string> = { name: "名称 ↑", name_desc: "名称 ↓", size_desc: "大小 ↓", size_asc: "大小 ↑", type: "类型 ↑", type_desc: "类型 ↓", created_desc: "创建 ↓", created_asc: "创建 ↑", modified_desc: "修改 ↓", modified_asc: "修改 ↑" };
const columns = [
  { label: "名称", asc: "name", desc: "name_desc", className: "browser-name-column" },
  { label: "类型", asc: "type", desc: "type_desc", className: "browser-type-column" },
  { label: "大小", asc: "size_asc", desc: "size_desc", className: "browser-size-column" },
  { label: "创建时间", asc: "created_asc", desc: "created_desc", className: "browser-created-column" },
  { label: "修改时间", asc: "modified_asc", desc: "modified_desc", className: "browser-modified-column" },
];
const dateFormat = new Intl.DateTimeFormat("zh-CN", { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false });
function fileTime(value: number) { return value ? dateFormat.format(new Date(value * 1000)) : "—"; }

export default function FileBrowser() {
  const { actor } = useGallery(), toast = useToast();
  const storageKey = `pimg:file-locations:${actor.owner_id}`;
  const [params, setParams] = useSearchParams();
  const [initialPath] = useState(() => { try { return sessionStorage.getItem(storageKey + ":last") || ""; } catch { return ""; } });
  const path = params.get("path") ?? initialPath, query = params.get("q") || "";
  const recursive = params.get("recursive") === "1";
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
  const visit = useCallback((next: string) => { setSearch(""); setParams({ path: next, ...(sort !== "name" ? { sort } : {}), ...(recursive ? { recursive: "1" } : {}) }); }, [setParams, sort, recursive]);
  const changeSort = (value: string) => setParams({ path: data?.path || path, ...(search.trim() ? { q: search.trim() } : {}), ...(value !== "name" ? { sort: value } : {}), ...(recursive ? { recursive: "1" } : {}) });
  useEffect(() => {
    const controller = new AbortController(), request = ++sequence.current;
    setLoading(true); setError(""); setData(null); setSelected(new Set()); setDetail(null); anchor.current = null; setMore(false);
    void api.get<BrowserListing>(`${browserBase}?${new URLSearchParams({ path, q: query, sort, ...(recursive ? { recursive: "1" } : {}) })}`, controller.signal).then(value => {
      if (request !== sequence.current || controller.signal.aborted) return;
      setData(value);
      try { sessionStorage.setItem(storageKey + ":last", value.path); } catch { /* Storage may be unavailable. */ }
    }).catch(cause => { if (!controller.signal.aborted && request === sequence.current) setError(cause.message); })
      .finally(() => { if (!controller.signal.aborted && request === sequence.current) setLoading(false); });
    return () => controller.abort();
  }, [path, query, sort, recursive, revision, storageKey]);
  useEffect(() => { setSearch(query); }, [query]);
  useEffect(() => {
    if (search === query) return;
    const timer = setTimeout(() => setParams({ ...(path ? { path } : {}), ...(search.trim() ? { q: search.trim() } : {}), ...(sort !== "name" ? { sort } : {}), ...(recursive ? { recursive: "1" } : {}) }, { replace: true }), 300);
    return () => clearTimeout(timer);
  }, [search, query, path, sort, recursive, setParams]);
  const loadMore = async () => {
    if (!data?.next_offset || more) return;
    const request = sequence.current;
    setMore(true);
    try {
      const result = await api.get<BrowserListing>(`${browserBase}?${new URLSearchParams({ path: data.path, q: query, sort, offset: String(data.next_offset), ...(recursive ? { recursive: "1", snapshot: data.snapshot || "" } : {}) })}`);
      if (request === sequence.current) { setData(previous => previous && { ...result, items: [...previous.items, ...result.items.filter(item => !previous.items.some(old => old.path === item.path))] }); return result; }
    } catch (cause: any) { if (request === sequence.current) toast(cause.message, "err"); }
    finally { if (request === sequence.current) setMore(false); }
  };
  const items = data?.items || [], files = items.filter(entry => !entry.directory);
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
  const drag = useDragSelection({ root, enabled: !loading && !detail && !importing && selectable.length > 0, scope: `${path}:${query}:${sort}:${recursive}:${revision}`, selected,
    onHit: paths => setSelected(previous => { const next = new Set(previous); paths.forEach(value => next.has(value) ? next.delete(value) : next.add(value)); return next; }), onRestore: setSelected });
  const active = media.findIndex(entry => entry.path === detail), current = media[active];
  const importPaths = (paths: string[], directory = false) => {
    if (!paths.length) return;
    if (paths.length > 200) { toast("一次最多选择 200 项；也可以使用“导入当前目录”。", "err"); return; }
    setDetail(null); setImporting({ paths, directory });
  };
  const folderNav = data?.breadcrumbs || [];
  const openEntry = (entry: BrowserEntry, range = false) => {
    if (entry.directory) visit(entry.path);
    else if (selected.size && entry.importable) toggle(entry, range);
    else if (entry.kind !== "other") setDetail(entry.path);
  };
  return <section className={`file-browser${selected.size ? " has-selection" : ""}`}>
    <h1 className="sr-only">文件</h1>
    <div className="file-browser-tools">
      <div className="file-browser-navigation">
        <button type="button" className="btn btn-ghost file-browser-icon file-browser-up" aria-label="上一级" title="上一级" disabled={!data || data.parent === data.path || loading} onClick={() => data && visit(data.parent)}><IconArrowUp size={17} /></button>
        <FileBookmarks locations={bookmarks.locations} currentPath={data?.path} onVisit={visit} onRemove={bookmarks.remove} />
        <nav ref={breadcrumbs} className="file-browser-breadcrumbs" aria-label="文件夹路径">{folderNav.map((item, index) => <span key={item.path}>{index > 0 && <IconChevronRight size={13} />}<button type="button" className="text-button" title={item.path} aria-current={index === folderNav.length - 1 ? "location" : undefined} onClick={() => visit(item.path)}>{item.name}</button></span>)}</nav>
        <button type="button" className="btn btn-ghost file-browser-icon file-browser-expand" aria-label={recursive ? "收起所有文件" : "展开所有文件"} title={recursive ? "收起所有文件，返回逐层浏览" : "展开当前目录及子目录中的所有图片和视频"} aria-pressed={recursive} disabled={loading || !data && !recursive} onClick={() => setParams({ path: data?.path || path, ...(search.trim() ? { q: search.trim() } : {}), ...(sort !== "name" ? { sort } : {}), ...(!recursive ? { recursive: "1" } : {}) })}><IconFolders size={17} /></button>
        <button type="button" className="btn btn-ghost file-browser-icon" aria-label={bookmarks.saved ? "取消收藏当前目录" : "收藏当前目录"} title={bookmarks.saved ? "取消收藏当前目录" : "收藏当前目录"} aria-pressed={bookmarks.saved} disabled={!data || loading} onClick={bookmarks.toggle}>{bookmarks.saved ? <IconStarFilled size={17} /> : <IconStar size={17} />}</button>
        <button type="button" className="btn btn-ghost file-browser-icon" aria-label="刷新目录" title="刷新目录" disabled={loading} onClick={() => setRevision(value => value + 1)}><IconRefresh size={17} /></button>
      </div>
      <div className="file-browser-actions">
        <SelectAllControl checked={allSelected} indeterminate={selected.size > 0 && !allSelected} disabled={!selectable.length || loading} scope="全选已加载的可导入文件" onChange={() => setSelected(allSelected ? new Set() : new Set(selectable.map(entry => entry.path)))} />
        <label className="file-browser-search"><IconSearch size={16} /><input className="input" aria-label={recursive ? "搜索所有图片和视频" : "搜索当前目录"} placeholder={recursive ? "搜索所有图片和视频" : "搜索当前目录"} value={search} onChange={event => setSearch(event.target.value)} />{search && <button type="button" className="btn btn-ghost" aria-label="清除文件搜索" onClick={() => setSearch("")}><IconX size={15} /></button>}</label>
        <FilterSelect label="文件排序" value={sort} displayLabel={sortLabels[sort]} options={sortOptions} active={sort !== "name"} width={208} onChange={changeSort} />
        <span className="file-browser-count" role="status">{data ? `${data.total} ${recursive ? "个文件 · 含子文件夹" : "项"}` : loading ? "读取中…" : ""}</span>
        <span className="file-browser-readonly">源文件只读</span>
        <button type="button" className="btn" disabled={!data || loading || !!query} title={query ? "清除搜索后可导入当前目录" : "选择相册后导入，可包含子目录"} onClick={() => data && importPaths([data.path], true)}>导入当前目录</button>
      </div>
    </div>
    <div className="file-browser-content" aria-busy={loading}>
      {error ? <div className="empty" role="alert"><p>{error}</p><p className="detail-hint">请检查目录的读取权限及 Docker 挂载，或返回默认目录继续浏览。</p><button type="button" className="btn" onClick={() => setRevision(value => value + 1)}>重新读取</button><button type="button" className="btn btn-ghost" onClick={() => visit("")}>返回默认目录</button></div> : loading ? <div className="empty" role="status">{recursive ? "正在整理当前目录及子文件夹中的图片和视频…" : "正在读取目录…"}</div> : !items.length ? <div className="empty"><IconFolder size={32} stroke={1.3} /><p>{recursive ? query ? "没有匹配的图片或视频" : "当前目录及子文件夹中没有图片或视频" : query ? "当前目录没有匹配的文件或文件夹" : "这个文件夹是空的"}</p>{query && <button type="button" className="btn" onClick={() => setSearch("")}>清除搜索</button>}</div> : <>
        {!!data?.skipped_directories && <p className="file-browser-warning" role="status">有 {data.skipped_directories} 个子文件夹无法读取，已展示其余可读取的文件。</p>}
        <div ref={root} className={`file-browser-list${drag.active ? " is-drag-selecting" : ""}`} {...drag.bind}>
          <table className="browser-table" aria-label={recursive ? "所有图片和视频" : "文件列表"}>
            <colgroup><col className="browser-check-column" />{columns.map(column => <col key={column.asc} className={column.className} />)}</colgroup>
            <thead><tr><th scope="col"><span className="sr-only">选择</span></th>{columns.map(column => <th scope="col" key={column.asc} className={column.className} aria-sort={sort === column.asc ? "ascending" : sort === column.desc ? "descending" : "none"}><button type="button" title={`按${column.label}排序`} onClick={() => changeSort(sort === column.asc ? column.desc : column.asc)}>{column.label}{sort === column.asc ? <IconArrowUp size={13} /> : sort === column.desc ? <IconArrowDown size={13} /> : null}</button></th>)}</tr></thead>
            <tbody>{items.map(entry => <tr key={entry.path + entry.version} data-file-id={entry.path} data-selection-disabled={!entry.importable ? "true" : undefined} className={`browser-row${selected.has(entry.path) ? " selected" : ""}`} onClick={event => { if (!(event.target as HTMLElement).closest("button,a,input")) openEntry(entry, event.shiftKey); }}>
              <td className="browser-select-cell">{entry.importable && <button type="button" className="browser-row-check" role="checkbox" aria-checked={selected.has(entry.path)} aria-label={`选择文件 ${entry.name}`} onClick={event => toggle(entry, event.shiftKey)}><span className="check" data-on={selected.has(entry.path)}><svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true"><path d="m3 8 3 3 5-6" /></svg></span></button>}</td>
              <td className="browser-name-cell"><button type="button" className="browser-row-open" data-selection-open disabled={!entry.directory && entry.kind === "other"} aria-label={`${entry.directory ? "打开文件夹" : "查看文件"} ${entry.name}`} title={entry.path} onClick={event => openEntry(entry, event.shiftKey)}>{entry.directory ? <IconFolder size={21} /> : entry.kind === "image" ? <IconPhoto size={21} /> : entry.kind === "video" ? <IconPlayerPlay size={21} /> : <IconFile size={21} />}<span className="browser-row-name"><span>{entry.name}</span>{recursive && entry.relative_path.includes("/") && <small title={entry.relative_path}>{entry.relative_path.slice(0, entry.relative_path.lastIndexOf("/"))}</small>}</span>{entry.directory && <IconChevronRight size={14} className="browser-folder-chevron" />}</button></td>
              <td className="browser-type-cell" title={entry.directory ? "文件夹" : entry.mime}>{entry.directory ? "文件夹" : entry.ext.toUpperCase() || "文件"}</td>
              <td className="browser-size-cell">{entry.directory ? "—" : fmtSize(entry.size)}</td>
              <td className="browser-created-cell" data-label="创建" title={entry.created_at ? new Date(entry.created_at * 1000).toLocaleString() : "文件系统未提供创建时间"}>{fileTime(entry.created_at)}</td>
              <td className="browser-modified-cell" data-label="修改" title={new Date(entry.modified_at * 1000).toLocaleString()}>{fileTime(entry.modified_at)}</td>
            </tr>)}</tbody>
          </table>
        </div>
        {!!data?.next_offset && <div className="file-browser-more"><button type="button" className="btn" disabled={more} onClick={() => void loadMore()}>{more ? "正在读取…" : `加载更多（已显示 ${items.length} / ${data.total}）`}</button></div>}
      </>}
    </div>
    {selected.size > 0 && <div className="batch-bar file-browser-batch" aria-label="文件导入操作"><span role="status">已选 {selected.size} 个文件</span><button type="button" className="btn btn-ghost" onClick={() => setSelected(new Set())}>取消选择</button><button type="button" className="btn btn-crayon" onClick={() => importPaths([...selected])}>导入到相册</button></div>}
    {current && <BrowserViewer key={current.path + current.version} entry={current} position={`${active + 1} / ${media.length}${data?.next_offset ? "+" : ""}`} selected={selected.has(current.path)} onSelect={() => toggle(current)} onPrev={active > 0 ? () => setDetail(media[active - 1].path) : undefined} onNext={active + 1 < media.length ? () => setDetail(media[active + 1].path) : data?.next_offset ? () => { void loadMore().then(result => { const next = result?.items.find(entry => entry.kind === "image" || entry.kind === "video"); if (next) setDetail(next.path); }); } : undefined} onClose={() => setDetail(null)} onImport={() => importPaths([current.path])} />}
    {importing && <LocalImport albums={[]} paths={importing.paths} directoryImport={importing.directory} onClose={() => setImporting(null)} />}
  </section>;
}
