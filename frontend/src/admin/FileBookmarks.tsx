import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import type { DragEvent, ReactNode } from "react";
import { createPortal } from "react-dom";
import { IconChevronDown, IconChevronRight, IconDots, IconFolder, IconFolderPlus, IconStar, IconStarFilled, IconTrash, IconX } from "@tabler/icons-react";
import { api } from "../api";
import { useToast } from "../App";
import { useLibraryPopover } from "./LibraryPopover";
import type { BrowserLocation } from "./browserMedia";
import { browserPathLabel } from "./browserMedia";

export type BookmarkNode = {
  id: number;
  type: "folder" | "bookmark";
  name: string;
  path?: string;
  directory?: boolean;
  children?: BookmarkNode[];
};

export type BookmarkTarget = { path: string; name: string; directory: boolean };

const base = "/api/admin/file-browser/bookmarks";
const keyOf = (node: BookmarkNode) => `${node.type}-${node.id}`;
// Shared across the bar and its portaled popovers so an item can be dragged
// out of a folder/more list and dropped back onto the bar.
let activeDragId: number | null = null;
const setActiveDrag = (id: number | null) => { activeDragId = id; };

function walk(nodes: BookmarkNode[], visit: (node: BookmarkNode) => void) {
  for (const node of nodes) {
    visit(node);
    if (node.type === "folder" && node.children?.length) walk(node.children, visit);
  }
}

function findItemsByPath(nodes: BookmarkNode[], path: string): BookmarkNode[] {
  const out: BookmarkNode[] = [];
  walk(nodes, node => { if (node.type === "bookmark" && node.path === path) out.push(node); });
  return out;
}

function countBookmarks(nodes: BookmarkNode[]): number {
  let total = 0;
  walk(nodes, node => { if (node.type === "bookmark") total++; });
  return total;
}

function flattenFolders(nodes: BookmarkNode[], depth: number): { id: number; name: string; depth: number }[] {
  const out: { id: number; name: string; depth: number }[] = [];
  for (const node of nodes) {
    if (node.type !== "folder") continue;
    out.push({ id: node.id, name: node.name, depth });
    if (node.children?.length) out.push(...flattenFolders(node.children, depth + 1));
  }
  return out;
}

// Name of the folder containing the first bookmark for path; empty when the
// bookmark sits directly on the bar.
function folderLabelForPath(nodes: BookmarkNode[], path: string): string {
  for (const node of nodes) {
    if (node.type !== "folder") continue;
    const children = node.children || [];
    if (children.some(child => child.type === "bookmark" && child.path === path)) return node.name;
    const nested = folderLabelForPath(children, path);
    if (nested) return nested;
  }
  return "";
}

