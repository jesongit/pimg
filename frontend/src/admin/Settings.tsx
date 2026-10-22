import { createContext, useCallback, useContext, useEffect, useState } from "react";
import type { FormEvent, InputHTMLAttributes, ReactNode } from "react";
import { Link } from "react-router-dom";
import { api, displaySiteName, fmtSize, storageName } from "../api";
import type { AdminSettings, StatusInfo } from "../api";
import { useToast } from "../App";
import "./settings.css";
import { useUnsavedChanges } from "./interactions";
import Tokens from "./Tokens";
import AdminCredentials from "./AdminCredentials";
import TwoFactor from "./TwoFactor";
import FooterLinksEditor from "./FooterLinksEditor";
import SiteFooter, { githubExample } from "../components/SiteFooter";
import type { FooterLink } from "../api";
import "./settings-workbench.css";

const groups = {
  site: ["public_base_url", "site_name", "site_desc", "logo_url"],
  home: ["home_cover_mode", "home_page_size"],
  footer: ["footer_text", "footer_links", "icp_number", "icp_url", "police_number", "police_url"],
  storage: ["cold_storage", "hot_storage"],
  policy: ["upload_max_mb", "allowed_exts", "original_cache_hours", "retention_hours", "presign_minutes"],
  logs: ["log_retention_days"],
  debug: ["debug_enabled"],
} as const;
type Group = keyof typeof groups;
type Key = (typeof groups)[Group][number];
type Draft = Record<Key, string>;
type Feedback = { kind: "ok" | "err"; text: string };
const numbers = new Set<Key>(["home_page_size", "upload_max_mb", "original_cache_hours", "retention_hours", "presign_minutes", "log_retention_days"]);
const groupNames: Record<Group, string> = { site: "站点信息", home: "首页公开", footer: "页脚设置", storage: "存储方案", policy: "上传与保留策略", logs: "日志保留设置",debug:"调试设置" };
const categories = [["site", "站点信息"], ["home", "首页公开"], ["footer", "页脚与链接"], ["policy", "上传与保留"], ["logs", "日志保留"], ["security", "账号安全"], ["tokens", "访问令牌"], ["debug","调试模式"], ["system", "运行状态"]] as const;
const ActiveSection = createContext("site");

function toDraft(settings: AdminSettings): Draft {
  return Object.fromEntries(Object.values(groups).flat().map((key) => [
    key, key === "footer_links" ? JSON.stringify(settings.footer_links ?? [githubExample]) : key === "site_name" ? displaySiteName(settings[key]) : String(settings[key] ?? (key === "original_cache_hours" ? 24 : key === "log_retention_days" ? 30 : key==="debug_enabled"?false:"")),
  ])) as Draft;
}

const sectionIcons: Record<string, string> = {
  site: "m10 13 4-4M8 15l-2 2a4 4 0 0 1-6-6l5-5a4 4 0 0 1 6 0m2 3 2-2a4 4 0 0 1 6 6l-5 5a4 4 0 0 1-6 0",
  home: "m3 10 9-7 9 7v11h-7v-7h-4v7H3z",
  footer: "M3 5h18v14H3zM3 14h18M7 17h3m4 0h3",
  policy: "M6 18H5a4 4 0 0 1-1-8 7 7 0 0 1 14-2 5 5 0 0 1 1 10h-1M12 21V11m-4 4 4-4 4 4",
  logs: "M5 3h14v18H5zM8 7h8M8 11h8M8 15h5",
  storage: "M3 3h18v7H3zM3 14h18v7H3zM7 6.5h.01M7 17.5h.01",
  security: "M12 3 3 7v5c0 5 9 10 9 10s9-5 9-10V7zM9 12l2 2 4-4",
  system: "M3 4h18v13H3zM8 21h8m-4-4v4M6 11h3l2-4 3 7 2-3h2",
  tokens: "m3 11 18-7-5 17-5-6-4 3 1-6 9-6-7 8",
  debug:"M9 3h6m-3 0v4M5 8l-2-2m16 2 2-2M3 14h4m10 0h4M5 20l3-3m11 3-3-3M7 10h10v6a5 5 0 0 1-10 0v-6Z",
  replicas: "M3 3h12v7H3zM9 14h12v7H9zM9 10v7H6m9-10h3v7",
};
const settingsIcon = (path: string) => <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={path} /></svg>;

