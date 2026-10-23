import { useEffect, useId, useState } from "react";
import { createPortal } from "react-dom";
import { IconFolderStar, IconX } from "@tabler/icons-react";
import { useToast } from "../App";
import { useLibraryPopover } from "./LibraryPopover";
import type { BrowserLocation } from "./browserMedia";
import { browserPathLabel } from "./browserMedia";

function readLocations(key: string): BrowserLocation[] {
  try {
    const value = JSON.parse(localStorage.getItem(key) || "[]"), paths = new Set<string>();
    return Array.isArray(value) ? value.filter((item): item is BrowserLocation => {
      if (typeof item?.path !== "string" || !item.path || typeof item?.name !== "string" || paths.has(item.path)) return false;
      paths.add(item.path); return true;
    }) : [];
  } catch { return []; }
}

export function useFileBookmarks(storageKey: string, current: BrowserLocation | null) {
  const [state, setState] = useState(() => ({ key: storageKey, locations: readLocations(storageKey) }));
  const locations = state.key === storageKey ? state.locations : [], toast = useToast();
  const saved = !!current && locations.some(item => item.path === current.path);
  useEffect(() => {
    const load = () => setState({ key: storageKey, locations: readLocations(storageKey) });
    load();
    const sync = (event: StorageEvent) => { if (event.storageArea === localStorage && (event.key === storageKey || event.key === null)) load(); };
    window.addEventListener("storage", sync); return () => window.removeEventListener("storage", sync);
  }, [storageKey]);
  const save = (next: BrowserLocation[]) => {
    try { localStorage.setItem(storageKey, JSON.stringify(next)); setState({ key: storageKey, locations: next }); }
    catch { toast("收藏未能保存，请检查浏览器存储权限后重试。", "err"); }
  };
  const remove = (path: string) => save(locations.filter(item => item.path !== path));
  return { locations, saved, remove, toggle: () => { if (current) saved ? remove(current.path) : save([...locations, current]); } };
}

export default function FileBookmarks({ locations, currentPath, onVisit, onRemove }: { locations: BrowserLocation[]; currentPath?: string; onVisit: (path: string) => void; onRemove: (path: string) => void }) {
  const menu = useLibraryPopover(240), id = useId();
  return <>
    <button ref={menu.trigger} type="button" className="btn btn-ghost file-browser-icon file-bookmarks-trigger" aria-label="收藏夹" title="收藏夹" aria-haspopup="menu" aria-expanded={menu.open} aria-controls={menu.open ? id : undefined} onClick={menu.toggle} onKeyDown={event => { if (["ArrowDown", "ArrowUp"].includes(event.key)) { event.preventDefault(); if (!menu.open) menu.toggle(); } }}><IconFolderStar size={17} /></button>
    {menu.open && createPortal(<div ref={menu.popup} id={id} role="menu" tabIndex={-1} aria-label="收藏夹" className="library-popover file-bookmarks-menu" style={menu.position} onKeyDown={event => {
      if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
      event.preventDefault(); const items = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>('button:not(:disabled)')), index = items.indexOf(document.activeElement as HTMLButtonElement);
      const next = event.key === "Home" ? 0 : event.key === "End" ? items.length - 1 : (index + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
      items[next]?.focus();
    }}>
      <div className="file-bookmarks-list" role="none">
        {locations.length ? locations.map(item => <div key={item.path} className="file-bookmark-row" role="none">
          <button type="button" role="menuitem" className="file-bookmark-open" title={browserPathLabel(item.path)} aria-current={item.path === currentPath ? "location" : undefined} onClick={() => { menu.close(true); onVisit(item.path); }}><span>{item.name || browserPathLabel(item.path)}</span></button>
          <button type="button" role="menuitem" className="file-bookmark-remove" aria-label={`取消收藏 ${item.name || item.path}`} title="取消收藏" onClick={() => { menu.popup.current?.focus({ preventScroll: true }); onRemove(item.path); }}><IconX size={15} /></button>
        </div>) : <p className="file-bookmarks-empty">暂无收藏</p>}
      </div>
    </div>, document.body)}
  </>;
}