export function useFileBookmarks(owner: string) {
  const [tree, setTree] = useState<BookmarkNode[]>([]);
  const [loaded, setLoaded] = useState(false);
  const toast = useToast();

  const reload = useCallback(async () => {
    try {
      const result = await api.get<{ items: BookmarkNode[] }>(base);
      setTree(result.items || []);
    } catch {
      /* A transient read failure keeps the last known bar. */
    } finally {
      setLoaded(true);
    }
  }, []);

  useEffect(() => {
    let alive = true;
    (async () => {
      let items: BookmarkNode[] = [];
      try {
        const result = await api.get<{ items: BookmarkNode[] }>(base);
        items = result.items || [];
      } catch { /* ignore */ }
      if (!alive) return;
      // One-time import of the legacy browser-local list, so previously saved
      // entries (and any stale ones) appear in the bar and can be removed.
      const legacyKey = `pimg:file-locations:${owner}`;
      let legacy: BrowserLocation[] = [];
      try {
        const raw = JSON.parse(localStorage.getItem(legacyKey) || "[]");
        if (Array.isArray(raw)) {
          legacy = raw
            .filter((item: any) => item && typeof item.path === "string" && item.path)
            .map((item: any) => ({ path: item.path as string, name: typeof item.name === "string" ? item.name : (item.path as string) }));
        }
      } catch { /* ignore */ }
      if (legacy.length) {
        if (items.length === 0) {
          const seen = new Set<string>();
          for (const item of legacy) {
            if (seen.has(item.path)) continue;
            seen.add(item.path);
            try {
              await api.post(`${base}/items`, { folder: 0, path: item.path, name: item.name || browserPathLabel(item.path) });
            } catch { /* keep importing the rest */ }
          }
          try {
            const result = await api.get<{ items: BookmarkNode[] }>(base);
            items = result.items || [];
          } catch { /* ignore */ }
        }
        try { localStorage.removeItem(legacyKey); } catch { /* ignore */ }
      }
      if (alive) { setTree(items); setLoaded(true); }
    })();
    return () => { alive = false; };
  }, [owner]);

  // Another tab or device may change the bar; refresh when the tab regains focus.
  useEffect(() => {
    const refresh = () => { void reload(); };
    window.addEventListener("focus", refresh);
    return () => window.removeEventListener("focus", refresh);
  }, [reload]);

  const add = useCallback(async (target: BookmarkTarget, folder: number) => {
    try {
      await api.post(`${base}/items`, { folder, path: target.path, name: target.name });
      await reload();
    } catch (error: any) {
      toast(error?.message || "收藏失败", "err");
      throw error;
    }
  }, [reload, toast]);

  const removeItem = useCallback(async (id: number) => {
    try { await api.del(`${base}/items/${id}`); await reload(); }
    catch (error: any) { toast(error?.message || "取消收藏失败", "err"); }
  }, [reload, toast]);

  const createFolder = useCallback(async (parent: number, name: string): Promise<number | null> => {
    try {
      const result = await api.post<{ id: number }>(`${base}/folders`, { parent, name });
      await reload();
      return result.id;
    } catch (error: any) {
      toast(error?.message || "新建收藏文件夹失败", "err");
      return null;
    }
  }, [reload, toast]);

  const removeFolder = useCallback(async (id: number) => {
    try { await api.del(`${base}/folders/${id}`); await reload(); }
    catch (error: any) { toast(error?.message || "删除收藏文件夹失败", "err"); }
  }, [reload, toast]);

  // Remove every bookmark entry pointing at path without touching the filesystem.
  const removePath = useCallback(async (path: string) => {
    const items = findItemsByPath(tree, path).filter(node => node.type === "bookmark");
    for (const item of items) {
      try { await api.del(`${base}/items/${item.id}`); } catch { /* keep removing the rest */ }
    }
    await reload();
  }, [tree, reload]);

  const hasPath = useCallback((path: string) => findItemsByPath(tree, path).length > 0, [tree]);
  const folderOf = useCallback((path: string) => folderLabelForPath(tree, path), [tree]);
  const move = useCallback(async (id: number, parent: number, position: number) => {
    try { await api.patch(`${base}/move/${id}`, { parent, position }); await reload(); }
    catch (error: any) { toast(error?.message || "移动收藏失败", "err"); }
  }, [reload, toast]);

  return { tree, loaded, add, removeItem, createFolder, removeFolder, removePath, hasPath, folderOf, move, reload };
}

