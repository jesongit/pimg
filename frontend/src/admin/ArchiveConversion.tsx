import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useGallery } from "./GalleryScope";
import type { FileItem } from "../api";
import { archiveEncrypted } from "./interactions";
import ConfirmButton from "./ConfirmButton";
import {fileEncryptionSetting} from "./FileEncryption";

interface Props { file: FileItem; disabled: boolean; dirty: boolean; onAccepted: (taskID: number) => Promise<void>; }

export default function ArchiveConversion({ file, disabled, dirty, onAccepted }: Props) {
  const {api, admin} = useGallery();
  const [submitting, setSubmitting] = useState(false), [error, setError] = useState("");
  const alive = useRef(true), busy = useRef(false);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const modern=file.media_version===2;
  const archives=(file.storage_locations||[]).filter(location=>location.role==="archive"&&location.confirmed);
  const setting=fileEncryptionSetting(file);
  const [choice,setChoice]=useState(setting.encrypted!==true);
  const conversion = file.conversion, target = conversion?.target_encrypted ?? (modern?choice:!archiveEncrypted(file));
  const running = conversion?.state === "pending" || conversion?.state === "running";
  const targetName = target ? "加密" : "未加密";
  const submit = async () => {
    if (busy.current || disabled) return;
    busy.current = true; setSubmitting(true); setError("");
    try {
      const response = await api.post<{ task_id: number }>(`/files/${encodeURIComponent(file.id)}/convert`, { encrypted: target, confirm: "CONVERT" });
      await onAccepted(response.task_id);
    } catch (cause: any) { if (alive.current) setError(cause.message || "转换提交失败，请重试。"); }
    finally { busy.current = false; if (alive.current) setSubmitting(false); }
  };
  return <div className="archive-conversion">
    <h4>加密转换</h4>
    {running ? <p className="detail-hint" role="status">{conversion.phase === "cleanup" ? "新归档已准备，正在完成原件替换。" : conversion.phase === "archive" ? "正在重新归档并验证新原件。" : "正在准备新的归档原件。"}</p> : conversion?.state === "failed" ? <><p className="detail-error" role="alert">归档转换未完成。{conversion.error || "请重试转换，或在任务页查看原因。"}</p><button type="button" className="btn" disabled={disabled || submitting} onClick={() => void submit()}>{submitting ? "正在重试…" : "重试转换"}</button></> : conversion?.state === "done" ? <p className="detail-hint" role="status">{conversion.cleanup_pending ? "转换完成，旧副本待清理；旧编号已停止通过本站访问。" : "转换完成，正在切换到新原件…"}</p> : <>
      {modern?<><p className="detail-hint">当前设置：{setting.label}。副本：{archives.filter(location=>location.encrypted).length} 个加密、{archives.filter(location=>location.encrypted===false).length} 个未加密。转换按存储分别重写并校验，保留分片内容、名称、相册和日期。</p><label>目标加密方式<select className="select" aria-label="目标加密方式" value={String(choice)} disabled={disabled||submitting} onChange={event=>setChoice(event.target.value==="true")}><option value="true">加密（按存储设置）</option><option value="false">未加密</option></select></label><p className="detail-hint">选择加密时，仅允许加密的存储保存密文；当前没有允许加密的存储也可转换，副本仍为明文。选择未加密时，目标归档副本均保存明文。新副本校验后才切换，原文件在完成前保留。</p></>:<p className="detail-hint">{archiveEncrypted(file) ? "转为未加密后，可临时或永久发布。" : "转为加密后，仅支持临时发布。"}新归档验证后替换，旧编号会停止通过本站访问。</p>}
      <ConfirmButton type="button" className="btn" disabled={disabled || submitting || file.archive_state !== "done"} confirmLabel="确认转换" busyLabel="正在提交…" resetKey={`${file.id}:${target}`} description={`转为${targetName}后，新归档验证通过才替换原件，保留名称、相册和上传时间，新原件默认不发布。会尝试清理旧 TG／本地归档及永久、临时副本，清理失败不影响新文件；旧地址停止通过本站访问，外部副本和缓存可能尚未删除。转换后需要重新获取链接。`} onConfirm={submit}>转为{targetName}</ConfirmButton>
    </>}
    {admin && (running || conversion?.state === "failed") && <Link className="btn btn-ghost" to="/admin/tasks">查看转换任务</Link>}
    {dirty && <p className="detail-hint">请先保存名称更改，再转换归档格式。</p>}
    {error && <p className="detail-error" role="alert">{error}</p>}
  </div>;
}
