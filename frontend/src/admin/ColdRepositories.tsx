import { useCallback, useEffect, useState } from "react";
import type { FormEvent } from "react";
import { Link } from "react-router-dom";
import { api, fmtTime } from "../api";
import { useToast } from "../App";

type Driver = { id: string; name: string; fields: { key: string; label: string; hint: string }[] };
type Repository = { id: number; name: string; driver: string; config: Record<string, string>; enabled: boolean; pending: number; synced: number; failed: number; running: number; last_synced: number; last_error: string };
type Response = { items: Repository[]; drivers: Driver[]; primary: { id: "primary"; driver: string; enabled: true } };

export default function ColdRepositories({ onDirtyChange }: { onDirtyChange: (dirty: boolean) => void }) {
  const toast = useToast();
  const [data, setData] = useState<Response | null>(null), [error, setError] = useState("");
  const [adding, setAdding] = useState(false), [name, setName] = useState(""), [driver, setDriver] = useState("local");
  const [config, setConfig] = useState<Record<string, string>>({}), [busy, setBusy] = useState<string | number | null>(null);
  const [formError, setFormError] = useState("");
  const load = useCallback(async () => {
    try { setData(await api.get<Response>("/api/admin/cold-repositories")); setError(""); }
    catch (e: any) { setError(e.message || "副仓状态暂时无法读取，请重试。"); }
  }, []);
  useEffect(() => { void load(); const timer = setInterval(load, 10000); return () => clearInterval(timer); }, [load]);
  useEffect(() => { onDirtyChange(adding && (!!name || Object.values(config).some(Boolean))); }, [adding, name, config, onDirtyChange]);
  useEffect(() => () => onDirtyChange(false), [onDirtyChange]);
  const selectedDriver = data?.drivers.find((item) => item.id === driver);
  const add = async (event: FormEvent) => {
    event.preventDefault(); if (busy || !name.trim() || !selectedDriver) return;
    setBusy("add"); setFormError("");
    try { await api.post("/api/admin/cold-repositories", { name: name.trim(), driver, config, role: "secondary" }); setName(""); setConfig({}); setAdding(false); await load(); toast("副仓已添加，勾选后开始同步"); }
    catch (e: any) { setFormError(e.message || "添加失败，输入已保留。"); }
    finally { setBusy(null); }
  };
  const toggle = async (repo: Repository) => {
    setBusy(repo.id);
    try { await api.patch(`/api/admin/cold-repositories/${repo.id}`, { enabled: !repo.enabled }); await load(); toast(repo.enabled ? "副仓已暂停" : "已启用副仓，将在空闲时同步"); }
    catch (e: any) { toast(e.message, "err"); }
    finally { setBusy(null); }
  };
  return <div className="cold-repositories">
    <div className="cold-repositories-head"><div><h3>副仓同步</h3><p className="settings-note">勾选的副仓按相册保存副本。同步和删除仅在空闲时执行；上传、预览和普通任务优先。</p></div><button type="button" className="btn" disabled={!!busy || !data} onClick={() => setAdding(true)}>添加副仓</button></div>
    {error && <div className="settings-feedback is-err" role="alert">{error} <button type="button" className="btn btn-sm" onClick={load}>重新读取</button></div>}
    {!data && !error && <p className="settings-note" role="status">正在读取副仓…</p>}
    {data?.items.length === 0 && <p className="settings-note">尚未添加副仓。主仓独立处理日常归档，添加副仓后需自行勾选启用。</p>}
    {!!data?.items.length && <ul className="cold-repository-list">{data.items.map((repo) => {
      const state = !repo.enabled ? "已暂停" : repo.running ? "同步中" : repo.failed ? "同步失败" : repo.pending ? "等待空闲" : repo.synced ? "已同步" : "等待扫描";
      return <li key={repo.id} className="cold-repository-row">
        <label className="cold-repository-choice"><input type="checkbox" checked={repo.enabled} disabled={busy !== null} onChange={() => toggle(repo)} aria-label={`同步到 ${repo.name}`} /><span><strong>{repo.name}</strong><span>{data.drivers.find((item) => item.id === repo.driver)?.name || repo.driver}{repo.config.directory ? ` · ${repo.config.directory}` : ""}</span></span></label>
        <div className="cold-repository-state"><span className={`chip ${repo.enabled && repo.failed ? "chip-err" : repo.enabled && repo.running ? "chip-warn" : repo.enabled && repo.synced ? "chip-ok" : ""}`}>{state}</span><p className="settings-hint">已同步 {repo.synced} · 等待 {repo.pending} · 失败 {repo.failed}</p>{repo.last_synced > 0 && <p className="settings-hint">上次同步 {fmtTime(repo.last_synced)}</p>}</div>
        {!repo.synced && <button type="button" className="btn btn-ghost" disabled={busy !== null || !!repo.running} onClick={async () => { setBusy(repo.id); try { await api.del(`/api/admin/cold-repositories/${repo.id}`); await load(); toast("副仓配置已移除"); } catch (e: any) { toast(e.message, "err"); } finally { setBusy(null); } }} aria-label={`移除副仓 ${repo.name}`}>移除配置</button>}
        {repo.last_error && <p className="cold-repository-error settings-feedback is-err" role="alert">{repo.last_error}</p>}
      </li>;
    })}</ul>}
    {adding && <form className="cold-repository-form" aria-label="添加副仓" onSubmit={add}>
      <fieldset className="settings-fields" disabled={busy !== null}>
        <div className="settings-field"><label htmlFor="replica-name">副仓名称</label><div className="settings-field-control"><input id="replica-name" className="input" required maxLength={80} value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：本地备份" /></div></div>
        <div className="settings-field"><label htmlFor="replica-driver">存储类型</label><div className="settings-field-control"><select id="replica-driver" className="select" value={driver} onChange={(e) => { setDriver(e.target.value); setConfig({}); }}>{data?.drivers.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select></div><p className="settings-hint">目前提供本地目录，其他存储接入后可在此选择。</p></div>
        {selectedDriver?.fields.map((field) => <div className="settings-field" key={field.key}><label htmlFor={`replica-${field.key}`}>{field.label}</label><div className="settings-field-control"><input id={`replica-${field.key}`} className="input" required maxLength={240} value={config[field.key] || ""} onChange={(e) => setConfig((current) => ({ ...current, [field.key]: e.target.value }))} aria-describedby={`replica-${field.key}-hint`} /></div><p className="settings-hint" id={`replica-${field.key}-hint`}>{field.hint}</p></div>)}
      </fieldset><div className="settings-actions"><button className="btn btn-crayon" disabled={busy !== null}>{busy === "add" ? "添加中…" : "添加副仓"}</button><button type="button" className="btn" disabled={busy !== null} onClick={() => { setAdding(false); setName(""); setConfig({}); setFormError(""); }}>取消</button><span className="settings-note">添加后默认不启用。</span></div>
      {formError && <p className="settings-feedback is-err" role="alert">{formError}</p>}
    </form>}
    <p className="settings-note">文件被未结束任务引用时，需先在<Link to="/admin/tasks">任务页</Link>取消并等待停止，才能提交删除。取消勾选仅暂停同步；删除任务仍会清理已有副本。</p>
  </div>;
}