// Bookmark toggle at the end of each row. Unsaved: pick a destination.
// Saved: clicking the filled star cancels directly, with no popup.
export function BookmarkRowButton({ target, tree, saved, onAdd, onRemovePath }: {
  target: BookmarkTarget; tree: BookmarkNode[]; saved: boolean;
  onAdd: (target: BookmarkTarget, folder: number) => Promise<void>; onRemovePath: (path: string) => Promise<void>;
}) {
  const menu = useLibraryPopover(260, "right");
  const [busy, setBusy] = useState(false);
  const folders = flattenFolders(tree, 0);
  const addTo = async (folder: number) => {
    if (busy) return;
    setBusy(true);
    try { await onAdd(target, folder); menu.close(true); } finally { setBusy(false); }
  };
  if (saved) {
    return <button type="button" className="btn btn-ghost browser-mark-button" aria-label={`取消收藏 ${target.name}`} title="取消收藏" aria-pressed onClick={event => { event.stopPropagation(); void onRemovePath(target.path); }}><IconStarFilled size={16} /></button>;
  }
  return <>
    <button ref={menu.trigger} type="button" className="btn btn-ghost browser-mark-button" aria-label={`收藏 ${target.name}`} title="收藏" aria-pressed={false} aria-haspopup="menu" aria-expanded={menu.open} onClick={event => { event.stopPropagation(); menu.toggle(); }}><IconStar size={16} /></button>
    {menu.open && createPortal(<div ref={menu.popup} role="menu" className="file-bookmark-menu file-bookmark-dest-menu" style={menu.position} aria-label="收藏到">
      <button type="button" role="menuitem" className="file-bookmark-target" disabled={busy} onClick={() => void addTo(0)}><IconFolder size={15} />收藏栏</button>
      {folders.map(folder => <button key={folder.id} type="button" role="menuitem" className="file-bookmark-target" disabled={busy} style={{ paddingLeft: 10 + folder.depth * 14 }} onClick={() => void addTo(folder.id)}><IconFolder size={15} />{folder.name}</button>)}
    </div>, document.body)}
  </>;
}

function BookmarkChip({ node, currentPath, onOpen, onRemoveItem, hidden }: {
  node: BookmarkNode; currentPath?: string; onOpen: (node: BookmarkNode) => void; onRemoveItem: (id: number) => void; hidden?: boolean;
}) {
  const path = node.path || "";
  return <div className={`file-bookmarks-bar-item${hidden ? " is-overflow" : ""}`} data-key={keyOf(node)}>
    <button type="button" className="text-button file-bookmark-chip-open" title={browserPathLabel(path)} aria-current={path === currentPath ? "location" : undefined} onClick={() => onOpen(node)}>{node.name || browserPathLabel(path)}</button>
    <button type="button" className="file-bookmark-chip-remove" aria-label={`移除收藏 ${node.name}`} title="移除收藏（不删除原文件）" onClick={() => onRemoveItem(node.id)}><IconX size={13} /></button>
  </div>;
}

type BookmarkDragState = {
  dragId: number | null;
  overId: number | null;
  start: (id: number) => void;
  over: (id: number) => void;
  end: () => void;
  drop: (event: DragEvent<HTMLElement>, target: BookmarkNode, siblings: BookmarkNode[], parent: number) => void;
  moveInto: (event: DragEvent<HTMLElement>, parent: number, position: number) => void;
};
const BookmarkDragContext = createContext<BookmarkDragState | null>(null);

function BookmarkDragProvider({ onMove, children }: { onMove: (id: number, parent: number, position: number) => void; children: ReactNode }) {
  const [dragId, setDragId] = useState<number | null>(null);
  const [overId, setOverId] = useState<number | null>(null);
  const drop = (event: DragEvent<HTMLElement>, target: BookmarkNode, siblings: BookmarkNode[], parent: number) => {
    event.preventDefault();
    event.stopPropagation();
    const draggedId = dragId ?? activeDragId;
    setDragId(null);
    setOverId(null);
    setActiveDrag(null);
    if (draggedId == null || draggedId === target.id) return;
    const rect = event.currentTarget.getBoundingClientRect();
    const ratio = rect.width ? (event.clientX - rect.left) / rect.width : 0;
    const base = siblings.filter(node => node.id !== draggedId);
    let index = base.findIndex(node => node.id === target.id);
    if (index < 0) index = base.length;
    if (ratio >= 0.5) index += 1;
    onMove(draggedId, parent, index);
  };
  const moveInto = (event: DragEvent<HTMLElement>, parent: number, position: number) => {
    const draggedId = dragId ?? activeDragId;
    if (draggedId == null) return;
    event.preventDefault();
    event.stopPropagation();
    setDragId(null);
    setOverId(null);
    setActiveDrag(null);
    onMove(draggedId, parent, position);
  };
  return <BookmarkDragContext.Provider value={{ dragId, overId, start: id => { setDragId(id); setActiveDrag(id); }, over: setOverId, end: () => { setDragId(null); setOverId(null); setActiveDrag(null); }, drop, moveInto }}>{children}</BookmarkDragContext.Provider>;
}

