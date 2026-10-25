import { useCallback, useEffect, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { api } from "../api";
import { useToast } from "../App";
import TelegramSettings from "./TelegramSettings";
import MigrationSettings from "./MigrationSettings";
import ProxyTrafficSettings from "./ProxyTrafficSettings";
import type { ProxyTraffic } from "./ProxyTrafficSettings";

type Config = { options?: Record<string, string>; directory?: string; endpoint?: string; bucket?: string; prefix?: string; public_base?: string; topic_id?: number; access_key?: string; secret_key?: string; bootstrap?: boolean; managed?: boolean; cold_directory?: string; temp_public_base?: string; cleanup_bucket?: string; temp_cache_seconds?: number; cdn_zone?: string; cdn_token?: string };
type Instance = { id: string; name: string; driver: string; enabled: boolean; configured: boolean; config: Config; capabilities: string[]; has_credentials: boolean };
type Policy = { archives: string[]; permanent: string; temporary: string; normal_mbps: number; accelerated_mbps: number; local_mbps: number; accelerated_start: string; accelerated_end: string; sync_cache_mb: number };
type Driver = { id: string; name: string; fields: { key: string; label: string; hint?: string; type: string; required: boolean }[] };
type Response = { data_root?: string; connection: {proxy_url: string; has_proxy: boolean; api_id: number; has_api_hash: boolean; proxy_traffic: ProxyTraffic}; drivers: Driver[]; instances: Instance[]; policy: Policy; pending_tasks: number; failed_tasks: number; expired_pending_cleanup: number };
const drivers: Record<string, string> = { local: "本地目录", telegram: "Telegram 归档", r2: "Cloudflare R2" };
function instancePosition(i: Instance) {
  if (i.driver === "r2" && i.config.bucket) return `桶：${i.config.bucket}${i.config.prefix ? ` / ${i.config.prefix}` : ""}`;
  return i.config.directory || i.config.bucket || (i.config.topic_id ? `话题 ${i.config.topic_id}` : "按账号自动建群、按相册建立话题");
}
function Field({ id, label, hint, children }: { id: string; label: string; hint?: string; children: ReactNode }) {
  return <div className="settings-field"><label htmlFor={id}>{label}</label><div className="settings-field-control">{children}</div>{hint && <p className="settings-hint" id={`${id}-hint`}>{hint}</p>}</div>;
}
export default function StorageSettings({ onDirtyChange = () => {}, initialCategory, compact = false }: { onDirtyChange?: (dirty: boolean) => void; initialCategory?: string; compact?: boolean }) {
  const toast = useToast();
  const [category, setCategory] = useState(initialCategory || (window.location.hash === "#settings-telegram" ? "connection" : "instances"));
  const initial=api.cached<Response>("/api/admin/storage");
  const [data, setData] = useState<Response | null>(initial||null), [policy, setPolicy] = useState<Policy | null>(initial?.policy||null);
  const [error, setError] = useState(""), [busy, setBusy] = useState(""), [adding, setAdding] = useState(false), [driver, setDriver] = useState("local"), [name, setName] = useState(""), [config, setConfig] = useState<Config>({});
  const [telegramDirty,setTelegramDirty]=useState(false);
  const [trafficDirty,setTrafficDirty]=useState(false);
  const [editing, setEditing] = useState("");
  const [proxy, setProxy] = useState<string | null>(null);
  const load = useCallback(async (reset = false) => {
    const next = await api.get<Response>("/api/admin/storage"); setData(next); setPolicy(old => reset || !old ? next.policy : old);
  }, []);
  useEffect(() => { void load(true).catch(e => setError(e.message)); }, [load]);
  useEffect(() => {
    const refresh = () => { void load().catch(e => setError(e.message)); };
    window.addEventListener("pimg:policy-updated", refresh);
    return () => window.removeEventListener("pimg:policy-updated", refresh);
  }, [load]);
  const dirty = !!policy && !!data && JSON.stringify(policy) !== JSON.stringify(data.policy);
  useEffect(() => { onDirtyChange(telegramDirty || trafficDirty || proxy !== null || dirty || adding && (!!name || Object.values(config).some(Boolean))); }, [telegramDirty, trafficDirty, proxy, dirty, adding, name, config, onDirtyChange]);
  useEffect(() => () => onDirtyChange(false), [onDirtyChange]);
  const act = async (key: string, work: () => Promise<unknown>, message: string) => {
    if (busy) return; setBusy(key); setError("");
    try { await work(); await load(); toast(message); }
    catch (e: any) { setError(e.message || "操作失败，请重试"); }
    finally { setBusy(""); }
  };
  if (!data || !policy) return <div role={error ? "alert" : "status"}>{error || "正在读取存储配置…"}{error && <button className="btn" onClick={() => void load(true).catch(e => setError(e.message))}>重新加载</button>}</div>;
  const set = <K extends keyof Policy>(key: K, value: Policy[K]) => setPolicy({ ...policy, [key]: value });
  const choices = (cap: string) => data.instances.filter(i => i.capabilities.includes(cap));
  const instanceSelect = (id: string, value: string, cap: string, update: (value: string) => void) => <select id={id} className="select" value={value} onChange={e => update(e.target.value)} required>{choices(cap).map(i => <option key={i.id} value={i.id} disabled={!i.enabled || !i.configured}>{i.name}{!i.configured ? "（未配置 / 未登录）" : ""}</option>)}</select>;
  const input = (field: Driver["fields"][number]) => { const {key, label, hint, type, required} = field; const option = key.startsWith("options."); const value = option ? config.options?.[key.slice(8)] : config[key as keyof Config];
    return <Field key={key} id={`storage-${key}`} label={label} hint={hint}><input id={`storage-${key}`} className="input" type={type} required={required && !(editing && type === "password")} autoComplete={type === "password" ? "new-password" : "off"} value={value === undefined ? "" : String(value)} onChange={e => setConfig(option ? {...config, options:{...config.options,[key.slice(8)]:e.target.value}} : { ...config, [key]: type === "number" ? Number(e.target.value) : e.target.value })} /></Field>;
  };

  return <div className={compact ? "storage-compact" : undefined}>
    <nav hidden={compact} className="settings-subnav" aria-label="存储设置分类">{[["instances","存储实例"],["policy","用途与同步"],["connection","连接管理"],["migration","整站迁移"]].map(([id,label])=><button type="button" key={id} aria-pressed={category===id} onClick={()=>setCategory(id)}>{label}{id==="policy"&&dirty?" · 未保存":""}</button>)}</nav>
    {category === "migration" && <MigrationSettings />}
    {error && <p className="settings-note" role="alert">{error}</p>}
    {category === "instances" && data.data_root && <p className="settings-note" style={{overflowWrap:"anywhere"}}>本机数据目录：{data.data_root}。数据库、密钥、TG 会话及默认本地存储集中保存在此目录。</p>}
    {category === "instances" && <div className="storage-connection-summary"><span>TG 归档按账号自动分配，登录和代理统一管理。</span><button className="btn btn-sm" onClick={()=>setCategory("connection")}>管理连接</button></div>}
    <p className="settings-note" hidden={category!=="instances"}>每个 R2 实例连接一个桶；默认将永久存储和临时存储分开，分别用于固定外链与到期清理。</p>
    <div className="storage-category" hidden={category!=="connection"}><h3>Telegram 归档连接</h3><p className="settings-note">管理员统一登录一次，所有 TG 归档实例共用此连接。每个 Pimg 账号自动分配超级群，相册对应话题。</p><TelegramSettings onDirtyChange={setTelegramDirty} onChanged={() => {void load();window.dispatchEvent(new Event("pimg:policy-updated"))}} /><h3 className="storage-connection-heading">共享网络代理</h3><form className="storage-subform" aria-label="存储网络代理" onSubmit={e => {e.preventDefault();void act("proxy",async()=>{await api.patch("/api/admin/storage/connection",{proxy_url:proxy});setProxy(null);},"存储代理已保存，TG 将重新连接");}}><fieldset className="settings-fields" disabled={!!busy}>
      <Field id="storage-proxy" label="存储网络代理" hint="R2 与 TG 共用，留空表示直连；Docker 可填 http://proxy:7890。含密码的代理不回显，修改后才会覆盖。"><input id="storage-proxy" className="input" type="text" autoComplete="off" placeholder={data.connection.has_proxy && !data.connection.proxy_url ? "已保存含凭据的代理" : ""} value={proxy ?? data.connection.proxy_url} onChange={e=>setProxy(e.target.value)} /></Field>
      </fieldset><div className="settings-actions"><button className="btn btn-crayon" disabled={!!busy || proxy === null}>{busy==="proxy"?"保存中…":"保存代理"}</button></div></form>
      <ProxyTrafficSettings initial={data.connection.proxy_traffic} active={category === "connection"} onDirtyChange={setTrafficDirty} />
    </div><div className="storage-category" hidden={category!=="instances"}><div className="storage-inventory" aria-label="已配置存储">
      {data.instances.map(i => <div className="storage-instance-row" key={i.id}><div><strong>{i.name}</strong><p>{data.drivers.find(d => d.id === i.driver)?.name || drivers[i.driver] || i.driver} · {instancePosition(i)}</p><small>{i.configured && i.capabilities.length ? i.capabilities.map(c => ({ archive: "归档", permanent: "发布", temporary: "临时" }[c] || c)).join(" / ") : "待配置"}</small></div><div className="storage-row-actions">{<button type="button" className="btn btn-ghost" disabled={!!busy} onClick={() => {setEditing(i.id);setAdding(true);setName(i.name);setDriver(i.driver);setConfig(i.config);}}>修改配置</button>}<button type="button" className="btn btn-ghost" disabled={!!busy || !i.capabilities.length || !i.configured} onClick={() => void act(i.id, () => api.post(`/api/admin/storage/instances/${i.id}/check?role=${i.capabilities.includes("archive") ? "cold" : "hot"}`), `${i.name}连接正常`)}>测试连接</button></div></div>)}
    </div>
    <button type="button" className="btn" disabled={!!busy} onClick={() => { setAdding(!adding);setEditing("");setName("");setConfig({}); setError(""); }}>{adding ? "取消编辑" : "添加存储实例"}</button>
    {adding && <form className="storage-subform" aria-label={editing ? "修改存储配置" : "添加存储实例"} onSubmit={(e: FormEvent) => { e.preventDefault(); void act("instance", async () => { if(editing)await api.patch(`/api/admin/storage/instances/${editing}`,{ name, driver, config, enabled: true });else await api.post("/api/admin/storage/instances", { name, driver, config, enabled: true }); setAdding(false); setName(""); setConfig({}); window.dispatchEvent(new Event("pimg:policy-updated")); }, editing ? "存储配置已更新" : "存储实例已添加，请选择用途"); }}><fieldset className="settings-fields" disabled={!!busy}>
      <Field id="storage-name" label="实例名称"><input id="storage-name" className="input" required maxLength={80} value={name} onChange={e => setName(e.target.value)} /></Field>
      <Field id="storage-driver" label="存储方式"><select id="storage-driver" className="select" value={driver} disabled={!!editing} onChange={e => { setDriver(e.target.value); setConfig({}); }}>{data.drivers.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}</select></Field>
      {data.drivers.find(d => d.id === driver)?.fields.filter(f => !(config.bootstrap && f.key === "prefix") && !(config.bootstrap && driver === "telegram" && f.key === "topic_id") && !(editing === "r2-temp" && f.key === "public_base")).map(input)}
      {driver === "r2" && <>
        {input({key:"cdn_zone",label:"CDN Zone ID",type:"text",required:false})}
        {input({key:"cdn_token",label:"CDN API Token",type:"password",required:false,hint:"留空保留已保存的令牌。"})}
        {editing !== "r2" && <>
          {input({key:"temp_public_base",label:"临时公开域名",type:"url",required:false,hint:"留空使用签名链接；填写后由 Pimg 到期清理，允许停机延迟。"})}

        </>}
      </>}
      {editing && <p className="settings-hint">密钥留空保留原值。已有数据或用途占用时，改桶、目录、域名或话题请新建实例后切换，旧文件继续使用原位置。</p>}
    </fieldset><div className="settings-actions"><button className="btn btn-crayon" disabled={!!busy}>{busy === "instance" ? "保存中…" : "保存实例"}</button></div></form>}

    </div><div className="storage-category" hidden={category!=="policy"}><form className="storage-subform" aria-label="存储用途与同步策略" onSubmit={e => { e.preventDefault(); void act("policy", async () => { const next = await api.patch<Response>("/api/admin/storage/policy", policy); setData(next); setPolicy(next.policy); window.dispatchEvent(new Event("pimg:policy-updated")); }, "存储用途与同步策略已保存"); }}>
      <fieldset className="settings-fields" disabled={!!busy}>
        <fieldset className="storage-archive-choices"><legend>归档存储（可多选）</legend>{choices("archive").map(i => <label key={i.id}><input type="checkbox" checked={policy.archives.includes(i.id)} disabled={!i.enabled || !i.configured && !policy.archives.includes(i.id)} onChange={e => set("archives", e.target.checked ? [...policy.archives, i.id] : policy.archives.filter(v => v !== i.id))} /><span>{i.name}{!i.configured ? "（未配置 / 未登录）" : ""}</span></label>)}<p className="settings-hint">已选归档按可用性、优先级和速度选择首份，其余自动同步。取消选择会保留已有副本。</p></fieldset>
        <Field id="storage-permanent" label="永久发布" hint="首选失败时按优先级降级，已发布链接通过 Pimg 稳定入口访问。">{instanceSelect("storage-permanent", policy.permanent, "permanent", value => set("permanent", value))}</Field>
        <Field id="storage-temporary" label="临时存储" hint="由 Pimg 到期清理；服务恢复后会补清理，允许延迟失效。">{instanceSelect("storage-temporary", policy.temporary, "temporary", value => set("temporary", value))}</Field>
        <Field id="storage-local" label="本地同步限速" hint="MiB/s；仅限制本地复制，不限制本地校验；不受加速时段影响。"><input id="storage-local" className="input" type="number" min={1} max={1024} required value={policy.local_mbps ?? 64} onChange={e => set("local_mbps", Number(e.target.value))} /></Field>
        <Field id="storage-normal" label="普通同步限速" hint="MiB/s；仅限制 TG、R2 等网络同步。"><input id="storage-normal" className="input" type="number" min={1} max={1024} required value={policy.normal_mbps} onChange={e => set("normal_mbps", Number(e.target.value))} /></Field>
        <Field id="storage-accelerated" label="加速同步限速" hint="MiB/s；加速时段的网络同步上限，前台任务仍优先。"><input id="storage-accelerated" className="input" type="number" min={policy.normal_mbps} max={1024} required value={policy.accelerated_mbps} onChange={e => set("accelerated_mbps", Number(e.target.value))} /></Field>
        <Field id="storage-start" label="加速开始" hint="北京时间；支持跨午夜。"><input id="storage-start" className="input" type="time" required value={policy.accelerated_start} onChange={e => set("accelerated_start", e.target.value)} /></Field>
        <Field id="storage-end" label="加速结束" hint="起止相同表示关闭加速时段。"><input id="storage-end" className="input" type="time" required value={policy.accelerated_end} onChange={e => set("accelerated_end", e.target.value)} /></Field>
        <Field id="storage-cache" label="同步工作空间上限" hint="MiB；每次只落盘一个分片，用后删除。"><input id="storage-cache" className="input" type="number" min={32} max={16384} required value={policy.sync_cache_mb} onChange={e => set("sync_cache_mb", Number(e.target.value))} /></Field>
      </fieldset><div className="settings-actions"><p className="settings-feedback" role="status">{dirty ? "有未保存的更改" : "用途与同步策略已保存"}</p><div className="settings-action-buttons">{dirty && <button type="button" className="btn btn-ghost" disabled={!!busy} onClick={() => setPolicy(data.policy)}>还原更改</button>}<button className="btn btn-crayon" disabled={!!busy || !dirty || !policy.archives.length}>保存存储策略</button></div></div>
    </form>

    </div></div>;
}
