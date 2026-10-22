import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import {Link} from "react-router-dom";
import { IconFile, IconFolder } from "@tabler/icons-react";
import { api, fmtSize } from "../api";
import type { Album } from "../api";
import { useToast } from "../App";
import { filesUpdated, useDialog } from "./interactions";
import ConfirmButton from "./ConfirmButton";
import "./local-import.css";

type Batch = { id: number; task_id?:number; paths: string[]; album_id: number; encrypted: boolean; recursive: boolean; schedule: string; state: string; error: string; counts: Record<string, number>; bytes: number };
type Item = { id: number; path: string; size: number; state: string; error: string; file_ids: string[]; existing_album?: string };
type Entry = { path: string; name: string; directory: boolean; size: number };
const names: Record<string, string> = { scanning: "扫描中", ready: "等待开始", running: "导入中", paused: "已暂停", canceled: "已取消", done: "已完成", done_errors: "部分失败", failed: "失败", pending: "待处理", archiving: "归档中", duplicate: "重复跳过", skipped: "已跳过" };
const base = "/api/admin/local-imports";

export default function LocalImport({ albums, onClose, paths, directoryImport=false }: { albums: Album[]; onClose: () => void; paths?: string[]; directoryImport?: boolean }) {
  const ref = useRef<HTMLDivElement>(null), toast = useToast();
  const [targetAlbums,setTargetAlbums]=useState(albums),[owners,setOwners]=useState<{id:string;username:string}[]>([]),[targetsReady,setTargetsReady]=useState(false),[targetsError,setTargetsError]=useState("");
  const [directory, setDirectory] = useState(""), [pathDraft, setPathDraft] = useState(""), [parent, setParent] = useState(""), [isDirectory, setIsDirectory] = useState(true), [entries, setEntries] = useState<Entry[]>([]), [next, setNext] = useState(0);
  const [selected, setSelected] = useState<Set<string>>(()=>new Set(paths)), [album, setAlbum] = useState<number | null>(null);
  const hasAlbum = targetsReady && album !== null && (album === 0 || targetAlbums.some(value => value.id === album));
  const [encrypted, setEncrypted] = useState(false), [recursive, setRecursive] = useState(true), [schedule, setSchedule] = useState("now");
  const [batches, setBatches] = useState<Batch[]>([]), [id, setID] = useState<number | null>(null), [batch, setBatch] = useState<Batch | null>(null), [items, setItems] = useState<Item[]>([]);
  const [offset, setOffset] = useState(0), [revision, setRevision] = useState(0), [busy, setBusy] = useState(false), [loading, setLoading] = useState(false), [error, setError] = useState("");
  const [itemFilter,setItemFilter]=useState("all");
  const failureShown=useRef(0);
  const alive = useRef(true), browseSequence = useRef(0);
  const loadTargets=useCallback(async()=>{setTargetsReady(false);setTargetsError("");try{const [catalog,accounts]=await Promise.all([api.get<{items:Album[]}>("/api/admin/albums?owner_id=all"),api.get<{items:{id:string;username:string;disabled:boolean;deleting:boolean}[]}>("/api/admin/accounts")]);if(alive.current){const active=accounts.items.filter(value=>!value.disabled&&!value.deleting);setOwners(active);setTargetAlbums(catalog.items.filter(value=>active.some(owner=>owner.id===value.owner_id)));setTargetsReady(true)}}catch(cause:any){if(alive.current)setTargetsError(cause.message||"目标相册读取失败，请重试。")}},[]);
  const close = () => { if (!busy) onClose(); };
  useDialog(ref, close);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  useEffect(()=>{void loadTargets()},[loadTargets]);
  const history = useCallback(async () => { const data = await api.get<{ items: Batch[] }>(base); if (alive.current) setBatches(data.items); }, []);
  const browse = useCallback(async (p: string, start = 0) => {
    const sequence = ++browseSequence.current; setLoading(true); setError("");
    try {
      const data = await api.get<{ path: string; parent: string; directory: boolean; items: Entry[]; next_offset: number }>(`${base}/browse?${new URLSearchParams({ path: p, offset: String(start) })}`);
      if (!alive.current || sequence !== browseSequence.current) return;
      setDirectory(data.path); setPathDraft(data.path); setParent(data.parent); setIsDirectory(data.directory); setEntries(previous => start ? [...previous, ...data.items] : data.items); setNext(data.next_offset);
      if (!data.directory) setSelected(previous => new Set([...previous, data.path]));
    } catch (cause: any) { if (alive.current && sequence === browseSequence.current) setError(cause.message); }
    finally { if (alive.current && sequence === browseSequence.current) setLoading(false); }
  }, []);
  useEffect(() => { if (!paths) void browse(""); void history().catch(cause => setError(cause.message)); }, [browse, history, paths]);
  useEffect(() => {
    if (!id) return;
    let stopped = false, timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      try {
        const data = await api.get<{ batch: Batch; items: Item[] }>(`${base}/${id}?offset=${offset}&state=${itemFilter}`);
        if (stopped) return;
        setBatch(data.batch); setItems(data.items);
        if(data.batch.state==="done_errors"&&(data.batch.counts.failed||0)>0&&failureShown.current!==id){failureShown.current=id;setItemFilter("failed");setOffset(0)}
        setBatches(previous => previous.map(value => value.id === data.batch.id ? data.batch : value));
        if (["running", "scanning", "paused"].includes(data.batch.state)) timer = setTimeout(load, 2000);
        if (["done", "done_errors"].includes(data.batch.state)) filesUpdated();
      } catch (cause: any) { if (!stopped) setError(cause.message); }
    };
    void load(); return () => { stopped = true; clearTimeout(timer); };
  }, [id, offset, revision, itemFilter]);
  const act = async (action: string) => {
    if (!id || busy) return;
    setBusy(true); setError("");
    try { await api.post(`${base}/${id}/${action}`); setRevision(value => value + 1); await history(); filesUpdated(); }
    catch (cause: any) { setError(cause.message); }
    finally { if (alive.current) setBusy(false); }
  };
  const scan = async () => {
    if (busy || !selected.size || !hasAlbum) return;
    setBusy(true); setError("");
    try {
      const data = await api.post<{ id: number }>(base, { paths: [...selected], album_id: album, encrypted, recursive, schedule });
      if (alive.current) { setBatch(null); setOffset(0); setID(data.id); await history(); }
    } catch (cause: any) { if (alive.current) setError(cause.message); }
    finally { if (alive.current) setBusy(false); }
  };
  const choose = (p: string) => setSelected(previous => { const value = new Set(previous); value.has(p) ? value.delete(p) : value.add(p); return value; });
  const chooseBatch = (value: number | null) => { setID(value); setBatch(null); if (value === null) setAlbum(null); setItems([]); setOffset(0);setItemFilter("all");failureShown.current=0; setError(""); setRevision(value => value + 1); };
  const count = batch ? Object.values(batch.counts).reduce((sum, n) => sum + n, 0) : 0;
  const processed = batch ? (batch.counts.done || 0) + (batch.counts.duplicate || 0) + (batch.counts.skipped || 0) + (batch.counts.failed || 0) : 0;
  const outcome = batch ? [["done", "已导入"], ["duplicate", "重复跳过"], ["skipped", "已跳过"], ["failed", "失败"]].filter(([key]) => batch.counts[key]).map(([key, label]) => `${label} ${batch.counts[key]}`).join(" · ") : "";
  const visibleCount=itemFilter==="failed"?batch?.counts.failed||0:count;
  return createPortal(<><div className="overlay" onClick={close} /><div ref={ref} className="modal local-import-dialog" role="dialog" aria-modal="true" aria-labelledby="local-import-title">
    <div className="dhead"><h2 id="local-import-title">{paths ? "导入到相册" : "服务器导入"}</h2><button type="button" className="btn btn-ghost" disabled={busy} onClick={close}>关闭</button></div>
    <div className="local-import-body">
      <p className="detail-hint">保留源文件，导入后归目标相册的用户所有，默认不公开。</p>
      {error && <p className="detail-error" role="alert">{error}</p>}
      {!id ? <>
        {!paths && <><form className="local-import-path" onSubmit={event => { event.preventDefault(); if (pathDraft.trim()) void browse(pathDraft.trim()); }}><label htmlFor="import-path">导入路径</label><input id="import-path" className="input" placeholder="输入文件或目录路径" value={pathDraft} onChange={event => setPathDraft(event.target.value)} autoComplete="off" spellCheck={false} /><button type="submit" className="btn" disabled={loading || !pathDraft.trim()}>打开</button></form>
        <div className="local-import-browser"><div className="local-import-browser-head">{isDirectory ? <label className="local-import-check"><input type="checkbox" aria-label="选择整个当前目录" disabled={!directory || loading} checked={selected.has(directory)} onChange={() => choose(directory)} />整个目录</label> : <span>已选择文件</span>}<button type="button" className="btn btn-sm btn-ghost" disabled={loading || !parent || parent === directory} onClick={() => void browse(parent)}>上一级</button></div>
        <div className="local-import-files" aria-label="服务器来源文件" aria-busy={loading}>
          {!entries.length && <p className="detail-hint">{loading ? "正在读取目录…" : "当前没有可显示的文件或目录。"}</p>}
          {entries.map(entry => <div className="local-import-file" key={entry.path}><input type="checkbox" aria-label={`选择${entry.directory ? "目录" : "文件"} ${entry.name}`} checked={selected.has(entry.path)} onChange={() => choose(entry.path)} />{entry.directory ? <IconFolder size={17} aria-hidden="true" /> : <IconFile size={17} aria-hidden="true" />}{entry.directory ? <button type="button" className="text-button" disabled={loading} onClick={() => void browse(entry.path)}>{entry.name}/</button> : <span>{entry.name}</span>}<small>{entry.directory ? "目录" : fmtSize(entry.size)}</small></div>)}
          {!!next && <button type="button" className="btn" disabled={loading} onClick={() => void browse(directory, next)}>加载更多</button>}
        </div></div></>}
        <div className="local-import-target"><label htmlFor="import-album">目标相册</label><select id="import-album" className="select" required disabled={!targetsReady} value={album !== null && hasAlbum ? album : ""} onChange={event => setAlbum(event.target.value === "" ? null : Number(event.target.value))}><option value="" disabled>{targetsReady?"请选择用户的相册（必选）":"正在读取相册…"}</option><option value={0}>自己 / 未分类</option>{owners.map(owner=><optgroup key={owner.id} label={owner.username}>{targetAlbums.filter(value=>value.owner_id===owner.id&&!(owner.id==="admin:1"&&value.is_unclassified)).map(value=><option key={value.id} value={value.id}>{owner.username} / {value.name}</option>)}</optgroup>)}</select></div>
        {targetsError&&<p className="detail-error" role="alert">{targetsError}<button type="button" className="btn btn-ghost" onClick={()=>void loadTargets()}>重新读取相册</button></p>}
        <div className="local-import-options" role="group" aria-label="导入选项"><label className="local-import-check"><input type="checkbox" checked={encrypted} onChange={event => setEncrypted(event.target.checked)} />加密归档</label>{(!paths || directoryImport) && <label className="local-import-check"><input type="checkbox" checked={recursive} onChange={event => setRecursive(event.target.checked)} />包含子目录</label>}<label className="local-import-check"><input type="checkbox" checked={schedule === "idle"} onChange={event => setSchedule(event.target.checked ? "idle" : "now")} />空闲处理</label></div>
        {!!selected.size && <details className="local-import-disclosure"><summary>查看已选 {selected.size} 项</summary><ul className="local-import-selection">{[...selected].map(p => <li key={p}><span>{p}</span><button type="button" className="btn btn-ghost" onClick={() => choose(p)}>移除</button></li>)}</ul><button type="button" className="btn btn-ghost" onClick={() => setSelected(new Set())}>清空选择</button></details>}
      </> : !batch ? <p role="status">正在读取批次…</p> : <>
        <div className="local-import-summary"><strong>{names[batch.state] || batch.state}</strong><span>{batch.album_id ? targetAlbums.find(value => value.id === batch.album_id)?.name || "相册已删除" : "未分类"} · {batch.encrypted ? "加密" : "未加密"} · 私有</span><span>{count} 个源文件 · {fmtSize(batch.bytes)}{batch.state !== "ready" ? ` · 已处理 ${processed} 项` : ""}</span></div>
        {batch.error && <p className="detail-error" role="alert">{batch.error}</p>}
        <p className="detail-hint">{batch.state === "ready" ? `${batch.counts.pending || 0} 项待导入${outcome ? ` · ${outcome}` : ""}` : outcome || "正在准备…"}</p>
        {batch.task_id&&<Link className="text-button" to={`/admin/logs?group_id=${batch.task_id}`}>查看本次操作记录 #{batch.task_id}</Link>}
        {["running", "paused"].includes(batch.state) && <p className="detail-hint">{batch.state === "paused" ? "已暂停接收新文件，当前已接收文件会继续归档。" : batch.schedule === "idle" ? "等待空闲时处理；上传、浏览原件等操作优先。" : "正在按顺序处理，关闭页面后仍会继续。"}</p>}
        <details className="local-import-disclosure" open={batch.state === "done_errors"}><summary>文件明细（{count}）</summary>
        <div className="operation-item-controls" role="group" aria-label="导入明细范围"><button type="button" className={`btn btn-sm ${itemFilter==="failed"?"":"btn-ghost"}`} aria-pressed={itemFilter==="failed"} onClick={()=>{setItemFilter("failed");setOffset(0)}}>失败项（{batch.counts.failed||0}）</button><button type="button" className={`btn btn-sm ${itemFilter==="all"?"":"btn-ghost"}`} aria-pressed={itemFilter==="all"} onClick={()=>{setItemFilter("all");setOffset(0)}}>全部文件</button></div>
        {!items.length&&<p className="detail-hint">{itemFilter==="failed"?"当前没有失败项。":"当前没有文件明细。"}</p>}
        <div className="local-import-results" aria-label="导入文件结果">{items.map(item => <div key={item.id} className="local-import-result"><div><strong>{item.path}</strong><small>{fmtSize(item.size)}</small></div><div><span>{names[item.state] || item.state}</span>{item.error && <p className={item.state === "failed" ? "detail-error" : "detail-hint"}>{item.error}{item.existing_album ? ` · 已有相册：${item.existing_album}` : ""}</p>}</div></div>)}</div>
        {visibleCount > 100 && <div className="local-import-actions"><button className="btn" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - 100))}>上一页</button><span>第 {Math.floor(offset / 100) + 1} 页</span><button className="btn" disabled={offset + items.length >= visibleCount} onClick={() => setOffset(offset + 100)}>下一页</button></div>}
        </details>
      </>}
      {!!batches.length && <details className="local-import-disclosure"><summary>导入记录（{batches.length}）</summary><div className="local-import-history"><label className="sr-only" htmlFor="import-history">最近批次</label><select id="import-history" className="select" disabled={busy} value={id || ""} onChange={event => chooseBatch(Number(event.target.value) || null)}><option value="">选择导入记录…</option>{batches.map(value => <option key={value.id} value={value.id}>#{value.id} · {names[batch && value.id === batch.id ? batch.state : value.state] || value.state} · {value.paths.map(p => p.split("/").pop() || "/").join("、")}</option>)}</select><button type="button" className="btn btn-ghost" disabled={busy} onClick={() => { void history().catch(cause => setError(cause.message)); if (id) { chooseBatch(id); toast("正在刷新记录"); } }}>刷新记录</button></div></details>}
    </div>
    <div className="local-import-footer">
      {!id ? <><span role="status">{selected.size ? `已选 ${selected.size} 项${hasAlbum ? "" : " · 请选择目标相册"}` : "请选择文件或目录"}</span><button type="button" className="btn btn-crayon" disabled={busy || !selected.size || !hasAlbum} onClick={() => void scan()}>{busy ? "扫描中…" : "预览导入"}</button></> : <>
        <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => chooseBatch(null)}>新建导入</button>
        <div className="local-import-actions">
          {batch && ["scanning", "ready", "running", "paused", "failed"].includes(batch.state) && <ConfirmButton type="button" className="btn btn-ghost" disabled={busy} confirmLabel="确认取消" busyLabel="取消中…" resetKey={`${batch.id}:${batch.state}`} description="将停止接收剩余文件。已接收的文件会继续归档，源文件始终保留。" onConfirm={()=>act("cancel")}>取消剩余导入</ConfirmButton>}
          {batch?.state === "ready" && <button type="button" className="btn btn-crayon" disabled={busy || !batch.counts.pending} onClick={() => void act("start")}>开始导入</button>}
          {batch?.state === "running" && <button type="button" className="btn" disabled={busy} onClick={() => void act("pause")}>暂停导入</button>}
          {batch?.state === "paused" && <button type="button" className="btn btn-crayon" disabled={busy} onClick={() => void act("resume")}>继续导入</button>}
          {batch?.state === "done_errors" && <button type="button" className="btn btn-crayon" disabled={busy} onClick={() => void act("retry")}>只重试失败项</button>}
          {batch?.state === "failed" && <button type="button" className="btn btn-crayon" disabled={busy} onClick={() => void act("retry")}>重新扫描</button>}
          {batch && ["done", "done_errors"].includes(batch.state) && <Link className="btn" to={`/admin?owner_id=${encodeURIComponent(batch.album_id ? targetAlbums.find(value=>value.id===batch.album_id)?.owner_id || "admin:1" : "admin:1")}&album_id=${batch.album_id || "unclassified"}`} onClick={close}>查看目标相册</Link>}
          {batch && ["done", "canceled"].includes(batch.state) && <button type="button" className="btn btn-crayon" onClick={close}>完成</button>}
        </div>
      </>}
    </div>
  </div></>, document.body);
}