const dragRowClass = (drag: BookmarkDragState | null, id: number) => `${drag?.dragId === id ? " dragging" : ""}${drag?.overId === id ? " drag-over" : ""}`;
function dragHandlers(drag: BookmarkDragState | null, node: BookmarkNode, siblings: BookmarkNode[], parent: number) {
  if (!drag) return {};
  return {
    draggable: true,
    onDragStart: (event: DragEvent<HTMLElement>) => { event.stopPropagation(); event.dataTransfer.effectAllowed = "move"; event.dataTransfer.setData("text/plain", String(node.id)); setActiveDrag(node.id); drag.start(node.id); },
    onDragOver: (event: DragEvent<HTMLElement>) => { event.preventDefault(); event.stopPropagation(); if (drag.overId !== node.id) drag.over(node.id); },
    onDrop: (event: DragEvent<HTMLElement>) => drag.drop(event, node, siblings, parent),
    onDragEnd: () => { setActiveDrag(null); drag.end(); },
  };
}

function BookmarkRows({ nodes, siblings, parent, currentPath, onOpen, onRemoveItem, onRemoveFolder, depth = 0 }: {
  nodes: BookmarkNode[]; siblings?: BookmarkNode[]; parent: number; currentPath?: string; onOpen: (node: BookmarkNode) => void; onRemoveItem: (id: number) => void; onRemoveFolder: (id: number) => void; depth?: number;
}) {
  const drag = useContext(BookmarkDragContext);
  const all = siblings ?? nodes;
  return <>{nodes.map(node => {
    if (node.type === "folder") return <BookmarkFolderRow key={keyOf(node)} folder={node} siblings={all} parent={parent} depth={depth} currentPath={currentPath} onOpen={onOpen} onRemoveItem={onRemoveItem} onRemoveFolder={onRemoveFolder} />;
    const path = node.path || "";
    return <div key={keyOf(node)} className={`file-bookmark-row${dragRowClass(drag, node.id)}`} role="none" {...dragHandlers(drag, node, all, parent)}>
      <button type="button" role="menuitem" className="file-bookmark-open" title={browserPathLabel(path)} aria-current={path === currentPath ? "location" : undefined} onClick={() => onOpen(node)}><span>{node.name || browserPathLabel(path)}</span></button>
      <button type="button" className="file-bookmark-remove" aria-label={`移除收藏 ${node.name}`} title="移除收藏（不删除原文件）" onClick={() => onRemoveItem(node.id)}><IconX size={15} /></button>
    </div>;
  })}</>;
}

function BookmarkFolderRow({ folder, siblings, parent, depth, currentPath, onOpen, onRemoveItem, onRemoveFolder }: {
  folder: BookmarkNode; siblings: BookmarkNode[]; parent: number; depth?: number; currentPath?: string; onOpen: (node: BookmarkNode) => void; onRemoveItem: (id: number) => void; onRemoveFolder: (id: number) => void;
}) {
  const drag = useContext(BookmarkDragContext);
  const menu = useLibraryPopover(240, "left", { placement: "left", autoFocus: false });
  const children = folder.children || [];
  const openChild = (node: BookmarkNode) => { menu.close(true); onOpen(node); };
  return <>
    <div className={`file-bookmark-row file-bookmark-folder-row${dragRowClass(drag, folder.id)}`} role="none" {...dragHandlers(drag, folder, siblings, parent)}>
      <button ref={menu.trigger} type="button" role="menuitem" className="file-bookmark-open file-bookmark-folder-open" aria-haspopup="menu" aria-expanded={menu.open} onClick={() => menu.toggle()}><IconFolder size={15} /><span>{folder.name}</span><IconChevronRight size={13} className="file-bookmark-submenu-arrow" /></button>
    </div>
    {menu.open && <div ref={menu.popup} role="menu" className="file-bookmark-menu file-bookmark-submenu" style={menu.position} aria-label={folder.name}
      onDragOver={event => { if (drag) event.preventDefault(); }}
      onDrop={event => { if (drag) drag.moveInto(event, folder.id, children.length); }}>
      <div className="file-bookmark-menu-head"><span className="file-bookmark-menu-title">{folder.name}</span><span className="file-bookmark-menu-count">{countBookmarks(children)} 项收藏</span></div>
      <div className="file-bookmark-menu-list">
        <BookmarkRows nodes={children} parent={folder.id} currentPath={currentPath} onOpen={openChild} onRemoveItem={onRemoveItem} onRemoveFolder={onRemoveFolder} />
        {children.length === 0 && <p className="file-bookmarks-empty">这个收藏文件夹是空的</p>}
      </div>
    </div>}
  </>;
}

