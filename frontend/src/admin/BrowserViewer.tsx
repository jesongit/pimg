import { useMemo, useRef } from "react";
import { createPortal } from "react-dom";
import { IconChevronLeft, IconChevronRight, IconX } from "@tabler/icons-react";
import { fmtSize } from "../api";
import type { FileItem } from "../api";
import MediaViewer from "./MediaViewer";
import { useDialog } from "./interactions";
import { browserContent, prepareBrowserMedia } from "./browserMedia";
import type { BrowserEntry } from "./browserMedia";

export default function BrowserViewer({ entry, position, selected, onSelect, onPrev, onNext, onClose, onImport }: {
  entry: BrowserEntry; position: string; selected: boolean; onSelect: () => void;
  onPrev?: () => void; onNext?: () => void; onClose: () => void; onImport: () => void;
}) {
  const ref = useRef<HTMLElement>(null);
  useDialog(ref, onClose);
  const file = useMemo<FileItem>(() => ({
    id: entry.path + "@" + entry.version, name: entry.name, orig_name: entry.name,
    ext: entry.ext, mime: entry.mime, size: entry.size, kind: entry.kind, sha256: "",
    width: 0, height: 0, duration: 0, album_id: null, archive_state: "done", archive_error: "",
    has_thumb: false, thumb_w: 0, thumb_h: 0, has_bthumb: false,
    published: false, published_at: 0, show_home: false, cold_storage: "", hot_storage: "",
    created_at: entry.modified_at, updated_at: entry.modified_at, media_version: 2, local_ready: true,
  }), [entry]);
  const sourceAdapter = useMemo(() => ({
    autoLoad: true,
    prepare: (compatible: string, retry: boolean, signal: AbortSignal) => prepareBrowserMedia(entry, compatible, retry, signal),
    download: browserContent(entry) + "&download=1",
  }), [entry]);
  return createPortal(<><div className="overlay detail-backdrop" onClick={onClose} />
    <aside ref={ref} className="drawer file-drawer detail-workspace detail-docked browser-viewer" role="dialog" aria-modal="true" aria-label={`文件预览 ${entry.name}`}>
      <header className="detail-topbar">
        <div className="detail-heading"><h2 title={entry.name}>{entry.name}</h2><p>{entry.ext.toUpperCase()}<span>{fmtSize(entry.size)}</span></p></div>
        <div className="viewer-navigation"><span className="detail-position">{position}</span><button type="button" className="btn btn-ghost detail-icon" aria-label="上一个文件" disabled={!onPrev} onClick={onPrev}><IconChevronLeft size={18} /></button><button type="button" className="btn btn-ghost detail-icon" aria-label="下一个文件" disabled={!onNext} onClick={onNext}><IconChevronRight size={18} /></button><label className="viewer-select"><input type="checkbox" checked={selected} disabled={!entry.importable} onChange={onSelect} />选中</label></div>
        <button type="button" className="btn btn-ghost detail-icon detail-close" aria-label="关闭文件预览" onClick={onClose}><IconX size={18} /></button>
      </header>
      <div className="detail-body"><div className="detail-canvas" aria-label="源文件预览"><MediaViewer key={file.id} file={file} sourceAdapter={sourceAdapter} onPrev={onPrev} onNext={onNext}
        status={<span className="detail-status-tag browser-source-path" title={entry.path}>源文件只读 · {entry.path}</span>}
        actions={<button type="button" className="btn btn-crayon" disabled={!entry.importable} title={entry.importable ? "复制原件到内部相册" : "此格式未启用导入"} onClick={onImport}>导入到相册</button>} />
      </div></div>
    </aside></>, document.body);
}
