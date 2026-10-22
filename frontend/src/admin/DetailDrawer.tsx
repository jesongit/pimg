import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { fileMediaTime, fileMediaTimeLabel, fmtDur, fmtSize, fmtTime, storageName } from "../api";
import type { Album, FileItem } from "../api";
import { useToast } from "../App";
import { fileStatus, operationBusy, permanentPublishAllowed, permanentPublishReason, useDialog, useUnsavedChanges } from "./interactions";
import StorageLabels, { archiveEncryption, useStorageClock } from "./StorageLabels";
import FileEncryption, {fileEncryptionSetting} from "./FileEncryption";
import AvailableLinks from "./AvailableLinks";
import ArchiveConversion from "./ArchiveConversion";
import MediaViewer from "./MediaViewer";
import ConfirmButton from "./ConfirmButton";
import { homeVisibility } from "./homeVisibility";
import "./detail-actions.css";

import { useGallery } from "./GalleryScope";
import {MoveAlbumSelect,moveAlbumAvailable,albumOwnerLabel,transferDescription} from "./MoveAlbumOptions";
import type {AlbumOwner} from "./MoveAlbumOptions";

type DetailPanel = "links" | "organize" | "info" | "archive" | "delete";
const panelNames: Record<DetailPanel, string> = { links: "链接与分享", organize: "整理图片", info: "文件信息", archive: "加密与归档", delete: "彻底删除原件" };