function BookmarkFolderChip({ folder, currentPath, onOpen, onRemoveItem, onRemoveFolder, onMove, hidden }: {
  folder: BookmarkNode; currentPath?: string; onOpen: (node: BookmarkNode) => void; onRemoveItem: (id: number) => void; onRemoveFolder: (id: number) => void; onMove: (id: number, parent: number, position: number) => void; hidden?: boolean;
}) {
  const menu = useLibraryPopover(260, "left");
  const [confirm, setConfirm] = useState(false);
  const children = folder.children || [];
  const openAndClose = (node: BookmarkNode) => { menu.close(true); onOpen(node); };
  return <div className={`file-bookmarks-bar-item${hidden ? " is-overflow" : ""}`} data-key={keyOf(folder)}>
    <button ref={menu.trigger} type="button" className="text-button file-bookmark-folder-toggle" title={folder.name} aria-haspopup="menu" aria-expanded={menu.open} onClick={menu.toggle}><IconFolder size={15} /><span>{folder.name}</span><IconChevronDown size={13} /></button>
    {menu.open && createPortal(<div ref={menu.popup} role="menu" className="file-bookmark-menu" style={menu.position} aria-label={`收藏文件夹 ${folder.name}`}>
      <div className="file-bookmark-menu-head"><span className="file-bookmark-menu-title">{folder.name}</span><span className="file-bookmark-menu-count">{countBookmarks(children)} 项收藏</span></div>
      <div className="file-bookmark-menu-list">
        <BookmarkDragProvider onMove={onMove}>
          <BookmarkRows nodes={children} parent={folder.id} currentPath={currentPath} onOpen={openAndClose} onRemoveItem={onRemoveItem} onRemoveFolder={onRemoveFolder} />
        </BookmarkDragProvider>
        {children.length === 0 && <p className="file-bookmarks-empty">这个收藏文件夹是空的</p>}
      </div>
      {confirm ? <div className="file-bookmark-confirm" role="alertdialog">
        <span>删除「{folder.name}」及其中 {countBookmarks(children)} 项收藏？只删除收藏，不影响磁盘上的文件。</span>
        <div className="file-bookmark-confirm-actions">
          <button type="button" className="btn btn-ghost" onClick={() => setConfirm(false)}>取消</button>
          <button type="button" className="btn btn-danger" onClick={() => { setConfirm(false); menu.close(true); onRemoveFolder(folder.id); }}>删除</button>
        </div>
      </div> : <button type="button" role="menuitem" className="file-bookmark-menu-action danger" onClick={() => setConfirm(true)}><IconTrash size={15} />删除此文件夹</button>}
    </div>, document.body)}
  </div>;
}

function NewBookmarkFolderButton({ onCreateFolder }: { onCreateFolder: (parent: number, name: string) => Promise<number | null> }) {
  const menu = useLibraryPopover(240, "right");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const create = async () => {
    const value = name.trim();
    if (!value || busy) return;
    setBusy(true);
    try {
      const id = await onCreateFolder(0, value);
      if (id != null) { setName(""); menu.close(true); }
    } finally { setBusy(false); }
  };
  return <>
    <button ref={menu.trigger} type="button" className="btn btn-ghost file-browser-icon file-bookmarks-add" aria-label="新建收藏文件夹" title="新建收藏文件夹" aria-haspopup="menu" aria-expanded={menu.open} onClick={menu.toggle}><IconFolderPlus size={16} /></button>
    {menu.open && createPortal(<div ref={menu.popup} role="menu" className="file-bookmark-menu file-bookmark-new-menu" style={menu.position} aria-label="新建收藏文件夹">
      <p className="file-bookmark-menu-title">新建收藏文件夹</p>
      <div className="file-bookmark-new-folder">
        <input className="input" value={name} placeholder="文件夹名称" aria-label="收藏文件夹名称" disabled={busy} onChange={event => setName(event.target.value)} onKeyDown={event => { if (event.key === "Enter") { event.preventDefault(); void create(); } }} />
        <button type="button" className="btn" disabled={busy || !name.trim()} onClick={() => void create()}><IconFolderPlus size={15} />新建</button>
      </div>
    </div>, document.body)}
  </>;
}

