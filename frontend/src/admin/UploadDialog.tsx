import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { IconFile, IconUpload } from "@tabler/icons-react";
import { api, fmtSize } from "../api";
import type { Album } from "../api";
import { useDialog } from "./interactions";
import { useUploads } from "./Uploads";
import { useGallery } from "./GalleryScope";
import "./upload-dialog.css";

interface UploadDialogProps {
  albums: Album[];
  initialFiles?: File[];
  onClose: () => void;
  onUpload: (files: File[], encrypted: boolean, albumID: number) => void;
}

const fileKeys = new WeakMap<File, number>();
let nextFileKey = 0;
function fileKey(file: File) {
  let key = fileKeys.get(file);
  if (key === undefined) { key = ++nextFileKey; fileKeys.set(file, key); }
  return key;
}

function appendFiles(previous: File[], added: File[]) {
  const known = new Set(previous.map(fileKey));
  return [...previous, ...added.filter(file => {
    const key = fileKey(file);
    if (known.has(key)) return false;
    known.add(key);
    return true;
  })];
}

export default function UploadDialog({ albums, initialFiles, onClose, onUpload }: UploadDialogProps) {
  const ref = useRef<HTMLDivElement>(null), input = useRef<HTMLInputElement>(null);
  const titleID = useId(), formatsID = useId(), albumID = useId(), encryptionID = useId();
  const { policy } = useUploads();
  const { admin, actor, owner: galleryOwner } = useGallery();
  const ownerID = useId();
  const [owner, setOwner] = useState(galleryOwner), [owners, setOwners] = useState<{ id: string; username: string }[]>([]);
  const [targets, setTargets] = useState(albums), [targetsReady, setTargetsReady] = useState(!admin), [targetsError, setTargetsError] = useState("");
  const alive = useRef(true);
  const loadTargets = useCallback(async () => {
    if (!admin) return;
    setTargetsReady(false); setTargetsError("");
    try {
      const [catalog, accounts] = await Promise.all([api.get<{ items: Album[] }>("/api/admin/albums?owner_id=all"), api.get<{ items: { id: string; username: string; disabled: boolean; deleting: boolean }[] }>("/api/admin/accounts")]);
      if (alive.current) { const active = accounts.items.filter(value => !value.disabled && !value.deleting); setOwners(active); setTargets(catalog.items.filter(value => active.some(account => account.id === value.owner_id))); setTargetsReady(true); }
    } catch (cause: any) { if (alive.current) setTargetsError(cause.message || "目标相册读取失败，请重试。"); }
  }, [admin]);
  useEffect(() => { alive.current = true; void loadTargets(); return () => { alive.current = false; }; }, [loadTargets]);
  const [files, setFiles] = useState<File[]>(() => appendFiles([], initialFiles || []));
  const [album, setAlbum] = useState<number | null>(null), [encrypted, setEncrypted] = useState(false);
  useDialog(ref, onClose);
  useEffect(() => {
    if (initialFiles?.length) setFiles(previous => appendFiles(previous, initialFiles));
  }, [initialFiles]);

  const allowed = useMemo(() => (policy.allowed_exts || "").split(",").map(ext => ext.trim().replace(/^\./, "").toLowerCase()).filter(Boolean), [policy.allowed_exts]);
  const maxMB = policy.upload_max_mb;
  const fileError = (file: File) => {
    if (maxMB && file.size > maxMB * 1024 * 1024) return `超过单文件上限 ${maxMB} MB，请移除此文件。`;
    const ext = file.name.split(".").pop()?.toLowerCase() || "";
    if (allowed.length && !allowed.includes(ext)) return "此格式不在服务器支持范围内，请移除此文件。";
    return "";
  };
  const invalid = files.some(file => !!fileError(file));
  const hasAlbum = targetsReady && album !== null && (album === 0 && owner === actor.owner_id || targets.some(value => value.id === album && (!admin || value.owner_id === owner)));
  const addFiles = (added: File[]) => setFiles(previous => appendFiles(previous, added));
  const start = () => {
    if (!files.length || invalid || album === null || !hasAlbum) return;
    onUpload(files, encrypted, album);
    onClose();
  };

  return createPortal(<>
    <div className="overlay" onClick={onClose} />
    <div ref={ref} className="modal upload-dialog" role="dialog" aria-modal="true" aria-labelledby={titleID}
      onDragOver={event => { if (event.dataTransfer.types.includes("Files")) { event.preventDefault(); event.stopPropagation(); event.dataTransfer.dropEffect = "copy"; } }}
      onDrop={event => { const added = Array.from(event.dataTransfer.files); if (added.length) { event.preventDefault(); event.stopPropagation(); addFiles(added); } }}
      onPaste={event => { const added = Array.from(event.clipboardData.files); if (added.length) { event.preventDefault(); event.stopPropagation(); addFiles(added); } }}>
      <div className="dhead"><h2 id={titleID}>上传</h2><button type="button" className="btn btn-ghost" onClick={onClose}>关闭</button></div>
      <div className="upload-dialog-body">
        <div className="upload-dialog-picker">
          <button type="button" className="btn" aria-describedby={formatsID} onClick={() => input.current?.click()}><IconUpload size={17} aria-hidden="true" />{files.length ? "添加文件" : "选择文件"}</button>
          <span className="detail-hint">也可拖入或粘贴文件</span>
          <input ref={input} type="file" multiple hidden accept={allowed.length ? allowed.map(ext => `.${ext}`).join(",") : undefined} aria-label="选择上传文件" onChange={event => { addFiles(Array.from(event.target.files || [])); event.target.value = ""; }} />
        </div>
        <p id={formatsID} className="detail-hint upload-dialog-policy">{allowed.length ? `支持格式：${allowed.map(ext => ext.toUpperCase()).join("、")}` : "支持格式以服务器配置为准。"}{maxMB ? ` · 单文件上限 ${maxMB} MB` : ""}</p>
        <div className="upload-dialog-selection">
          <div className="upload-dialog-selection-head"><span role="status">{files.length ? `已选 ${files.length} 个文件 · ${fmtSize(files.reduce((sum, file) => sum + file.size, 0))}` : "尚未选择文件"}</span>{!!files.length && <button type="button" className="btn btn-sm btn-ghost" onClick={() => setFiles([])}>清空选择</button>}</div>
          {files.length ? <ul className="upload-dialog-files" aria-label="待上传文件">{files.map(file => {
            const key = fileKey(file), error = fileError(file);
            return <li key={key} className="upload-dialog-file"><IconFile size={17} aria-hidden="true" /><div><strong>{file.name}</strong><small>{fmtSize(file.size)}</small>{error && <p className="detail-error">{error}</p>}</div><button type="button" className="btn btn-sm btn-ghost" aria-label={`移除 ${file.name}`} onClick={() => setFiles(previous => previous.filter(value => fileKey(value) !== key))}>移除</button></li>;
          })}</ul> : <p className="detail-hint upload-dialog-empty">选择文件后，可在这里检查并移除；点击“开始上传”才会提交。</p>}
        </div>
        {admin && <div className="upload-dialog-target"><label htmlFor={ownerID}>所属用户</label><select id={ownerID} className="select" disabled={!targetsReady} value={owner} onChange={event => { setOwner(event.target.value); setAlbum(null); }}><option value={actor.owner_id}>自己（{actor.username}）</option>{owners.filter(value => value.id !== actor.owner_id).map(value => <option key={value.id} value={value.id}>{value.username}</option>)}</select></div>}
        {targetsError && <p className="detail-error" role="alert">{targetsError}<button type="button" className="btn btn-ghost" onClick={() => void loadTargets()}>重新读取相册</button></p>}
        <div className="upload-dialog-target"><label htmlFor={albumID}>目标相册</label><select id={albumID} className="select" required disabled={!targetsReady} value={album !== null && hasAlbum ? album : ""} onChange={event => setAlbum(event.target.value === "" ? null : Number(event.target.value))}><option value="" disabled>{targetsReady ? "请选择相册（必选）" : "正在读取相册…"}</option>{owner === actor.owner_id && <option value={0}>未分类</option>}{targets.filter(value => (!admin || value.owner_id === owner) && !(owner === actor.owner_id && value.is_unclassified)).map(value => <option key={value.id} value={value.id}>{value.name}</option>)}</select></div>
        <div className="upload-dialog-options"><label htmlFor={encryptionID}><input id={encryptionID} type="checkbox" checked={encrypted} onChange={event => setEncrypted(event.target.checked)} />加密归档</label><p className="detail-hint">{admin ? "文件归所选用户所有，上传后保持私有。" : "上传后仅自己可见。"}加密仅应用于允许加密的归档存储。</p></div>
      </div>
      <div className="upload-dialog-footer"><button type="button" className="btn btn-ghost" onClick={onClose}>取消</button><button type="button" className="btn btn-crayon" disabled={!files.length || invalid || !hasAlbum} onClick={start}>开始上传</button></div>
    </div>
  </>, document.body);
}
