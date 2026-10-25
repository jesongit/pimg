import { useCallback, useEffect, useId, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { api, fmtSize, fmtTime } from "../api";
import { useToast } from "../App";
import { useDialog, useUnsavedChanges } from "./interactions";
import StorageSettings from "./StorageSettings";
import { useLibraryPopover } from "./LibraryPopover";
import ConfirmButton from "./ConfirmButton";
import "./users.css";
import "./settings.css";
import "./storage-page.css";

type Config = Record<string, any> & { encrypt?: boolean; split_video?: boolean; video_part_mb?: number; priority?: number };
type Health = { available: boolean | null; latency_ms: number; bytes_per_second: number; checked_at: number; error?: string };
type Instance = { id: string; name: string; driver: string; enabled: boolean; configured: boolean; config: Config; capabilities: string[]; health?: Health };
type Field = { key: string; label: string; hint?: string; type: string; required: boolean };
type Driver = { id: string; name: string; fields: Field[] };
type Response = { drivers: Driver[]; instances: Instance[]; policy: { archives: string[]; permanent: string; temporary: string } };
type Editor = { id: string; name: string; driver: string; enabled: boolean; config: Config };
const defaults = (driver: string): Config => ({ encrypt: driver === "telegram", split_video: driver === "telegram", video_part_mb: 256, priority: 0 });
const known = new Set(["directory", "cold_directory", "endpoint", "access_key", "secret_key", "bucket", "prefix", "public_base", "temp_public_base", "cleanup_bucket", "temp_cache_seconds", "topic_id", "cdn_zone", "cdn_token"]);

export default function StoragePage() {
  const toast = useToast();
  const [data, setData] = useState<Response | null>(()=>api.cached<Response>("/api/admin/storage")||null), [query, setQuery] = useState(""), [tab, setTab] = useState("list");
  const [error, setError] = useState(""), [busy, setBusy] = useState(""), [editor, setEditor] = useState<Editor | null>(null);
  const [editorDirty,setEditorDirty]=useState(false),[settingsDirty,setSettingsDirty]=useState(false);
  useUnsavedChanges(editorDirty||settingsDirty);
  const load = useCallback(async () => { setData(await api.get<Response>("/api/admin/storage")); }, []);
  useEffect(() => { void load().catch(error => setError(error.message)); const refresh = () => void load().catch(() => {}); window.addEventListener("pimg:policy-updated", refresh); return () => window.removeEventListener("pimg:policy-updated", refresh); }, [load]);
  useEffect(()=>{if(tab!=="list")return;const refresh=()=>{if(!document.hidden)void load().catch(()=>{})};const timer=setInterval(refresh,10000);document.addEventListener("visibilitychange",refresh);return()=>{clearInterval(timer);document.removeEventListener("visibilitychange",refresh)}},[load,tab]);
  const action = async (key: string, work: () => Promise<unknown>, message: string) => {
    if (busy) return; setBusy(key); setError("");
    try { await work(); await load(); window.dispatchEvent(new Event("pimg:policy-updated")); toast(message); }
    catch (error: any) { setError(error.message || "操作失败，请重试"); }
    finally { setBusy(""); }
  };
  if (!data) return <div role={error ? "alert" : "status"}>{error || "正在读取存储…"}{error && <button className="btn" onClick={() => void load()}>重试</button>}</div>;
  const shown = data.instances.filter(instance => `${instance.name} ${instance.driver}`.toLowerCase().includes(query.toLowerCase()));
  return <div className="storage-page users-page">
    <div className="users-heading"><div><h1>存储</h1><p>归档可多选并自动同步，发布和临时各选一个首选；故障时自动降级。</p></div><button className="btn btn-crayon" onClick={() => setEditor({ id: "", name: "", driver: "local", enabled: true, config: defaults("local") })}>添加存储</button></div>
    <nav className="storage-page-tabs" aria-label="存储管理分类">{[["list", "存储列表"], ["policy", "用途与同步"], ["connection", "连接管理"], ["migration", "整站迁移"]].map(([id, name]) => <button key={id} className={`btn${tab === id ? " btn-crayon" : " btn-ghost"}`} aria-current={tab === id ? "page" : undefined} onClick={() => {if(id!==tab&&(!settingsDirty||confirm("放弃尚未保存的存储设置？"))){setSettingsDirty(false);setTab(id);}}}>{name}</button>)}</nav>
    {error && <div className="users-error" role="alert">{error}</div>}
    {tab === "list" ? <>
      <div className="users-toolbar"><label className="users-search"><span className="sr-only">搜索存储</span><input className="input" type="search" placeholder="搜索名称或存储方式" value={query} onChange={event => setQuery(event.target.value)} /></label><span className="users-count">{shown.length} 个存储</span><button className="btn" disabled={!!busy} onClick={() => void action("probe", () => api.post("/api/admin/storage/probe", {}), "已安排检测，仅更新可用状态和延迟")}>检测可用性</button><button className="btn btn-ghost" disabled={!!busy} onClick={() => void load()}>刷新状态</button></div>
      <div className="users-registry storage-registry"><table className="users-table" aria-label="存储列表"><colgroup><col className="storage-col-name"/><col className="storage-col-priority"/><col className="storage-col-health"/><col className="storage-col-roles"/><col className="storage-col-mode"/><col className="storage-col-actions"/></colgroup><thead><tr><th scope="col">存储</th><th scope="col">优先级</th><th scope="col">状态与速度</th><th scope="col">用途</th><th scope="col">归档方式</th><th scope="col" className="storage-actions-heading">操作</th></tr></thead><tbody>{shown.map(instance => {
        const roles = [data.policy.archives.includes(instance.id) && "归档", data.policy.permanent === instance.id && "发布首选", data.policy.temporary === instance.id && "临时首选"].filter(Boolean);
        return <tr key={instance.id} data-storage-id={instance.id}>
          <th scope="row" className="storage-instance-cell"><div className="storage-instance"><strong className="users-name" title={instance.name}>{instance.name}</strong><span className="storage-driver">{data.drivers.find(driver => driver.id === instance.driver)?.name || instance.driver}</span></div></th>
          <td className="storage-priority-cell" data-label="优先级">{instance.config.priority || 0}</td>
          <td className="storage-health-cell" data-label="状态与速度"><StorageHealth instance={instance}/></td>
          <td className="storage-roles-cell" data-label="用途">{roles.join(" · ") || "降级候选"}</td>
          <td className="storage-mode-cell" data-label="归档方式"><div className="storage-mode"><span>{instance.config.encrypt ? "允许加密" : "明文存储"}</span><span className="storage-inline-secondary">{instance.config.split_video ? `无损分片 ${instance.config.video_part_mb || 256} MB` : "完整文件"}</span></div></td>
          <td className="storage-actions-cell" data-label="操作"><div className="users-actions" role="group" aria-label={`${instance.name}的操作`}><button className="btn btn-sm" disabled={!!busy} onClick={() => setEditor({ id: instance.id, name: instance.name, driver: instance.driver, enabled: instance.enabled, config: { ...defaults(instance.driver), ...instance.config } })}>编辑</button>{instance.enabled ? <ConfirmButton className="btn btn-sm btn-ghost" disabled={!!busy} confirmLabel="确认停用" busyLabel="停用中…" resetKey={instance.id} description={`停用「${instance.name}」后，它不再参与新的存储选择；已有副本保留。请确认还有其他可用存储。`} onConfirm={()=>action(instance.id, () => api.patch(`/api/admin/storage/instances/${instance.id}`, { name: instance.name, driver: instance.driver, enabled: false, config: instance.config }), "存储已停用，已有副本保留")}>停用</ConfirmButton> : <button className="btn btn-sm btn-ghost" disabled={!!busy} onClick={() => void action(instance.id, () => api.patch(`/api/admin/storage/instances/${instance.id}`, { name: instance.name, driver: instance.driver, enabled: true, config: instance.config }), "存储已启用")}>启用</button>}</div></td>
        </tr>;
      })}</tbody></table>{shown.length === 0 && <div className="empty">没有符合条件的存储。</div>}</div>
      <p className="settings-note">优先级数值越大越优先；同级优先使用实测速率更高的存储，暂无速率记录时比较延迟。定时检测不会修改你的存储选择。</p>
    </> : <StorageSettings key={tab} initialCategory={tab} compact onDirtyChange={setSettingsDirty} />}
    {editor && <StorageEditor onDirtyChange={setEditorDirty} value={editor} drivers={data.drivers} busy={!!busy} onClose={() => setEditor(null)} onSave={value => void action("instance", async () => { if (value.id) await api.patch(`/api/admin/storage/instances/${value.id}`, value); else await api.post("/api/admin/storage/instances", value); setEditor(null); }, "存储已保存，已有副本保持原来的加密与分片方式")} />}
  </div>;
}

function StorageHealth({instance}:{instance:Instance}) {
  const {health}=instance,popup=useLibraryPopover(340),id=useId();
  const state=!instance.enabled?"已停用":!instance.configured?"未配置 / 未登录":health?.available===true?"可用":health?.available===false?"暂不可用":"尚未检测";
  const tone=instance.enabled&&instance.configured?(health?.available===true?"is-available":health?.available===false?"is-unavailable":""):"";
  const latency=health&&health.checked_at>0&&Number.isFinite(health.latency_ms)&&health.latency_ms>=0?`${Math.round(health.latency_ms)} ms`:"延迟待测";
  const speed=health?.bytes_per_second?`${fmtSize(health.bytes_per_second)}/s`:"速率待记录";
  return <div className="storage-health">
    <button ref={popup.trigger} type="button" className={`storage-health-trigger ${tone}`} aria-label={`${instance.name}的检测详情：${state}`} aria-haspopup="dialog" aria-expanded={popup.open} aria-controls={popup.open?id:undefined} onClick={popup.toggle}><span>{state}</span><svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="m8 10 4 4 4-4"/></svg></button>
    <span className="storage-metrics"><span aria-label={`延迟：${latency}`}>{latency}</span><span className="storage-metric-divider" aria-hidden="true">·</span><span aria-label={`实测速率：${speed}`}>{speed}</span></span>
    {popup.open&&createPortal(<div ref={popup.popup} id={id} className="storage-health-popover" style={popup.position} role="dialog" aria-modal="false" aria-labelledby={`${id}-title`}><header><strong id={`${id}-title`}>检测详情</strong><button type="button" className="btn btn-sm btn-ghost" aria-label="关闭检测详情" onClick={()=>popup.close(true)}>关闭</button></header><p className="storage-health-name">{instance.name}</p><dl><div><dt>状态</dt><dd>{state}</dd></div><div><dt>延迟</dt><dd>{latency}</dd></div><div><dt>实测速率</dt><dd>{speed}</dd></div><div><dt>上次检测</dt><dd>{health?.checked_at?fmtTime(health.checked_at):"尚未检测"}</dd></div></dl>{health?.error&&<p className="storage-health-error">{health.error}</p>}</div>,document.body)}
  </div>;
}

function StorageEditor({ value, drivers, busy, onClose, onSave,onDirtyChange }: { onDirtyChange:(dirty:boolean)=>void; value: Editor; drivers: Driver[]; busy: boolean; onClose: () => void; onSave: (value: Editor) => void }) {
  const [draft, setDraft] = useState(value), ref = useRef<HTMLElement>(null);
  const dirty = JSON.stringify(draft) !== JSON.stringify(value);
  useEffect(()=>{onDirtyChange(dirty);return()=>onDirtyChange(false)},[dirty,onDirtyChange]);
  const close = () => { if (!busy && (!dirty || confirm("放弃尚未保存的存储设置？"))) onClose(); };
  useDialog(ref, close);
  const config = (key: string, value: any) => setDraft(draft => ({ ...draft, config: { ...draft.config, [key]: value } }));
  const fields = drivers.find(driver => driver.id === draft.driver)?.fields || [];
  const fieldsID = useId();
  return <div className="storage-editor-shade"><section ref={ref} className="storage-editor" role="dialog" aria-modal="true" aria-labelledby="storage-editor-title"><header><h2 id="storage-editor-title">{draft.id ? "编辑存储" : "添加存储"}</h2><button className="btn btn-ghost" type="button" onClick={close}>关闭</button></header><form onSubmit={event => { event.preventDefault(); onSave(draft); }}><fieldset disabled={busy}>
    <label>名称<input className="input" required maxLength={80} value={draft.name} onChange={event => setDraft({ ...draft, name: event.target.value })} /></label>
    <label>存储方式<select className="select" disabled={!!draft.id} value={draft.driver} onChange={event => setDraft({ ...draft, driver: event.target.value, config: defaults(event.target.value) })}>{drivers.map(driver => <option key={driver.id} value={driver.id}>{driver.name}</option>)}</select></label>
    {fields.filter(field => !(draft.config.bootstrap && (field.key === "prefix" || draft.driver === "telegram" && field.key === "topic_id"))).map(field => <label key={field.key}><span id={`${fieldsID}-${field.key}-label`}>{field.label}</span><input className="input" aria-labelledby={`${fieldsID}-${field.key}-label`} aria-describedby={field.hint ? `${fieldsID}-${field.key}-hint` : undefined} type={field.type === "password" ? "password" : field.type === "number" ? "number" : "text"} required={field.required && !draft.id} autoComplete={field.type === "password" ? "new-password" : "off"} value={draft.config[field.key] ?? draft.config.options?.[field.key.replace(/^options\./, "")] ?? ""} onChange={event => { const value = field.type === "number" ? Number(event.target.value) : event.target.value; if (known.has(field.key)) config(field.key, value); else config("options", { ...draft.config.options, [field.key.replace(/^options\./, "")]: String(value) }); }} />{field.hint && <small id={`${fieldsID}-${field.key}-hint`}>{field.hint}</small>}</label>)}
    {draft.driver === "r2" && <><label>CDN Zone ID<input className="input" value={draft.config.cdn_zone || ""} onChange={event => config("cdn_zone", event.target.value)} /></label><label>CDN API Token<input className="input" type="password" autoComplete="new-password" value={draft.config.cdn_token || ""} onChange={event => config("cdn_token", event.target.value)} /><small>留空保留已保存凭据。</small></label><label>临时公开域名<input className="input" type="url" value={draft.config.temp_public_base || ""} onChange={event => config("temp_public_base", event.target.value)} /></label></>}
    <label>优先级<input className="input" type="number" required min={0} max={1000000} value={draft.config.priority || 0} onChange={event => config("priority", Number(event.target.value))} /><small>数值越大越优先；同级比较实际传输速度与检测延迟。</small></label>
    <label className="storage-check"><input type="checkbox" checked={!!draft.config.encrypt} onChange={event => config("encrypt", event.target.checked)} />允许加密保存</label><p className="settings-hint">上传或导入选择加密，且此处开启时才加密保存。</p>
    <label className="storage-check"><input type="checkbox" checked={!!draft.config.split_video} onChange={event => config("split_video", event.target.checked)} />视频无损分片</label>
    <label>分片大小（MB）<input className="input" type="number" min={1} max={4096} required disabled={!draft.config.split_video} value={draft.config.video_part_mb || 256} onChange={event => config("video_part_mb", Number(event.target.value))} /><small>该存储独立配置，默认 256 MB。无法无损切分时保留来源并报告原因。</small></label>
    <label className="storage-check"><input type="checkbox" checked={draft.enabled} onChange={event => setDraft({ ...draft, enabled: event.target.checked })} />启用存储</label>
    {draft.id && <p className="settings-hint">加密、分片设置影响后续写入。修改已有数据的存储位置，请添加新存储后切换用途。</p>}
    </fieldset><footer><button className="btn" type="button" disabled={busy} onClick={close}>取消</button><button className="btn btn-crayon" disabled={busy}>{busy ? "保存中…" : "保存存储"}</button></footer></form></section></div>;
}
