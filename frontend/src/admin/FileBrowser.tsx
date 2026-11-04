import FloatingFeedback from "../FloatingFeedback";
import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useSearchParams } from "react-router-dom";
import { IconArrowDown, IconArrowUp, IconChevronRight, IconFile, IconFolder, IconFolders, IconPhoto, IconPlayerPlay, IconRefresh, IconSearch, IconDownload, IconFolderPlus, IconLink, IconX } from "@tabler/icons-react";
import { api, fmtSize } from "../api";
import type { BrowserIndexStatus } from "../api";
import { useGallery } from "./GalleryScope";
import { useToast } from "../App";
import SelectAllControl from "./SelectAllControl";
import LocalImport from "./LocalImport";
import BrowserViewer from "./BrowserViewer";
import MediaFullscreenSession from "../media/MediaFullscreenSession";
import useDragSelection from "./useDragSelection";
import { browserBase, browserPathLabel } from "./browserMedia";
import type { BrowserEntry, BrowserListing } from "./browserMedia";
import "./file-browser.css";

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
function isRootPath(value: string) { return /^(?:\/+|[a-z]:[\\/]*|[\\/]{2}[^\\/]+[\\/][^\\/]+[\\/]*)$/i.test(value); }

export default function FileBrowser() {
  const { actor } = useGallery(), toast = useToast();
  const storageKey = `pimg:file-locations:${actor.owner_id}`;
  const [params, setParams] = useSearchParams();
  const [initialPath] = useState(() => { try { return sessionStorage.getItem(storageKey + ":last") || "/data"; } catch { return "/data"; } });
  const path = params.get("path") ?? initialPath, query = params.get("q") || "";
  const previousPath = useRef(path), pathChanged = previousPath.current !== path;
  const recursive = params.get("recursive") === "1" && !pathChanged && !isRootPath(path);
  useEffect(() => {
    previousPath.current = path;
    if ((pathChanged || isRootPath(path)) && params.has("recursive")) {
      const next = new URLSearchParams(params); next.delete("recursive"); setParams(next, { replace: true });
    }
  }, [path, pathChanged, params, setParams]);
  const requestedSort = params.get("sort") || "name", sort = columns.some(column => requestedSort === column.asc || requestedSort === column.desc) ? requestedSort : "name";
  const [search, setSearch] = useState(query);
  const [data, setData] = useState<BrowserListing | null>(null), [loading, setLoading] = useState(true), [error, setError] = useState("");
  const [revision, setRevision] = useState(0), [more, setMore] = useState(false);
  const [indexChanged, setIndexChanged] = useState(false);
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
  const changeSort = (value: string) => setParams({ path: data?.path || path, ...(search.trim() ? { q: search.trim() } : {}), ...(value !== "name" ? { sort: value } : {}), ...(recursive ? { recursive: "1" } : {}) });
  const toggleExpanded = () => {
    const currentPath = data?.path || path;
    previousPath.current = currentPath;
    setParams({ path: currentPath, ...(search.trim() ? { q: search.trim() } : {}), ...(sort !== "name" ? { sort } : {}), ...(!recursive ? { recursive: "1" } : {}) });
  };
  useEffect(() => {
    const controller = new AbortController(), request = ++sequence.current;
    setLoading(true); setError(""); setData(null); setSelected(new Set()); setDetail(null); anchor.current = null; setMore(false); setIndexChanged(false);
    void api.get<BrowserListing>(`${browserBase}?${new URLSearchParams({ path, q: query, sort, ...(recursive ? { recursive: "1" } : {}) })}`, controller.signal).then(value => {
      if (request !== sequence.current || controller.signal.aborted) return;
      setData(value);
      try { sessionStorage.setItem(storageKey + ":last", value.path); } catch { /* Storage may be unavailable. */ }
    }).catch(cause => { if (!controller.signal.aborted && request === sequence.current) setError(cause.message); })
      .finally(() => { if (!controller.signal.aborted && request === sequence.current) setLoading(false); });
    return () => controller.abort();
  }, [path, query, sort, recursive, revision, storageKey]);
  useEffect(() => {
    if (!data?.indexed || loading) return;
    const controller = new AbortController(), request = sequence.current;
    let busy = false;
    const timer = setInterval(async () => {
      if (busy || document.hidden) return;
      busy = true;
      try {
        const status = await api.get<BrowserIndexStatus>(`${browserBase}/index`, controller.signal);
        const normalize = (value: string) => /^[a-z]:|^\\\\/i.test(value) ? value.replaceAll("\\", "/").toLowerCase() : value;
        const current = normalize(data.index_root || data.path), root = status.roots.find(value => current === normalize(value.path) || current.startsWith(normalize(value.path) + "/"));
        if (!root) { setIndexChanged(true); return; }
        const changed = !data.snapshot?.endsWith(`:${root.revision}`);
        const stateChanged = data.index_building !== !root.ready || data.index_updating !== root.updating || (data.index_error || "") !== (root.error || status.error || "");
        if (!changed && !stateChanged) return;
        if (selected.size || detail || importing || more || data.items.length > 200) {
          if (changed) setIndexChanged(true);
          if (stateChanged) setData(previous => previous && { ...previous, index_building: !root.ready, index_updating: root.updating, index_error: root.error || status.error });
          return;
        }
        const value = await api.get<BrowserListing>(`${browserBase}?${new URLSearchParams({ path: data.path, q: query, sort, ...(recursive ? { recursive: "1" } : {}) })}`, controller.signal);
        if (!controller.signal.aborted && sequence.current === request) { setData(value); setIndexChanged(false); }
      } catch { /* A transient status failure must not hide a usable listing. */ }
      finally { busy = false; }
    }, 3000);
    return () => { controller.abort(); clearInterval(timer); };
  }, [data, loading, selected.size, detail, importing, more, query, sort, recursive]);
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
      const result = await api.get<BrowserListing>(`${browserBase}?${new URLSearchParams({ path: data.path, q: query, sort, offset: String(data.next_offset), ...(data.snapshot ? { snapshot: data.snapshot } : {}), ...(recursive ? { recursive: "1" } : {}) })}`);
      if (request === sequence.current) { setData(previous => previous && { ...result, items: [...previous.items, ...result.items.filter(item => !previous.items.some(old => old.path === item.path))] }); return result; }
    } catch (cause: any) { if (request === sequence.current) toast(cause.message, "err"); }
    finally { if (request === sequence.current) setMore(false); }
  };
  const items = data?.items || [], files = items.filter(entry => !entry.directory);
  const allItemsSelected = items.length > 0 && items.every(entry => selected.has(entry.path));
  const refresh = async () => {
    if (data?.indexed) {
      try { await api.post(`${browserBase}/index/refresh`, {}); }
      catch (cause: any) { toast(cause.message, "err"); }
    }
    setRevision(value => value + 1);
  };
  const selectable = files.filter(entry => entry.importable), media = files.filter(entry => entry.kind === "image" || entry.kind === "video");
  const allSelected = selectable.length > 0 && selectable.every(entry => selected.has(entry.path));
  const toggle = (entry: BrowserEntry, range = false) => {
    if (entry.directory && entry.kind === "other" && archiveFileExt(entry.ext)) return;
    const scope = items;
    const start = range && anchor.current ? scope.findIndex(value => value.path === anchor.current) : -1;
    const end = scope.findIndex(value => value.path === entry.path);
    const targets = start >= 0 ? scope.slice(Math.min(start, end), Math.max(start, end) + 1).map(value => value.path) : [entry.path];
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
  const isArchive = (entry: BrowserEntry) => !entry.directory && /^(zip|rar|7z|iso)$/i.test(entry.ext);
  const archiveFileExt = (ext: string) => /^(zip|rar|7z|iso)$/i.test(ext);
  const [toolsOpen, setToolsOpen] = useState(false);
  const [toolsPos, setToolsPos] = useState({ top: 0, left: 0 });
  const [mkdirOpen, setMkdirOpen] = useState(false), [mkdirName, setMkdirName] = useState(""), [mkdirBusy, setMkdirBusy] = useState(false);
  const [linkOpen, setLinkOpen] = useState(false), [linkTarget, setLinkTarget] = useState(""), [linkBusy, setLinkBusy] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!toolsOpen) return;
    const close = (event: MouseEvent) => { if (!menuRef.current?.contains(event.target as Node)) setToolsOpen(false); };
    document.addEventListener("pointerdown", close);
    return () => document.removeEventListener("pointerdown", close);
  }, [toolsOpen]);
  const doMkdir = async () => {
    const name = mkdirName.trim();
    if (!name || !data) return;
    setMkdirBusy(true);
    try { await api.post(`${browserBase}/mkdir`, { parent: data.path, name }); toast(`已创建文件夹 ${name}`, "ok"); setMkdirOpen(false); setMkdirName(""); void refresh(); }
    catch (err) { toast(err instanceof Error ? err.message : "创建文件夹失败", "err"); }
    finally { setMkdirBusy(false); }
  };
  const doHardlink = async () => {
    if (!selected.size || !linkTarget.trim()) return;
    setLinkBusy(true);
    try { await api.post(`${browserBase}/hardlink`, { paths: [...selected], target: linkTarget.trim() }); toast("已创建硬链接", "ok"); setLinkOpen(false); }
    catch (err) { toast(err instanceof Error ? err.message : "硬链接失败", "err"); }
    finally { setLinkBusy(false); }
  };
  const hardlinkFiles = [...selected].filter(p => items.some(e => e.path === p && !e.directory)).length;
  const hardlinkDirs = [...selected].filter(p => items.some(e => e.path === p && e.directory)).length;
  const [extracting, setExtracting] = useState<Record<string, number>>({});
  const extractArchive = async (entry: BrowserEntry) => {
    if (!isArchive(entry) || extracting[entry.path] !== undefined) return;
    setExtracting(prev => ({ ...prev, [entry.path]: 0 }));
    try {
      const job = await api.post<{ done: boolean; ok: boolean; error?: string; target?: string; percent?: number }>(`${browserBase}/extract`, { path: entry.path });
      if (job.ok) { toast(`已解压到 ${job.target}`, "ok"); setExtracting(prev => { const n = { ...prev }; delete n[entry.path]; return n; }); return; }
      if (job.done) { toast(job.error || "解压失败", "err"); setExtracting(prev => { const n = { ...prev }; delete n[entry.path]; return n; }); return; }
      toast("开始后台解压…", "ok");
      // poll progress until done
      const timer = window.setInterval(async () => {
        try {
          const st = await api.get<{ done: boolean; ok: boolean; error?: string; percent?: number; target?: string }>(`${browserBase}/extract?${new URLSearchParams({ path: entry.path })}`);
          if (st.done) {
            window.clearInterval(timer);
            if (st.ok) toast(`已解压到 ${st.target}`, "ok"); else toast(st.error || "解压失败", "err");
            setExtracting(prev => { const n = { ...prev }; delete n[entry.path]; return n; });
          } else {
            setExtracting(prev => ({ ...prev, [entry.path]: st.percent ?? -1 }));
          }
        } catch { /* keep polling */ }
      }, 1500);
    } catch (err) {
      toast(err instanceof Error ? err.message : "解压失败", "err");
      setExtracting(prev => { const n = { ...prev }; delete n[entry.path]; return n; });
    }
  };
  return <section className={`file-browser${selected.size ? " has-selection" : ""}`}>
    <h1 className="sr-only">文件</h1>
    <div className="file-browser-tools">
      <div className="file-browser-navigation">
        <button type="button" className="btn btn-ghost file-browser-icon file-browser-up" aria-label="上一级" title="上一级" disabled={!data || data.parent === data.path || loading} onClick={() => data && visit(data.parent)}><IconArrowUp size={17} /></button>
        <nav ref={breadcrumbs} className="file-browser-breadcrumbs" aria-label="文件夹路径">{folderNav.map((item, index) => <span key={item.path}>{index > 0 && <IconChevronRight size={13} />}<button type="button" className="text-button" title={item.path} aria-current={index === folderNav.length - 1 ? "location" : undefined} onClick={() => visit(item.path)}>{item.name}</button></span>)}</nav>
        <button type="button" className="btn btn-ghost file-browser-icon file-browser-expand" aria-label={recursive ? "收起所有文件" : "展开所有文件"} title={data && data.parent === data.path || isRootPath(path) ? "根目录不支持展开，请先进入子文件夹" : recursive ? "收起所有文件，返回逐层浏览" : "展开当前目录及子目录中的所有图片和视频"} aria-pressed={recursive} disabled={loading || !data && !recursive || !!data && data.parent === data.path || isRootPath(path)} onClick={toggleExpanded}><IconFolders size={17} /></button>
        <button type="button" className="btn btn-ghost file-browser-icon" aria-label="刷新目录" title="刷新目录" disabled={loading} onClick={() => void refresh()}><IconRefresh size={17} /></button>
      </div>
      <div className="file-browser-actions">
        <SelectAllControl checked={allSelected} indeterminate={selected.size > 0 && !allSelected} disabled={!selectable.length || loading} scope="全选已加载的可导入文件" onChange={() => setSelected(allSelected ? new Set() : new Set(selectable.map(entry => entry.path)))} />
        <label className="file-browser-search"><IconSearch size={16} /><input className="input" aria-label={recursive ? "搜索所有图片和视频" : "搜索当前目录"} placeholder={recursive ? "搜索所有图片和视频" : "搜索当前目录"} value={search} onChange={event => setSearch(event.target.value)} />{search && <button type="button" className="btn btn-ghost" aria-label="清除文件搜索" onClick={() => setSearch("")}><IconX size={15} /></button>}</label>
        <span className="file-browser-count" role="status">{data ? `${data.scan_limited ? "已扫描到 " : ""}${data.total} ${recursive ? "个文件 · 含子文件夹" : "项"}` : loading ? "读取中…" : ""}</span>
        <span className="file-browser-readonly">源文件只读</span>
        <button type="button" className="btn" disabled={!data || loading || !!query} title={query ? "清除搜索后可导入当前目录" : "选择相册后导入，可包含子目录"} onClick={() => data && importPaths([data.path], true)}>导入当前目录</button>
        <div ref={menuRef} className="browser-tools-menu-wrap">
          <button type="button" className="btn btn-ghost browser-tools-menu-trigger" aria-haspopup="menu" aria-expanded={toolsOpen} onClick={event => { setToolsOpen(v => !v); const r = (event.currentTarget as HTMLElement).getBoundingClientRect(); setToolsPos({ top: r.bottom + 6, left: r.right - 180 }); }}>操作<i aria-hidden="true" style={{fontStyle:"normal",marginLeft:4}}>▾</i></button>
          {toolsOpen && createPortal(<div className="browser-tools-menu" role="menu" style={{ position: "fixed", top: toolsPos.top, left: toolsPos.left, zIndex: 999999, minWidth: 160, width: "auto", background: "#ffffff", color: "#1f2a33", border: "1px solid #cbd5df", borderRadius: 8, padding: 4, boxShadow: "0 8px 30px rgba(0,0,0,.3)" }}>
            <button type="button" role="menuitem" style={{ display: "flex", alignItems: "center", gap: 8, width: "100%", whiteSpace: "nowrap", padding: "9px 12px", border: 0, borderRadius: 6, background: "transparent", color: "inherit", font: "inherit", fontSize: 13, cursor: "pointer" }} onClick={() => { setToolsOpen(false); setMkdirOpen(true); }}><IconFolderPlus size={16} />新建文件夹</button>
            <button type="button" role="menuitem" disabled={!selected.size} title={selected.size ? "" : "请先勾选要硬链接的文件或文件夹"} style={{ display: "flex", alignItems: "center", gap: 8, width: "100%", whiteSpace: "nowrap", padding: "9px 12px", border: 0, borderRadius: 6, background: "transparent", color: "inherit", font: "inherit", fontSize: 13, cursor: "pointer", opacity: selected.size ? 1 : .45 }} onClick={() => { setToolsOpen(false); setLinkTarget(data?.path || ""); setLinkOpen(true); }}><IconLink size={16} />硬链接选中项</button>
          </div>, document.body)}
        </div>
      </div>
    </div>
    <div className="file-browser-content" aria-busy={loading}>
      {data?.indexed && <FloatingFeedback className="file-browser-warning" role="status">{data.index_building ? "正在建立索引，当前列表可能尚不完整，完成后自动更新。" : data.index_updating ? "正在更新索引，先显示已记录的文件。" : "已使用文件索引"}{data.index_error && ` ${data.index_error}，请检查目录并刷新。`}{indexChanged && <> 文件已更新。<button type="button" className="text-button" onClick={() => setRevision(value => value + 1)}>载入最新列表</button></>}</FloatingFeedback>}
      {!!data?.scan_limited && <FloatingFeedback className="file-browser-warning" role="status">目录过大，本次仅展示已扫描范围内的 {data.total} 个文件。进入更小的子文件夹后展开，可查看完整内容。</FloatingFeedback>}
      {error ? <FloatingFeedback className="empty" role="alert"><p>{error}</p><p className="detail-hint">请检查目录的读取权限及 Docker 挂载，或返回默认目录继续浏览。</p><button type="button" className="btn" onClick={() => setRevision(value => value + 1)}>重新读取</button><button type="button" className="btn btn-ghost" onClick={() => visit("")}>返回默认目录</button></FloatingFeedback> : loading ? <div className="empty" role="status">{recursive ? "正在整理当前目录及子文件夹中的图片和视频…" : "正在读取目录…"}</div> : !items.length ? <div className="empty"><IconFolder size={32} stroke={1.3} /><p>{data?.index_building ? "索引正在建立，文件会陆续显示…" : data?.scan_limited ? "本次扫描未找到图片或视频，请进入子文件夹后浏览" : recursive ? query ? "没有匹配的图片或视频" : "当前目录及子文件夹中没有图片或视频" : query ? "当前目录没有匹配的文件或文件夹" : "这个文件夹是空的"}</p>{query && <button type="button" className="btn" onClick={() => setSearch("")}>清除搜索</button>}</div> : <>
        {!!data?.skipped_directories && <FloatingFeedback className="file-browser-warning" role="status">有 {data.skipped_directories} 个子文件夹无法读取，已展示其余可读取的文件。</FloatingFeedback>}
        <div ref={root} className={`file-browser-list${drag.active ? " is-drag-selecting" : ""}`} {...drag.bind}>
          <table className="browser-table" aria-label={recursive ? "所有图片和视频" : "文件列表"}>
            <colgroup><col className="browser-check-column" />{columns.map(column => <col key={column.asc} className={column.className} />)}</colgroup>
            <thead><tr><th scope="col" className="browser-check-column">{items.length > 0 && <button type="button" role="checkbox" aria-checked={allItemsSelected ? "true" : selected.size > 0 ? "mixed" : "false"} aria-label="全选当前列表（含文件夹）" className="browser-row-check browser-select-all" onClick={() => setSelected(allItemsSelected ? new Set() : new Set(items.map(item => item.path)))}><span className="check" data-on={allItemsSelected} data-indeterminate={!allItemsSelected && selected.size > 0}><svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true"><path d="m3 8 3 3 5-6" /></svg></span></button>}</th>{columns.map(column => <th scope="col" key={column.asc} className={column.className} aria-sort={sort === column.asc ? "ascending" : sort === column.desc ? "descending" : "none"}><button type="button" title={`按${column.label}排序`} onClick={() => changeSort(sort === column.asc ? column.desc : column.asc)}>{column.label}{sort === column.asc ? <IconArrowUp size={13} /> : sort === column.desc ? <IconArrowDown size={13} /> : null}</button></th>)}</tr></thead>
            <tbody>{items.map(entry => <tr key={entry.path + entry.version} data-file-id={entry.path} data-selection-disabled={!entry.importable ? "true" : undefined} className={`browser-row${selected.has(entry.path) ? " selected" : ""}`} onClick={event => { if (!(event.target as HTMLElement).closest("button,a,input")) openEntry(entry, event.shiftKey); }}>
              <td className="browser-select-cell"><button type="button" className="browser-row-check" role="checkbox" aria-checked={selected.has(entry.path)} aria-label={`选择 ${entry.directory ? "文件夹" : "文件"} ${entry.name}`} onClick={event => toggle(entry, event.shiftKey)}><span className="check" data-on={selected.has(entry.path)}><svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true"><path d="m3 8 3 3 5-6" /></svg></span></button></td>
              <td className="browser-name-cell"><div className="browser-name-row-wrap"><button type="button" className="browser-row-open" data-selection-open disabled={!entry.directory && entry.kind === "other" && !isArchive(entry)} aria-label={`${entry.directory ? "打开文件夹" : "查看文件"} ${entry.name}`} title={browserPathLabel(entry.path)} onClick={event => openEntry(entry, event.shiftKey)}>{entry.directory ? <IconFolder size={21} /> : entry.kind === "image" ? <IconPhoto size={21} /> : entry.kind === "video" && !isArchive(entry) ? <IconPlayerPlay size={21} /> : <IconFile size={21} />}<span className="browser-row-name"><span>{entry.name}</span>{recursive && entry.relative_path.includes("/") && <small title={entry.relative_path}>{entry.relative_path.slice(0, entry.relative_path.lastIndexOf("/"))}</small>}</span>{entry.directory && <IconChevronRight size={14} className="browser-folder-chevron" />}</button>{isArchive(entry) ? <button type="button" className="browser-extract-btn" aria-label={`解压 ${entry.name}`} title={extracting[entry.path] !== undefined ? `解压 ${entry.name} 中 ${extracting[entry.path] >= 0 ? extracting[entry.path] + "%" : "…"}` : "解压到当前目录旁"} disabled={extracting[entry.path] !== undefined} onClick={event => { event.stopPropagation(); void extractArchive(entry); }}>{extracting[entry.path] !== undefined ? <span className="browser-extract-label">{extracting[entry.path] >= 0 ? `${extracting[entry.path]}%` : "解压中"}</span> : <IconDownload size={16} />}</button> : null}</div></td>
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
    {current && <MediaFullscreenSession><BrowserViewer key={current.path + current.version} entry={current} position={`${active + 1} / ${media.length}${data?.next_offset ? "+" : ""}`} selected={selected.has(current.path)} onSelect={() => toggle(current)} onPrev={active > 0 ? () => setDetail(media[active - 1].path) : undefined} onNext={active + 1 < media.length ? () => setDetail(media[active + 1].path) : data?.next_offset ? () => { void loadMore().then(result => { const next = result?.items.find(entry => entry.kind === "image" || entry.kind === "video"); if (next) setDetail(next.path); }); } : undefined} onClose={() => setDetail(null)} onImport={() => importPaths([current.path])} /></MediaFullscreenSession>}
    {importing && <LocalImport albums={[]} paths={importing.paths} directoryImport={importing.directory} onClose={() => setImporting(null)} />}
    {mkdirOpen ? <><div className="overlay" onClick={() => !mkdirBusy && setMkdirOpen(false)} /><div className="modal" role="dialog" aria-modal="true" aria-labelledby="mkdir-title" onClick={event => event.stopPropagation()}>
      <div className="dhead"><h2 id="mkdir-title">新建文件夹</h2><button type="button" className="btn btn-ghost" disabled={mkdirBusy} onClick={() => setMkdirOpen(false)}>关闭</button></div>
      <form onSubmit={event => { event.preventDefault(); void doMkdir(); }}>
        <fieldset disabled={mkdirBusy}><label htmlFor="mkdir-name">文件夹名称</label><input id="mkdir-name" className="input" value={mkdirName} onChange={event => setMkdirName(event.target.value)} placeholder="名称（不允许 / 或 ..）" autoFocus /></fieldset>
        <div className="modal-actions"><button type="button" className="btn btn-ghost" disabled={mkdirBusy} onClick={() => setMkdirOpen(false)}>取消</button><button type="submit" className="btn" disabled={mkdirBusy || !mkdirName.trim()}>{mkdirBusy ? "创建中…" : "创建"}</button></div>
      </form>
    </div></> : null}
    {linkOpen ? <><div className="overlay" onClick={() => !linkBusy && setLinkOpen(false)} /><div className="modal" role="dialog" aria-modal="true" aria-labelledby="link-title" onClick={event => event.stopPropagation()}>
      <div className="dhead"><h2 id="link-title">硬链接</h2><button type="button" className="btn btn-ghost" disabled={linkBusy} onClick={() => setLinkOpen(false)}>关闭</button></div>
      <form onSubmit={event => { event.preventDefault(); void doHardlink(); }}>
        <fieldset disabled={linkBusy}><p>将选中的 <strong>{hardlinkFiles}</strong> 个文件、<strong>{hardlinkDirs}</strong> 个文件夹硬链接到目标目录</p><label htmlFor="link-target">目标目录（绝对路径）</label><input id="link-target" className="input" value={linkTarget} onChange={event => setLinkTarget(event.target.value)} placeholder="/data/..." autoFocus /></fieldset>
        <div className="modal-actions"><button type="button" className="btn btn-ghost" disabled={linkBusy} onClick={() => setLinkOpen(false)}>取消</button><button type="submit" className="btn" disabled={linkBusy || !linkTarget.trim()}>{linkBusy ? "链接中…" : "硬链接"}</button></div>
      </form>
    </div></> : null}
  </section>;
}