function Section({ id, title, description, children }: { id: string; title: string; description: string; children: ReactNode }) {
  const active = useContext(ActiveSection);
  return <section id={`settings-${id}`} className="settings-section" aria-labelledby={`heading-${id}`} hidden={active !== id}>
    <div className="settings-section-heading"><h2 id={`heading-${id}`}>{settingsIcon(sectionIcons[id])}{title}</h2><p>{description}</p></div>
    <div className="settings-section-body">{children}</div>
  </section>;
}

function Field({ id, label, hint, children }: { id: string; label: string; hint?: ReactNode; children: ReactNode }) {
  return <div className="settings-field"><label htmlFor={id}>{label}</label><div className="settings-field-control">{children}</div>
    {hint && <p className="settings-hint" id={`${id}-hint`}>{hint}</p>}
  </div>;
}

export default function Settings({ username }: { username: string }) {
  const toast = useToast();
  const [active, setActive] = useState(() => { const raw = window.location.hash.replace("#settings-", ""), section = raw === "telegram" ? "storage" : raw; return categories.some(([id]) => id === section) ? section : "site"; });
  const switchSection = (id: string) => { setActive(id); window.history.replaceState(window.history.state, "", `#settings-${id}`); window.scrollTo({ top: 0, behavior: "instant" }); };
  useEffect(() => { const sync = () => { const raw = window.location.hash.replace("#settings-", ""), section = raw === "telegram" ? "storage" : raw; setActive(categories.some(([id]) => id === section) ? section : "site"); }; window.addEventListener("hashchange", sync); return () => window.removeEventListener("hashchange", sync); }, []);
  const initial=api.cached<AdminSettings>("/api/admin/settings"),initialStatus=api.cached<StatusInfo>("/api/admin/status");
  const [draft, setDraft] = useState<Draft | null>(()=>initial?toDraft(initial):null);
  const [saved, setSaved] = useState<Draft | null>(()=>initial?toDraft(initial):null);
  
  const [loadError, setLoadError] = useState("");
  const [debugError,setDebugError]=useState("");
  const [formatDefaults, setFormatDefaults] = useState<string[]>(initial?.default_allowed_exts||[]);
  const [loading, setLoading] = useState(!initial);
  const [status, setStatus] = useState<StatusInfo | null>(initialStatus||null);
  const [statusLoading, setStatusLoading] = useState(!initialStatus);
  const [statusError, setStatusError] = useState("");
  const [saving, setSaving] = useState<Group | null>(null);
  const [feedback, setFeedback] = useState<Partial<Record<Group, Feedback>>>({});
  const [credentialsDirty, setCredentialsDirty] = useState(false);
  const [repositoriesDirty, setRepositoriesDirty] = useState(false);

  const load = useCallback(async () => {
    setLoading(!api.cached("/api/admin/settings")); setLoadError("");
    try {
      const response = await api.get<AdminSettings>("/api/admin/settings");
      const next = toDraft(response);
      setDebugError(response.debug_state?.error || "");
      setFormatDefaults(response.default_allowed_exts || []);
      setDraft(previous=>initial&&previous&&JSON.stringify(previous)!==JSON.stringify(toDraft(initial))?previous:next); setSaved(next); setFeedback({});
    } catch (e: any) { setLoadError(e.message || "暂时无法读取设置，请重试。"); }
    finally { setLoading(false); }
  }, []);
  const refreshStatus = useCallback(async () => {
    setStatusLoading(!api.cached("/api/admin/status")); setStatusError("");
    try { setStatus(await api.get<StatusInfo>("/api/admin/status")); }
    catch { setStatusError("暂时无法检查服务状态，请稍后重试。"); }
    finally { setStatusLoading(false); }
  }, []);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => { if(active==="system")void refreshStatus(); }, [active,refreshStatus]);
  useEffect(() => {
    const refresh = () => { void refreshStatus(); };
    window.addEventListener("pimg:policy-updated", refresh);
    return () => window.removeEventListener("pimg:policy-updated", refresh);
  }, [refreshStatus]);

  const dirty = (group: Group) => !!draft && !!saved && groups[group].some((key) => draft[key] !== saved[key]);
  const changed = (Object.keys(groups) as Group[]).filter(dirty).length;
  useUnsavedChanges(!!changed || credentialsDirty || repositoriesDirty);

  const update = (key: Key, value: string) => {
    setDraft((previous) => previous && ({ ...previous, [key]: value }));
    const group = (Object.keys(groups) as Group[]).find((name) => (groups[name] as readonly Key[]).includes(key));
    if (group) setFeedback((previous) => ({ ...previous, [group]: undefined }));
  };
  const reset = (group: Group) => {
    if (!saved) return;
    setDraft((previous) => previous && ({ ...previous, ...Object.fromEntries(groups[group].map((key) => [key, saved[key]])) }));
    setFeedback((previous) => ({ ...previous, [group]: undefined }));
  };
  const save = async (group: Group, event: FormEvent) => {
    event.preventDefault();
    if (!draft || saving || !dirty(group)) return;
    const patch = Object.fromEntries(groups[group].map((key) => [key, key === "footer_links" ? JSON.parse(draft[key]) : key==="debug_enabled"?draft[key]==="true":numbers.has(key) ? Number(draft[key]) : draft[key]]));
    setSaving(group); setFeedback((previous) => ({ ...previous, [group]: undefined }));
    try {
      const settings=await api.patch<AdminSettings>("/api/admin/settings",patch);
      const response = toDraft(settings);
      if(group==="debug")setDebugError(settings.debug_state?.error||"");
      const confirmed = Object.fromEntries(groups[group].map((key) => [key, response[key]]));
      setSaved((previous) => previous && ({ ...previous, ...confirmed }));
      setDraft((previous) => previous && ({ ...previous, ...confirmed }));
      setFeedback((previous) => ({ ...previous, [group]: { kind: "ok", text: "已保存" } }));
      toast(`${groupNames[group]}已保存`);
      if (group === "site" || group === "home" || group === "footer") window.dispatchEvent(new Event("pimg:site-updated"));
      if (group === "policy" || group === "storage") { refreshStatus(); window.dispatchEvent(new Event("pimg:policy-updated")); }
    } catch (e: any) {
      const text = e.message || "保存失败，输入已保留，请重试。";
      setFeedback((previous) => ({ ...previous, [group]: { kind: "err", text } })); toast(text, "err");
    } finally { setSaving(null); }
  };
  const actions = (group: Group) => <div className={`settings-actions${dirty(group) ? " is-dirty" : ""}${feedback[group] ? " has-feedback" : ""}`}>
    <div className={`settings-feedback${feedback[group] ? ` is-${feedback[group]!.kind}` : ""}`} role={feedback[group]?.kind === "err" ? "alert" : "status"}>
      {feedback[group]?.text || (dirty(group) ? "有未保存的更改" : "修改后单独保存此分区")}
    </div>
    <div className="settings-action-buttons">
      {dirty(group) && <button type="button" className="btn btn-ghost" disabled={!!saving} onClick={() => reset(group)}>还原更改</button>}
      <button className="btn btn-crayon" disabled={!!saving || !dirty(group)}>{settingsIcon("M4 3h13l4 4v14H3V3zM7 3v6h10V3M7 21v-8h10v8")}{saving === group ? "保存中…" : `保存${group === "site" ? "站点信息" : group === "home" ? "首页设置" : group === "footer" ? "页脚设置" : group === "storage" ? "存储方案" : group === "logs" ? "日志设置" : group==="debug"?"调试设置":"策略"}`}</button>
      {(group === "home" || group === "footer") && <Link className="btn settings-home-link" to="/">{settingsIcon("M10 5H4v15h15v-6M14 3h7v7m0-7L10 14")}查看首页</Link>}
    </div>
  </div>;
  const textInput = (key: Key, id: string, extra: InputHTMLAttributes<HTMLInputElement> = {}) =>
    <input id={id} className="input" value={draft?.[key] || ""} maxLength={4096} onChange={(event) => update(key, event.target.value)} {...extra} />;
  const numeric = (key: Key, id: string, unit: string, min: number, max?: number) => <div className="settings-unit">
    {textInput(key, id, { type: "number", min, max, step: 1, required: true, "aria-describedby": `${id}-hint` })}<span aria-hidden="true">{unit}</span>
  </div>;

  const sectionDirty = (id: string) => id === "storage" ? repositoriesDirty : id === "security" ? credentialsDirty : id in groups ? dirty(id as Group) : false;
  return <div className="settings-page settings-workbench">
    <h1 className="sr-only">设置</h1>
    <nav className="settings-index" aria-label="设置分区">{categories.map(([id, label]) => <button type="button" key={id} aria-current={active === id ? "page" : undefined} onClick={event => { switchSection(id); event.currentTarget.scrollIntoView({ block: "nearest", inline: "nearest" }); }}>{settingsIcon(sectionIcons[id])}<span>{label}</span>{sectionDirty(id) && <i className="settings-unsaved-dot" aria-label="有未保存的更改" />}</button>)}{!!changed && <p role="status">{changed} 个分类尚未保存</p>}</nav>
    <div className="settings-content"><ActiveSection.Provider value={active}>
    {loading ? <div className="settings-loading" role="status" aria-label="正在读取设置"><div /><div /><div /><span>正在读取设置…</span></div>
      : loadError ? <div className="settings-load-error" role="alert"><h2>暂时无法读取设置</h2><p>{loadError}</p><button className="btn" onClick={load}>重新加载</button></div>
      : draft && <>
      <Section id="site" title="站点信息" description="设置网站访问地址、名称、简介和标识。">
        <form onSubmit={(event) => save("site", event)} aria-label="站点标识设置">
          <fieldset className="settings-fields" disabled={saving === "site"}>
            <Field id="public-base-url" label="公网地址" hint="用于复制文件链接，例如 https://img.example.com。留空生成相对链接；保存后立即生效。">{textInput("public_base_url", "public-base-url", { type: "url", inputMode: "url", placeholder: "https://img.example.com", "aria-describedby": "public-base-url-hint" })}</Field>
            <Field id="site-name" label="站点名称" hint="显示在浏览器标题与站点导航中，留空使用 PIMG。">{textInput("site_name", "site-name", { placeholder: "PIMG", "aria-describedby": "site-name-hint" })}</Field>
            <Field id="site-desc" label="站点描述" hint="用一句话介绍你的图床。"><textarea id="site-desc" className="input" rows={1} maxLength={4096} value={draft.site_desc} onChange={(event) => update("site_desc", event.target.value)} aria-describedby="site-desc-hint" /></Field>
            <Field id="site-logo" label="Logo 地址" hint="可选，填写图片链接。">{textInput("logo_url", "site-logo", { inputMode: "url", placeholder: "https://…", "aria-describedby": "site-logo-hint" })}</Field>
          </fieldset>{actions("site")}
        </form>
      </Section>
      <Section id="home" title="首页公开" description="选择公开作品的封面方式与每页数量。">
        <form onSubmit={(event) => save("home", event)} aria-label="首页与页脚设置">
          <fieldset className="settings-fields" disabled={saving === "home"}>
            <fieldset className="settings-cover-options"><legend>首页封面</legend><div>
              {([["normal", "普通封面", "展示已公开作品的缩略图"], ["placeholder", "隐私占位", "使用统一占位图，隐藏缩略图"]] as const).map(([value, label, hint]) => <label key={value} className="settings-cover-option" data-selected={draft.home_cover_mode === value}>
                <input type="radio" name="home-cover-mode" value={value} checked={draft.home_cover_mode === value} onChange={() => update("home_cover_mode", value)} /><span><strong>{label}</strong><small>{hint}</small></span>
              </label>)}
            </div></fieldset>
            <Field id="home-page-size" label="每页作品数" hint="6–96 帧">{numeric("home_page_size", "home-page-size", "帧", 6, 96)}</Field>
          </fieldset>{actions("home")}
        </form>
      </Section>
      <Section id="footer" title="页脚与链接" description="网站名称、自定义文字与备案信息排在左侧，图标链接排在右侧。">
        <form onSubmit={event => save("footer", event)} aria-label="页脚与链接设置">
          <fieldset className="settings-fields" disabled={saving === "footer"}>
            <Field id="footer-text" label="页脚文字" hint="显示在网站名称后、备案信息前；留空只显示网站名。"><textarea id="footer-text" className="input" rows={1} maxLength={4096} placeholder="可选，例如：记录生活中的光" value={draft.footer_text} onChange={(event) => update("footer_text", event.target.value)} aria-describedby="footer-text-hint" /></Field>
            <fieldset className="settings-registration"><legend>备案信息 <span>选填，显示在首页页脚</span></legend><div>
              <Field id="icp-number" label="ICP 备案号">{textInput("icp_number", "icp-number", { placeholder: "京ICP备00000000号" })}</Field>
              <Field id="icp-url" label="ICP 备案链接">{textInput("icp_url", "icp-url", { inputMode: "url", placeholder: "https://beian.miit.gov.cn" })}</Field>
              <Field id="police-number" label="公安备案号">{textInput("police_number", "police-number", { placeholder: "选填" })}</Field>
              <Field id="police-url" label="公安备案链接">{textInput("police_url", "police-url", { inputMode: "url", placeholder: "https://beian.mps.gov.cn" })}</Field>
            </div></fieldset>
            <FooterLinksEditor links={JSON.parse(draft.footer_links) as FooterLink[]} onChange={links => update("footer_links", JSON.stringify(links))} />
            <div className="footer-settings-preview"><span>页脚预览</span><SiteFooter preview site={{ ...draft, footer_links: JSON.parse(draft.footer_links) as FooterLink[] }} /></div>
          </fieldset>{actions("footer")}
        </form>
      </Section>
      <Section id="storage" title="存储与同步" description="配置存储实例、用途与来源整理规则。">
        <p><Link to="/admin/storage">打开存储管理</Link></p>
      </Section>
      <Section id="policy" title="上传与保留" description="控制上传范围，以及临时查看副本的生命周期。">
        <form onSubmit={(event) => save("policy", event)} aria-label="上传与保留设置">
          <fieldset className="settings-fields" disabled={saving === "policy"}>
            <div className="settings-columns">
              <Field id="upload-max" label="单文件大小上限" hint="0 表示不限制大小。上传和导入统一使用此设置；分片大小在存储页面设置。">{numeric("upload_max_mb", "upload-max", "MB", 0, 65536)}</Field>
              <Field id="allowed-exts" label="允许的文件扩展名" hint={<>用逗号分隔，留空允许全部支持格式。{!!formatDefaults.length && <>支持：{formatDefaults.join("、")}。</>}</>}>{textInput("allowed_exts", "allowed-exts", { placeholder: formatDefaults.join(","), "aria-describedby": "allowed-exts-hint", spellCheck: false })}</Field>
              <Field id="retention" label="临时副本保留时间" hint="默认 24 小时，到期后按系统计划清理。">{numeric("retention_hours", "retention", "小时", 1)}</Field>
              <Field id="presign" label="临时链接有效期" hint="默认 30 分钟，不超过副本的剩余保留时间。">{numeric("presign_minutes", "presign", "分钟", 1, 1440)}</Field>
              <Field id="original-cache" label="兼容查看缓存" hint="默认 24 小时（1 天），从加载完成起计时。到期仅清理解码与兼容查看缓存；正式本地副本和预览图保留。">{numeric("original_cache_hours", "original-cache", "小时", 1, 8760)}</Field>
            </div>
            <p className="settings-note">临时副本到期后由清理任务删除。链接有效期不会超过副本剩余保留时间；使用 R2 时可另设生命周期规则兜底。</p>
          </fieldset>{actions("policy")}
        </form>
      </Section>
      <Section id="logs" title="日志保留" description="定期清理过期历史，保留当前任务和恢复所需记录。">
        <form onSubmit={event=>save("logs",event)} aria-label="日志保留设置">
          <fieldset className="settings-fields" disabled={saving==="logs"}>
            <Field id="log-retention" label="日志保留时间" hint="默认 30 天，可设置 1～3650 天。按系统计划清理，默认每天 06:00（北京时间），可在任务页调整。等待、运行和恢复所需记录保留，普通文件失败仍可恢复。">{numeric("log_retention_days","log-retention","天",1,3650)}</Field>
          </fieldset>{actions("logs")}
        </form>
      </Section>
      <Section id="debug" title="调试模式" description="按需采集性能数据，帮助排查页面卡顿与任务变慢。">
        <form onSubmit={event=>save("debug",event)} aria-label="调试设置">
          <fieldset className="settings-fields" disabled={saving==="debug"}>
            <Field id="debug-enabled" label="调试模式" hint="默认关闭。开启并保存后开始记录接口耗时、数据库等待、任务执行和资源趋势，保存最近 7 天的数据。">
              <div className="settings-debug-toggle"><input id="debug-enabled" type="checkbox" role="switch" checked={draft.debug_enabled==="true"} onChange={event=>update("debug_enabled",String(event.target.checked))} aria-describedby="debug-enabled-hint"/><span>{draft.debug_enabled==="true"?"开启性能采集":"关闭性能采集"}</span></div>
            </Field>
          </fieldset>
          <p className="settings-note">关闭并保存后，自动删除全部详细性能采集数据。普通运行日志、任务历史、文件和配置会保留。</p>
          {debugError&&<p className="settings-feedback is-err" role="alert">{debugError}<button type="button" className="btn btn-ghost" disabled={!!saving} onClick={async()=>{if(!saved)return;setSaving("debug");try{const response=await api.patch<AdminSettings>("/api/admin/settings",{debug_enabled:saved.debug_enabled==="true"});setDebugError(response.debug_state?.error||"");}catch(cause:any){setDebugError(cause.message)}finally{setSaving(null)}}}>重新应用</button></p>}
          {actions("debug")}
        </form>
      </Section>
      <Section id="security" title="账号安全" description="修改管理员用户名或密码，保存时需要验证当前密码。">
        <AdminCredentials username={username} onDirtyChange={setCredentialsDirty} />
        <TwoFactor />
      </Section>
      <Section id="tokens" title="访问令牌" description="管理上传器与 API 使用的访问凭据。">{active === "tokens" && <Tokens />}</Section>
      <Section id="system" title="系统状态" description="检查存储连接与媒体处理能力，查看当前存档概况。">
        <div className="settings-system-head"><span className="settings-hint">{status ? `当前版本 v${status.version}` : "服务连接检查"}</span><button className="btn btn-ghost" onClick={refreshStatus} disabled={statusLoading}>{statusLoading ? "检查中…" : "刷新状态"}</button></div>
        {statusError && <p className="settings-feedback is-err" role="alert">{statusError}{status && "以下为上次检查结果。"}</p>}
        <div className="settings-services" aria-busy={statusLoading}>
          {[...(status?.storage_checks || []).map(item => [`${item.purpose} · ${item.name || storageName(item.instance_id)}`, "当前所选实例的连接检查", item.status === "ok" ? "ok" : "err"]), ...(!status ? [["存储连接", "正在检查各归档、永久发布与临时存储", "unknown"]] : []), ["ffmpeg", "视频封面生成", status ? status.ffmpeg ? "ok" : "warn" : "unknown"]].map(([name, description, state]) => <div className="settings-service" key={name}>
            <div><strong>{name}</strong><span>{description}</span></div><span className={`settings-service-state is-${state}`}><i aria-hidden="true" />{state === "unknown" ? statusError ? "未能检查" : "检查中" : name === "ffmpeg" ? state === "ok" ? "可用" : "未安装" : state === "ok" ? "连接正常" : "连接异常"}</span>
          </div>)}
        </div>
        {status && <dl className="settings-archive">
          {[["总作品数", `${status.stats.total} 帧`], ["存档体积", fmtSize(status.stats.total_size)], ["已归档", `${status.stats.archived} 帧`], ["已发布", `${status.stats.published} 帧`], ["首页公开", `${status.stats.show_home} 帧`], ["临时副本", `${status.stats.temp_live} 个`], ["失败任务", `${status.stats.tasks_failed} 个`]].map(([label, value]) => <div key={label}><dt>{label}</dt><dd className="num">{value}</dd></div>)}
        </dl>}
      </Section>
    </>}
  </ActiveSection.Provider></div></div>;
}