export function MultiBookmarkButton({ targets, tree, onAdd }: {
  targets: BookmarkTarget[]; tree: BookmarkNode[];
  onAdd: (target: BookmarkTarget, folder: number) => Promise<void>;
}) {
  const menu = useLibraryPopover(280, "right");
  const [busy, setBusy] = useState(false);
  const folders = flattenFolders(tree, 0);
  const addAll = async (folder: number) => {
    if (busy || !targets.length) return;
    setBusy(true);
    try {
      for (const target of targets) { try { await onAdd(target, folder); } catch { /* keep adding the rest */ } }
      menu.close(true);
    } finally { setBusy(false); }
  };
  return <>
    <button ref={menu.trigger} type="button" className="btn btn-ghost file-bookmark-batch" aria-label={`收藏选中的 ${targets.length} 项`} title={targets.length ? "收藏选中的文件夹/文件" : "请先勾选要收藏的文件夹或文件"} aria-haspopup="menu" aria-expanded={menu.open} disabled={!targets.length || busy} onClick={menu.toggle}><IconStar size={16} />收藏选中</button>
    {menu.open && createPortal(<div ref={menu.popup} role="menu" className="file-bookmark-menu file-bookmark-dest-menu" style={menu.position} aria-label="收藏到">
      <button type="button" role="menuitem" className="file-bookmark-target" disabled={busy} onClick={() => void addAll(0)}><IconFolder size={15} />收藏栏</button>
      {folders.map(folder => <button key={folder.id} type="button" role="menuitem" className="file-bookmark-target" disabled={busy} style={{ paddingLeft: 10 + folder.depth * 14 }} onClick={() => void addAll(folder.id)}><IconFolder size={15} />{folder.name}</button>)}
    </div>, document.body)}
  </>;
}

