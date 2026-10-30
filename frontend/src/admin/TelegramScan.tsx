import FloatingFeedback from "../FloatingFeedback";
import { useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import type { Album, TelegramStatus, TelegramTopic } from "../api";
import ConfirmButton from "./ConfirmButton";

const stateNames: Record<string, string> = { pending: "话题任务等待中", creating: "话题创建中", running: "话题同步中", syncing: "话题同步中", moving: "正在迁移文件", deleting: "迁入未分类后删除", missing: "关联话题已缺失", failed: "话题处理未完成", error: "话题处理未完成", active: "话题已关联", done: "话题已关联", ready: "话题已关联", bound: "话题已关联" };
export function TelegramAlbumState({ album }: { album: Album }) {
  const tg = album.telegram;
  if (!tg) return null;
  return <span className={`album-telegram-state${tg.error || ["failed", "error", "missing"].includes(tg.state) ? " is-error" : ""}`} title={tg.error || undefined}>{tg.topic_id > 0 && !!tg.chat_id ? `TG 话题 ${tg.topic_id}` : "TG 话题"} · {stateNames[tg.state] || tg.state || "等待关联"}{tg.error && <span>{tg.error}</span>}</span>;
}

export default function TelegramScan({ album, onChanged, initialOpen = false }: { album: Album; onChanged: () => void; initialOpen?: boolean }) {
  const [open, setOpen] = useState(initialOpen), [busy, setBusy] = useState("");
  const [error, setError] = useState(""), [feedback, setFeedback] = useState("");
  const [since, setSince] = useState(""), [until, setUntil] = useState("");
  const [topics, setTopics] = useState<TelegramTopic[]>([]), [topicID, setTopicID] = useState(""), [choosing, setChoosing] = useState(false);
  const mounted = useRef(true), working = useRef(false);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const associated = !!album.telegram;
  const bound = !!album.telegram?.chat_id && album.telegram.topic_id > 0;
  const pending = ["pending", "creating", "running", "syncing", "moving", "deleting", "missing"].includes(album.telegram?.state || "");
  const scan = async (mode: "latest" | "date" | "resume", event?: FormEvent) => {
    event?.preventDefault();
    if (working.current || !bound || pending || mode === "date" && (!since || !!until && until < since)) return;
    working.current = true; setBusy(mode); setError(""); setFeedback("");
    try {
      await api.post(`/api/admin/albums/${album.id}/scan`, { mode, ...(mode === "date" ? { since, ...(until ? { until } : {}) } : {}) });
      if (mounted.current) { setFeedback(mode === "latest" ? "补新任务已安排，已导入的消息会自动跳过。" : mode === "resume" ? "已安排继续扫描，可在任务页查看进度。" : "日期扫描任务已安排，可在任务页查看进度。"); }
      onChanged();
    } catch (cause: any) { if (mounted.current) setError(cause.message || "扫描未能提交，请重试。"); }
    finally { working.current = false; if (mounted.current) setBusy(""); }
  };
  const findTopics = async () => {
    if (working.current) return;
    working.current = true; setBusy("topics"); setError("");
    try {
      const status = await api.get<TelegramStatus>("/api/admin/telegram");
      if (status.state !== "authorized" || !status.chat_id) throw new Error("请先在设置中登录 TG，等待系统完成归档群分配。");
      const response = await api.get<{ items: TelegramTopic[] }>(`/api/admin/telegram/topics?chat_id=${encodeURIComponent(status.chat_id)}`);
      if (mounted.current) { setTopics(response.items || []); setChoosing(true); }
    } catch (cause: any) { if (mounted.current) setError(cause.message || "话题读取失败，请重试。"); }
    finally { working.current = false; if (mounted.current) setBusy(""); }
  };
  const bind = async () => {
    if (working.current || !topicID || associated) return;
    working.current = true; setBusy("bind"); setError(""); setFeedback("");
    try { await api.post(`/api/admin/albums/${album.id}/telegram`, { topic_id: Number(topicID) }); if (mounted.current) { setFeedback("话题已关联。请先选择扫描起点并提交扫描，以启用后续自动同步。"); setChoosing(false); } onChanged(); }
    catch (cause: any) { if (mounted.current) setError(cause.message || "关联未完成，请重试。"); }
    finally { working.current = false; if (mounted.current) setBusy(""); }
  };
  return <details className="telegram-scan" open={open} onToggle={event => setOpen(event.currentTarget.open)}>
    <summary>TG 话题与扫描{bound && <span className="chip">话题 {album.telegram!.topic_id}</span>}</summary>
    <div className="telegram-scan-body">
      <TelegramAlbumState album={album} />
      {bound ? <>
        <p className="detail-hint">从关联话题补回 TG 中的文件。首次关联后需选择扫描起点并提交扫描，才会启用后续自动同步。可用一键补新从最近消息开始，或指定日期补历史区间；已导入的消息自动跳过。</p>
        <div className="telegram-scan-actions"><button type="button" className="btn" disabled={!!busy || pending} onClick={() => void scan("latest")}>{busy === "latest" ? "提交中…" : "一键补新"}</button><button type="button" className="btn btn-ghost" disabled={!!busy || pending} onClick={() => void scan("resume")}>{busy === "resume" ? "提交中…" : "继续上次扫描"}</button><Link className="btn btn-ghost" to="/admin/tasks">查看扫描任务</Link></div>
        <form className="telegram-scan-dates" onSubmit={event => void scan("date", event)} aria-label="按日期扫描 TG 话题"><label>开始日期<input className="input" type="date" required value={since} max={until || undefined} onChange={event => setSince(event.target.value)} disabled={!!busy || pending} /></label><label>结束日期（可选）<input className="input" type="date" value={until} min={since || undefined} onChange={event => setUntil(event.target.value)} disabled={!!busy || pending} /></label><button className="btn" disabled={!!busy || pending || !since || !!until && until < since}>{busy === "date" ? "提交中…" : "扫描指定日期"}</button></form>
        {album.telegram?.scan_date && <p className="detail-hint">扫描记录：{album.telegram.scan_date}</p>}
      </> : <>
        <p className="detail-hint">相册归档到所属账号专属群中的独立话题。可等待新话题创建，或首次关联已有话题，再扫描其中的文件。</p>
        {!album.is_home && !album.is_unclassified && !associated && <button type="button" className="btn" disabled={!!busy} onClick={() => void findTopics()}>{busy === "topics" ? "读取中…" : "关联已有话题…"}</button>}
        {associated && <p className="detail-hint" role="status">{pending && album.telegram?.state !== "missing" ? "话题任务正在处理，完成后会更新。" : "已有话题关联已保留，请在任务页查看状态和失败原因。"}<Link to="/admin/tasks">查看任务</Link></p>}
        {choosing && !bound && <div className="telegram-topic-binding"><label>已有话题<select className="select" aria-label="已有话题" value={topicID} onChange={event => setTopicID(event.target.value)} disabled={!!busy}><option value="">请选择话题</option>{topics.filter(topic => !topic.closed && topic.id > 1).map(topic => <option key={topic.id} value={topic.id}>{topic.name} · {topic.id}</option>)}</select></label>{!topics.some(topic => !topic.closed && topic.id > 1) && <p className="detail-hint">没有可关联的开放话题，请在 TG 中检查话题状态后重试。</p>}<ConfirmButton type="button" className="btn" disabled={!topicID || !!busy} confirmLabel="确认关联" busyLabel="提交中…" resetKey={`${album.id}:${topicID}`} description={`将关联「${topics.find(topic => String(topic.id) === topicID)?.name || "所选话题"}」。关联后固定使用此话题，扫描其中的文件会归入当前相册。`} onConfirm={bind}>关联此话题</ConfirmButton></div>}
      </>}
      {error && <FloatingFeedback className="detail-error" role="alert">{error}</FloatingFeedback>}{feedback && <FloatingFeedback className="telegram-scan-feedback" role="status">{feedback}</FloatingFeedback>}
    </div>
  </details>;
}