interface Props {
  id: string; albums: Album[]; selected: boolean; position: string; onToggle: (file:FileItem) => void;
  moveAlbums?: Album[]; albumOwners?: AlbumOwner[]; moveAlbumsLoading?: boolean; moveAlbumsError?: string; onRetryMoveAlbums?:()=>void;
  onClose: () => void; onChanged: () => void; onTransferred?: (id: string) => void; onConverted: (id: string, notice?: string, encrypted?: boolean) => void; conversionNotice?: string; onPrev?: () => void; onNext?: () => void;
}
export default function DetailDrawer(props: Props) {
  const { id, albums, selected, position, onToggle, onClose, onChanged, onPrev, onNext } = props;
  const toast = useToast();
  const {api, admin, readOnly,actor,owner} = useGallery();
  const moveAlbums=props.moveAlbums||albums,albumOwners=props.albumOwners||[];
  const storageNow = useStorageClock();
  const [file, setFile] = useState<(FileItem & { chunks?: Record<string, number> }) | null>(null);
  const [conversionReceipt, setConversionReceipt] = useState<Pick<FileItem, "id" | "name" | "conversion" | "operation"> | null>(null);
  const [loadError, setLoadError] = useState(""), [rename, setRename] = useState("");
  const [panel, setPanel] = useState<DetailPanel | null>(null);
  const panelRef = useRef<HTMLDivElement>(null), panelTrigger = useRef<HTMLButtonElement | null>(null), sequence = useRef(0);
  const [submitting, setSubmitting] = useState(false), [pending, setPending] = useState<{ id?: number; type: string } | null>(null);
  const [moveAlbum, setMoveAlbum] = useState<number | null>(null);
  const purging = useRef(false);
  const publicChanging = useRef(false);
  const drawer = useRef<HTMLElement>(null), callbacks = useRef(props), alive = useRef(true), loaded = useRef(false);
  callbacks.current = props;
  const navigatedTo = useRef("");
  const dirty = !!file && typeof file.name === "string" && rename !== file.name;
  const dirtyRef = useRef(dirty); dirtyRef.current = dirty;
  useEffect(() => { setMoveAlbum(null); }, [id, file?.owner_id, file?.album_id]);
  useUnsavedChanges(dirty);
  const leave = (action?: () => void) => { if (!dirty || confirm("名称尚未保存，确定放弃更改？")) action?.(); };
  const closePanel = () => { setPanel(null); requestAnimationFrame(() => panelTrigger.current?.focus({ preventScroll: true })); };
  useDialog(drawer, () => panel ? closePanel() : leave(onClose));
  useEffect(() => {
    if (!panel) return;
    const target = panelRef.current?.querySelector<HTMLElement>(panel === "organize" ? "input" : "button, input, select, a[href]");
    target?.focus({ preventScroll: true });
  }, [panel]);
  const load = useCallback(async () => {
    const request = ++sequence.current;
    try {
      const next = await api.get<FileItem & { chunks: Record<string, number> }>(`/files/${id}`);
      if (!alive.current || request !== sequence.current) return;
      // Retired files return a named conversion receipt without media metadata.
      // Keep any loaded metadata until navigation; a direct old-ID visit can
      // follow the receipt without attempting to render it as a full file.
      if (typeof next.ext !== "string") {
        setConversionReceipt(next);
        setFile(previous => previous ? { ...previous, ...next } : null);
      } else {
        setConversionReceipt(null); setFile(next);
        if (!loaded.current || !dirtyRef.current) setRename(next.name);
      }
      setLoadError("");
      loaded.current = true;
    } catch (error: any) { if (alive.current && request === sequence.current && error.name !== "AbortError") setLoadError(error.message || "暂时无法读取图片，请重试。"); }
  }, [id, api]);
  useEffect(() => { alive.current = true; void load(); return () => { alive.current = false; sequence.current++; }; }, [load]);
  useEffect(()=>{const updated=()=>void load();window.addEventListener("pimg:files-updated",updated);return()=>window.removeEventListener("pimg:files-updated",updated)},[load]);
  const deleting = !!file?.purge_state;
  const converting = pending?.type === "convert" || pending?.type === "transfer" || !!file?.conversion;
  const working = !!pending || !!file && operationBusy(file);
  useEffect(() => {
    if (!file || !(working || !!file.task_references?.length || ["pending", "running"].includes(file.archive_state) || ["pending", "running"].includes(file.purge_state || ""))) return;
    const timer = setInterval(() => { void load(); }, 1000); return () => clearInterval(timer);
  }, [file, working, load]);
  useEffect(() => {
    if (!file || !pending) return;
    const operation = file.operation;
    if (pending.type === "convert" || pending.type === "transfer") {
      if (["failed","canceled"].includes(file.conversion?.state || "") || operation?.state === "failed") setPending(null);
      return;
    }
    if (operation && operation.id === pending.id && ["done", "failed", "canceled"].includes(operation.state)) {
      setPending(null); callbacks.current.onChanged();
      toast(operation.state === "done" ? pending.type === "verify" ? "恢复校验完成" : pending.type === "publish" ? "已发布，可复制外链" : pending.type === "move" ? "文件已迁入目标相册" : "已撤销发布，归档原件保留" : operation.error || admin ? "操作未完成，请在任务页查看原因。" : "操作未完成，请稍后重试。", operation.state === "done" ? "ok" : "err");
    } else if (pending.type === "publish" && file.published || pending.type === "unpublish" && !file.published) { setPending(null); callbacks.current.onChanged(); }
  }, [file, pending, toast]);
  useEffect(() => {
    const completed = conversionReceipt || file;
    if (!completed?.conversion || completed.conversion.state !== "done" || completed.conversion.phase !== "done") return;
    const result = completed.operation?.result;
    let resultTarget = "";
    try { resultTarget = typeof result === "string" ? JSON.parse(result)?.new_file_id || "" : result?.new_file_id || ""; } catch { /* Prefer the explicit receipt. */ }
    const target = completed.conversion.target_file_id || resultTarget;
    if (!target || target === id || navigatedTo.current === target) return;
    navigatedTo.current = target;
    if(completed.conversion.kind==="transfer_owner"){
      toast(completed.conversion.cleanup_pending?"归属转移完成，新文件默认私有；旧副本清理中。":"归属转移完成，新文件默认私有。");
      callbacks.current.onTransferred?.(id);callbacks.current.onChanged();callbacks.current.onClose();return;
    }
    const notice = completed.conversion.cleanup_pending ? "新原件已就绪；转换时有旧副本待清理，旧编号已停止通过本站访问。可在任务页查看最新清理状态。" : undefined;
    toast(notice ? "转换完成，旧副本待清理；旧编号已停止访问。" : "转换完成，新原件尚未发布，请重新获取链接。");
    callbacks.current.onConverted(target, notice, completed.conversion.target_encrypted);
  }, [file, conversionReceipt, id, toast]);
  const act = async (type: string, body?: object) => {
    if (readOnly || submitting || working) return;
    if (type === "publish" && file && !permanentPublishAllowed(file)) { toast(permanentPublishReason(file), "err"); return; }
    setSubmitting(true);
    try {
      const response = type === "rename" || type === "move" ? await api.patch<{ task_id?: number }>(`/files/${id}`, body) : await api.post<{ task_id?: number }>(`/files/${id}/${type}`,body);
      if (!alive.current) { callbacks.current.onChanged(); return; }
      if (type === "move" || type === "transfer") setMoveAlbum(null);
      if (["publish", "unpublish", "verify","transfer"].includes(type) || type === "move" && response.task_id) { setPending({ id: response.task_id, type }); if (type === "move" || type === "transfer") toast(type==="transfer"?"归属转移任务已安排，校验完成后自动更新":admin ? "文件迁移任务已安排，可在任务页查看进度" : "文件迁移任务已安排，完成后自动更新"); }
      else { if (type === "rename") dirtyRef.current = false; toast(type === "rename" ? "名称已保存" : type === "move" ? response.task_id ? "文件迁移任务已安排，可在任务页查看进度" : "相册调整请求已提交" : "已重新提交归档"); }
      await load(); callbacks.current.onChanged();
    } catch (error: any) { toast(error.message, "err"); } finally { if (alive.current) setSubmitting(false); }
  };
  const purge = async () => {
    if (!file || readOnly || submitting || purging.current || converting || file.purge_state) return;
    purging.current = true; setSubmitting(true);
    try { await api.del(`/files/${id}?confirm=PURGE`); callbacks.current.onChanged(); callbacks.current.onClose(); toast("删除任务已提交"); }
    catch (error: any) { toast(error.message, "err"); }
    finally { purging.current = false; if (alive.current) setSubmitting(false); }
  };
  const changePublic = async () => {
    if(!admin||readOnly||!file||submitting||working||deleting||publicChanging.current)return;
    publicChanging.current=true;setSubmitting(true);
    try { await api.patch(`/files/${id}`,{show_home:!file.show_home});await load();callbacks.current.onChanged();toast(file.show_home?"已取消公开，首页缩略图访问已关闭":"已公开缩略图，原件访问权限保持不变"); }
    catch(error:any){toast(error.message,"err")}
    finally{publicChanging.current=false;if(alive.current)setSubmitting(false)}
  };
  const guestAlbumID = albums.find(album => album.is_home)?.id;
  const moveCandidate=moveAlbum===0?{id:0,name:"未分类",owner_id:file?.owner_id||owner,is_home:false,displayed:false}:moveAlbums.find(album=>album.id===moveAlbum);
  const moveTarget=moveAlbumAvailable(moveCandidate,albumOwners)?moveCandidate:undefined;
  useEffect(()=>{if(moveAlbum!==null&&!props.moveAlbumsLoading&&!props.moveAlbumsError&&!moveTarget)setMoveAlbum(null)},[moveAlbum,props.moveAlbumsLoading,props.moveAlbumsError,moveTarget]);
  const crossMove=!!moveTarget?.owner_id&&moveTarget.owner_id!==(file?.owner_id||owner);
  const transferring=file?.conversion?.kind==="transfer_owner"||pending?.type==="transfer";
  const moveDescription=crossMove?transferDescription(albumOwnerLabel(moveTarget!.owner_id!,albumOwners,actor),moveTarget!.name):`将「${file?.name||"当前文件"}」移入「${moveTarget?.name||"目标相册"}」，用户归属保持不变。${moveTarget?.displayed?"目标相册已公开，未单独隐藏的文件移入后会公开缩略图。":""}`;
  const visibility = file ? homeVisibility(file, guestAlbumID) : null;
  const togglePanel = (next: DetailPanel, trigger: HTMLButtonElement) => { panelTrigger.current = trigger; if (panel === next) closePanel(); else setPanel(next); };
  const icon = (path: string) => <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={path} /></svg>;
  useEffect(() => {
    const updated = (event: Event) => { if ((event as CustomEvent).detail?.id === id) void load(); };
    window.addEventListener("pimg:media-stored", updated);
    return () => window.removeEventListener("pimg:media-stored", updated);
  }, [id]);
  const encryption = file ? archiveEncryption(file) : null;
  const encryptionSetting = file ? fileEncryptionSetting(file) : null;
  const fileOwner=file?.owner_id||owner;
  const fileAlbum=[...albums,...moveAlbums].find(album=>(album.owner_id||owner)===fileOwner&&(file?.album_id?album.id===file.album_id:album.is_unclassified));
  const operationLabel = pending ? ({ publish: "正在永久发布…", unpublish: "正在撤销发布…", verify: "正在校验原件…", move: "正在迁移相册…", convert: "正在转换归档…", transfer: "正在转移归属…" } as Record<string, string>)[pending.type] : file ? fileStatus(file) : "";
  const detailError = file?.operation?.state === "failed" && !file.conversion ? file.operation.error : file?.publish_error || file?.archive_error;
  const panelButton = (name: DetailPanel, label = panelNames[name]) => <button type="button" className={`btn${name === "delete" ? " detail-delete-button" : ""}`} aria-expanded={panel === name} aria-controls="detail-function-panel" onClick={event => togglePanel(name, event.currentTarget)}>{label}</button>;
  return <><div className="overlay detail-backdrop" onClick={() => leave(onClose)} /><aside ref={drawer} className="drawer file-drawer detail-workspace detail-docked" role="dialog" aria-modal="true" aria-label={file ? `图片详情 ${file.name}` : "图片详情"}>
    <header className="detail-topbar">
      <div className="detail-heading"><h2 title={file?.name}>{file?.name || "正在读取图片…"}</h2>{file && <p>{file.ext.toUpperCase()}<span>{fmtSize(file.size)}</span>{!!file.width && !!file.height && <span>{file.width} × {file.height}</span>}{file.kind === "video" && <span>{fmtDur(file.duration)}</span>}</p>}</div>
      <div className="viewer-navigation"><span className="detail-position" aria-label="当前浏览位置">{position}</span>{!readOnly && <label className="viewer-select"><input type="checkbox" checked={selected} disabled={!file || deleting} onChange={()=>{if(file)onToggle(file)}} />选中</label>}</div>
      <button className="btn btn-ghost detail-icon detail-close" aria-label="关闭详情" title="关闭（Esc）" onClick={() => leave(onClose)}>{icon("M6 6l12 12M18 6L6 18")}</button>
    </header>
    {loadError && <div className="detail-load-error" role="alert"><p>{loadError}</p><button className="btn" onClick={() => void load()}>重新加载</button></div>}
    {!file && !loadError && <div className="detail-loading" role="status">正在读取图片详情…</div>}
    {file && <div className="detail-body">
      <div className="detail-canvas" aria-label="图片预览"><MediaViewer key={id} file={file} blocked={deleting ? "正在删除，预览已停止" : converting ? transferring ? "正在转移归属，可在下方查看进度" : "正在转换归档，可在下方查看进度" : undefined} onPrev={onPrev?()=>leave(onPrev):undefined} onNext={onNext?()=>leave(onNext):undefined} interactionActive={!panel}
        status={<><span className="detail-status-tag">用户：{albumOwnerLabel(fileOwner,albumOwners,actor)}</span><span className="detail-status-tag">所属相册：{fileAlbum?.name||(file.album_id?`相册 ${file.album_id}`:"未分类")}</span><span className={`detail-status-tag file-encryption-setting is-${encryptionSetting?.state}`}><FileEncryption file={file} />加密设置：{encryptionSetting?.label}</span><StorageLabels file={file} now={storageNow} />{admin && <span className={`detail-status-tag${visibility?.state === "visible" ? " is-visible" : ""}`} title={visibility?.description}>{visibility?.title}</span>}{(pending || !["已发布", "已发布 · 首页", "未发布"].includes(fileStatus(file))) && <span className={`detail-status-tag${file.archive_state === "failed" || file.purge_state === "failed" ? " is-failed" : " is-working"}`} role="status">{operationLabel}</span>}</>}
        notice={<>{detailError && <p className="detail-dock-notice is-error" role="alert">{detailError}</p>}{props.conversionNotice && <p className="detail-dock-notice" role="status">{props.conversionNotice} {admin && <Link to="/admin/tasks">查看清理任务</Link>}</p>}</>}
        actions={<>
          {admin&&!readOnly&&!converting&&!deleting&&<ConfirmButton className="btn detail-public-button" disabled={submitting||working||file.archive_state!=="done"} resetKey={`${id}:${file.show_home}:${file.home_visibility}`} confirmLabel={file.show_home?"确认取消公开":"确认公开"} description={file.show_home?`取消「${file.name}」的公开，即使相册公开，此文件也会单独隐藏。原件的永久发布链接独立管理。`:`将「${file.name}」的缩略图公开到首页。游客不能通过此功能读取原件或播放原视频。`} onConfirm={changePublic}>{file.show_home?"取消公开":"公开"}</ConfirmButton>}
          {!deleting && !readOnly && <fieldset disabled={submitting || working || converting} className="detail-publish-actions">{file.archive_state === "done" ? file.published ? <button className="btn" onClick={() => void act("unpublish")}>{file.operation?.type === "unpublish" && file.operation.state === "failed" ? "重试撤销发布" : "撤销发布"}</button> : permanentPublishAllowed(file) ? <button className="btn" onClick={() => void act("publish")}>{file.album_id === guestAlbumID ? "发布到首页" : "永久发布"}</button> : null : file.archive_state === "failed" ? <button className="btn" onClick={() => void act("retry")}>重试归档</button> : null}</fieldset>}
          {!deleting && !converting && <>{panelButton("links", "链接与分享")}{!readOnly && panelButton("organize", dirty ? "整理 · 未保存" : "整理")}</>}
          {panelButton("info", "文件信息")}{!deleting && !readOnly && (file.archive_state === "done" || !!file.conversion) && panelButton("archive", converting ? transferring ? "转移进度" : "转换进度" : "加密与归档")}
          {admin && (working || file.operation?.type === "tg_move_file" && file.operation.state === "failed") && <Link className="btn" to={file.operation?.state === "failed" ? `/admin/logs?file_id=${encodeURIComponent(id)}` : "/admin/tasks"}>查看任务</Link>}

          {file.show_home && <a className="btn" href="/" target="_blank" rel="noreferrer">查看首页</a>}
          {!readOnly && !converting && (deleting ? panelButton("delete", "删除进度") : <ConfirmButton className="btn detail-delete-button" disabled={submitting} confirmLabel="确认删除" busyLabel="提交中…" resetKey={id} description={`永久删除「${file.name}」的全部归档、展示副本与索引，无法恢复；他人保存的副本无法收回。${file.task_references?.length ? "会自动取消等待中的归档、发布和同步；正在执行的步骤停止后再清理。" : ""}`} onConfirm={purge}>删除</ConfirmButton>)}
        </>}
      />
      </div>
      {panel && <div ref={panelRef} id="detail-function-panel" className={`detail-function-panel detail-panel-${panel}`} role="region" aria-label={panelNames[panel]}>
        <div className="detail-panel-head"><h3>{panelNames[panel]}</h3><button type="button" className="btn btn-ghost detail-icon" aria-label="收起功能区" onClick={closePanel}>{icon("M6 6l12 12M18 6L6 18")}</button></div>
        <div className="detail-panel-content">
          {panel === "links" && !deleting && !converting && <AvailableLinks file={file} onChanged={() => { void load(); callbacks.current.onChanged(); }} />}
          {panel === "organize" && !deleting && !converting && <section className="detail-edit"><form onSubmit={event => { event.preventDefault(); void act("rename", { name: rename.trim() }); }}><label htmlFor="file-name">显示名称</label><div className="name-editor"><input id="file-name" className="input" value={rename} onChange={event => setRename(event.target.value)} maxLength={255} aria-describedby="file-name-hint" /><button className="btn" disabled={submitting || working || !dirty || !rename.trim()}>保存</button></div><p id="file-name-hint" className="detail-hint">只修改显示名称，原文件名和归档内容保持不变。</p></form><label htmlFor="file-album">所属相册</label><span className="choice-confirm-slot"><MoveAlbumSelect id="file-album" className="select" albums={moveAlbums} grouped={admin} users={albumOwners} actor={actor} owner={file.owner_id||owner} value={moveAlbum ?? (file.album_id || "")} disabled={submitting || working || props.moveAlbumsLoading} onChange={event => {
            const albumID = Number(event.target.value);
            setMoveAlbum(albumID);
          }}><option value="">未分类</option></MoveAlbumSelect><ConfirmButton aria-label={crossMove?"转移归属":"移入相册"} disabled={submitting || working || moveAlbum === null || !moveTarget || props.moveAlbumsLoading || !!props.moveAlbumsError} confirmLabel={crossMove?"确认转移归属":"确认移入"} busyLabel="提交中…" resetKey={JSON.stringify([id,file.owner_id,file.album_id,file.published,moveTarget?.id,moveTarget?.owner_id,moveTarget?.name,moveTarget?.displayed])} armKey={moveAlbum ?? undefined} description={moveDescription} onConfirm={()=>moveAlbum===null||!moveTarget?undefined:act(crossMove?"transfer":"move", {album_id:moveAlbum,...(crossMove?{confirm:"TRANSFER_OWNER"}:{})})}>{crossMove?"转移归属":"移入相册"}</ConfirmButton></span>{props.moveAlbumsLoading&&<p role="status">正在更新全部相册…</p>}{props.moveAlbumsError&&<p role="alert">{props.moveAlbumsError} <button className="btn" onClick={props.onRetryMoveAlbums}>重试读取相册</button></p>}</section>}
          {panel === "info" && <dl className="meta-grid"><dt>原文件名</dt><dd>{file.orig_name}</dd><dt>文件大小</dt><dd>{fmtSize(file.size)}</dd>{!!file.width && !!file.height && <><dt>尺寸</dt><dd>{file.width} × {file.height}</dd></>}{file.kind === "video" && <><dt>时长</dt><dd>{fmtDur(file.duration)}</dd></>}<dt>类型</dt><dd>{file.mime}</dd><dt>编号</dt><dd>{file.id}</dd><dt>SHA256</dt><dd>{file.sha256}</dd><dt>归档存储</dt><dd>{file.media_version === 2 ? <StorageLabels file={file} now={storageNow} /> : <>{storageName(file.cold_storage)} · {encryption?.label}</>}</dd><dt>发布存储</dt><dd>{file.published ? storageName(file.hot_storage) : "未发布"}</dd><dt>分片</dt><dd>{file.media_version === 2 ? `${file.media_parts?.length || 1} 段（按存储分别保存）` : file.chunks ? `原件 ${file.chunks.data || 0} · 封面 ${file.chunks.thumb || 0}` : "—"}</dd><dt>{fileMediaTimeLabel(file)}</dt><dd>{fmtTime(fileMediaTime(file))}</dd><dt>导入时间</dt><dd>{fmtTime(file.created_at)}</dd>{file.published_at > 0 && <><dt>发布时间</dt><dd>{fmtTime(file.published_at)}</dd></>}</dl>}
          {panel === "archive" && <section className="detail-archive-tools">{converting && <p className="detail-hint">{transferring?"正在复制并校验目标归档，成功后转移归属。新文件默认私有，旧编号停止通过本站访问，旧公开副本随后清理；失败时原文件与恢复进度保留。":"转换期间旧链接停止通过本站访问。完成后，请从新原件重新获取链接。"}</p>}{transferring&&file.conversion?.error&&<p role="alert">{file.conversion.error}</p>}{transferring&&admin&&["failed","canceled"].includes(file.conversion?.state||"")&&<button className="btn" disabled={submitting||working} onClick={()=>void act("transfer",{album_id:file.conversion?.target_album_id,confirm:"TRANSFER_OWNER"})}>重试归属转移</button>}{file.archive_state === "done" && !deleting && !converting && <div className="actions"><button className="btn" disabled={submitting || working} onClick={() => void act("verify")}>校验恢复</button></div>}{file.operation?.type === "verify" && file.operation.state === "done" && <p className="detail-hint" role="status">恢复校验完成。</p>}{!deleting && !transferring && (file.archive_state === "done" || !!file.conversion) && <ArchiveConversion file={file} disabled={submitting || working || dirty} dirty={dirty} onAccepted={async taskID => { if (alive.current) { setPending({ id: taskID, type: "convert" }); await load(); } callbacks.current.onChanged(); }} />}{!deleting && !converting && !permanentPublishAllowed(file) && <p className="detail-hint">{permanentPublishReason(file)}</p>}</section>}
          {panel === "delete" && !converting && <section className="detail-danger"><p className="detail-hint">删除全部归档、展示副本与索引，无法恢复；他人保存的副本无法收回。</p>{!file.purge_state && !!file.task_references?.length && <p className="detail-hint">删除会自动取消等待中的归档、发布和同步；正在执行的步骤停止后再清理。</p>}{file.purge_state ? <><p role="status">{file.purge_state === "failed" ? "删除未完成，剩余清理进度已保留。" : "删除任务已提交，等待空闲清理。"}</p>{file.purge_error && <p role="alert" className="detail-error">{file.purge_error}</p>}{admin && file.purge_state === "failed" && <Link className="btn btn-danger" to={`/admin/logs?file_id=${encodeURIComponent(id)}`}>查看并重试删除任务</Link>}</> : <ConfirmButton className="btn btn-danger" disabled={submitting || readOnly} confirmLabel="确认删除" busyLabel="提交中…" resetKey={id} description={`将永久删除「${file.name}」及全部归档、展示副本与索引，无法恢复。`} onConfirm={purge}>彻底删除</ConfirmButton>}</section>}
        </div>
      </div>}
    </div>}
  </aside></>;
}