export function FileBookmarksBar({ tree, currentPath, onOpen, onRemoveItem, onRemoveFolder, onCreateFolder, onMove }: {
  tree: BookmarkNode[]; currentPath?: string; onOpen: (node: BookmarkNode) => void; onRemoveItem: (id: number) => void; onRemoveFolder: (id: number) => void;
  onCreateFolder: (parent: number, name: string) => Promise<number | null>; onMove: (id: number, parent: number, position: number) => void;
}) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [shown, setShown] = useState(tree.length);
  const [dragId, setDragId] = useState<number | null>(null);
  const [overId, setOverId] = useState<number | null>(null);
  const moreMenu = useLibraryPopover(260, "right");
  const overflow = tree.slice(shown);

  useEffect(() => {
    const scroll = scrollRef.current;
    if (!scroll) return;
    const measure = () => {
      const items = Array.from(scroll.querySelectorAll<HTMLElement>(".file-bookmarks-bar-item"));
      const available = scroll.clientWidth;
      let total = 0, count = 0;
      for (const item of items) {
        const width = item.getBoundingClientRect().width;
        const gap = count ? 2 : 0;
        if (sumFits(total, width, gap, available)) { total += width + gap; count++; } else break;
      }
      setShown(tree.length ? Math.max(count, 1) : 0);
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(scroll);
    return () => observer.disconnect();
  }, [tree]);

  const drop = (event: DragEvent<HTMLDivElement>, target: BookmarkNode) => {
    event.preventDefault();
    event.stopPropagation();
    const draggedId = dragId ?? activeDragId;
    const dragged = draggedId == null ? null : tree.find(node => node.id === draggedId);
    setDragId(null);
    setOverId(null);
    setActiveDrag(null);
    if (!dragged || dragged.id === target.id) return;
    const rect = event.currentTarget.getBoundingClientRect();
    const ratio = rect.width ? (event.clientX - rect.left) / rect.width : 0;
    if (target.type === "folder" && ratio > 0.3 && ratio < 0.7) {
      onMove(dragged.id, target.id, (target.children || []).length);
      return;
    }
    const base = tree.filter(node => node.id !== dragged.id);
    let index = base.findIndex(node => node.id === target.id);
    if (index < 0) index = base.length;
    if (ratio >= 0.5) index += 1;
    onMove(dragged.id, 0, index);
  };

  const dropOnBar = (event: DragEvent<HTMLDivElement>) => {
    if (activeDragId == null) return;
    event.preventDefault();
    const id = activeDragId;
    setActiveDrag(null);
    setDragId(null);
    setOverId(null);
    const position = tree.filter(node => node.id !== id).length;
    onMove(id, 0, position);
  };

  return <div className="file-browser-bookmarks-bar" role="toolbar" aria-label="收藏栏" onDragOver={event => { if (activeDragId != null) event.preventDefault(); }} onDrop={dropOnBar}>
    <div ref={scrollRef} className="file-bookmarks-scroll">
      {tree.length ? tree.map((node, index) => {
        const hidden = index >= shown;
        return <div key={keyOf(node)} className={`file-bookmarks-drag${hidden ? " is-overflow" : ""}${dragId === node.id ? " dragging" : ""}${overId === node.id ? " drag-over" : ""}`}
          draggable
          onDragStart={event => { event.dataTransfer.effectAllowed = "move"; event.dataTransfer.setData("text/plain", String(node.id)); setDragId(node.id); setActiveDrag(node.id); }}
          onDragOver={event => { event.preventDefault(); event.dataTransfer.dropEffect = "move"; if (overId !== node.id) setOverId(node.id); }}
          onDragLeave={() => setOverId(previous => previous === node.id ? null : previous)}
          onDrop={event => drop(event, node)}
          onDragEnd={() => { setDragId(null); setOverId(null); setActiveDrag(null); }}>
          {node.type === "folder"
            ? <BookmarkFolderChip folder={node} currentPath={currentPath} onOpen={onOpen} onRemoveItem={onRemoveItem} onRemoveFolder={onRemoveFolder} onMove={onMove} />
            : <BookmarkChip node={node} currentPath={currentPath} onOpen={onOpen} onRemoveItem={onRemoveItem} />}
        </div>;
      }) : <span className="file-bookmarks-empty-hint">尚未收藏；点每行末尾的星标收藏后显示在这里</span>}
    </div>
    <button ref={moreMenu.trigger} type="button" className="btn btn-ghost file-browser-icon file-bookmarks-more" aria-label="更多收藏" title="更多收藏" aria-haspopup="menu" aria-expanded={moreMenu.open} disabled={!overflow.length} style={{ visibility: overflow.length ? "visible" : "hidden" }} onClick={moreMenu.toggle}><IconDots size={16} /></button>
    {moreMenu.open && overflow.length > 0 && createPortal(<div ref={moreMenu.popup} role="menu" className="file-bookmark-menu" style={moreMenu.position} aria-label="更多收藏">
      <div className="file-bookmark-menu-list">
        <BookmarkDragProvider onMove={onMove}>
          <BookmarkRows nodes={overflow} siblings={tree} parent={0} currentPath={currentPath} onOpen={node => { moreMenu.close(true); onOpen(node); }} onRemoveItem={onRemoveItem} onRemoveFolder={onRemoveFolder} />
        </BookmarkDragProvider>
      </div>
    </div>, document.body)}
    <NewBookmarkFolderButton onCreateFolder={onCreateFolder} />
  </div>;
}

function sumFits(total: number, width: number, gap: number, available: number) {
  return total + width + gap <= available + 0.5;
}
