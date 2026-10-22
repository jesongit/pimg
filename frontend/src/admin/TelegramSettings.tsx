import { useCallback, useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { Link } from "react-router-dom";
import { api, fmtSize } from "../api";
import type { TelegramStatus } from "../api";
import ConfirmButton from "./ConfirmButton";

const names: Record<TelegramStatus["state"], string> = { unconfigured: "尚未配置", connecting: "正在连接", phone: "等待登录", code: "等待验证码", password: "等待两步验证", authorized: "已登录", reauth_required: "需要重新登录", error: "连接异常" };

export default function TelegramSettings({ onDirtyChange, onChanged }: { onDirtyChange: (dirty: boolean) => void; onChanged: () => void }) {
  const [status, setStatus] = useState<TelegramStatus | null>(()=>api.cached<TelegramStatus>("/api/admin/telegram")||null), [loadError, setLoadError] = useState("");
  const [phone, setPhone] = useState(""), [code, setCode] = useState(""), [password, setPassword] = useState("");
  const [busy, setBusy] = useState(""), [error, setError] = useState(""), [feedback, setFeedback] = useState("");
  const [authError, setAuthError] = useState("");
  const [apiID,setApiID]=useState(0), [savedApiID,setSavedApiID]=useState(0), [apiHash,setApiHash]=useState("");
  const [showPassword, setShowPassword] = useState(false);
  const mounted = useRef(true), inFlight = useRef(false), serial = useRef(0);
  const accept = useCallback((next: TelegramStatus) => { setStatus(next); }, []);
  const load = useCallback(async () => {
    if (inFlight.current) return;
    inFlight.current = true; const request = serial.current;
    try { const next = await api.get<TelegramStatus>("/api/admin/telegram"); if (mounted.current && serial.current === request) { accept(next); setLoadError(""); } }
    catch (cause: any) { if (mounted.current && serial.current === request) setLoadError(cause.message || "无法读取 TG 状态，请重试。"); }
    finally { inFlight.current = false; }
  }, [accept]);
  // StrictMode replays this effect while the initial request is still shared.
  // Keep that response valid; real unmounts are guarded by mounted.current.
  useEffect(() => { mounted.current = true; void load(); return () => { mounted.current = false; }; }, [load]);
  useEffect(() => { if (status?.state !== "connecting") return; const timer = setInterval(() => void load(), 3000); return () => clearInterval(timer); }, [status?.state, load]);
  useEffect(()=>{void api.get<{connection:{api_id:number}}>("/api/admin/storage").then(v=>{setApiID(v.connection.api_id);setSavedApiID(v.connection.api_id);}).catch(e=>setError(e.message));},[]);
  const dirty = apiID !== savedApiID || !!apiHash || !!phone || !!code || !!password;
  useEffect(() => { onDirtyChange(dirty); }, [dirty, onDirtyChange]);
  useEffect(() => { if (status?.state !== "code") setCode(""); if (status?.state !== "password") { setPassword(""); setShowPassword(false); } }, [status?.state]);
  const action = async (kind: string, body: object, message: string) => {
    if (busy) return;
    setBusy(kind); setError(""); setAuthError(""); setFeedback(""); serial.current++;
    try {
      const next = await api.post<TelegramStatus>(`/api/admin/telegram/${kind}`, body);
      if (!mounted.current) return;
      if(kind === "config" && "api_id" in body) {setSavedApiID(apiID);setApiHash("");}
      accept(next); setFeedback(kind.startsWith("auth/") && next.state === "authorized" ? "TG 账号已登录" : message); onChanged();
      if (kind.startsWith("auth/")) setPhone("");
      if (kind === "auth/restart") { setCode(""); setPassword(""); setShowPassword(false); }
    } catch (cause: any) { if (mounted.current) { const message = cause.message || "操作未完成，请检查输入后重试。"; if (kind.startsWith("auth/")) setAuthError(message); else setError(message); } }
    finally { if (mounted.current) setBusy(""); }
  };
  const retrySpace = async (owner: string) => {
    if (busy) return;
    setBusy(`space:${owner}`); setError(""); setFeedback("");
    try { await api.post(`/api/admin/accounts/${encodeURIComponent(owner)}/storage/retry`, {}); await load(); if (mounted.current) setFeedback("归档空间已安排重试，可刷新状态查看结果。"); }
    catch (cause: any) { if (mounted.current) setError(cause.message || "无法安排重试，请稍后再试。"); }
    finally { if (mounted.current) setBusy(""); }
  };
  const submitCredential = (event: FormEvent, kind: string, value: string) => { event.preventDefault(); if (kind === "code") setCode(""); if (kind === "password") setPassword(""); void action(`auth/${kind}`, { [kind]: value }, kind === "phone" ? "登录请求已提交，请按当前状态继续。" : "验证信息已提交"); };
  const authStep = status && ["phone", "code", "password"].includes(status.state);
  const currentAuthError = authError || (authStep ? status?.error : "");
  return <div className="telegram-settings" aria-label="Telegram 账号设置">
    <div className="telegram-status-line"><span className={`chip ${status?.state === "authorized" ? "chip-ok" : status?.state === "error" || status?.state === "reauth_required" ? "chip-warn" : ""}`} role="status">{status ? names[status.state] : loadError ? "TG 状态读取失败" : "正在读取 TG 状态…"}</span>{status?.name && <span>{status.name}</span>}<button type="button" className="btn btn-ghost" onClick={() => void load()} disabled={!!busy}>刷新 TG 状态</button></div>
    {(loadError || error || !authStep && status?.error) && <p className="settings-feedback is-err" role="alert">{error || loadError || status?.error}</p>}
    {feedback && <p className="settings-feedback is-ok" role="status">{feedback}</p>}
    <details className="telegram-credentials" open={status?.configured === false}><summary>应用凭据</summary><form className="storage-subform" aria-label="Telegram 应用凭据" onSubmit={e=>{e.preventDefault();void action("config",{api_id:apiID,api_hash:apiHash},"应用凭据已保存，正在重新连接；已有登录会话保留。");}}><fieldset className="settings-fields" disabled={!!busy}>
      <div className="settings-field"><label htmlFor="tg-api-id">API ID</label><div className="settings-field-control"><input id="tg-api-id" className="input" type="number" min={1} required value={apiID||""} onChange={e=>setApiID(Number(e.target.value))} /></div></div>
      <div className="settings-field"><label htmlFor="tg-api-hash">API Hash</label><div className="settings-field-control"><input id="tg-api-hash" className="input" type="password" autoComplete="new-password" required={!status?.configured} value={apiHash} onChange={e=>setApiHash(e.target.value)} /></div><p className="settings-hint">保存后不回显；留空保留原值。已有账号、归档群和登录状态保留。</p></div>
      </fieldset><div className="settings-actions"><button className="btn btn-crayon" disabled={!!busy || apiID===savedApiID && !apiHash}>{busy==="config"?"保存中…":"保存应用凭据"}</button></div></form></details>
    {status?.configured && ["error", "reauth_required"].includes(status.state) && <div className="settings-actions"><button type="button" className="btn" disabled={!!busy} onClick={() => void action("reconnect", {}, "已重新发起 TG 连接，请按当前状态继续。")}>{busy === "reconnect" ? "正在重新连接…" : "重新连接"}</button><p className="settings-hint">连接已中断或需重新授权，可重新连接后继续。</p></div>}
    {status && <>
      {status.configured && status.state !== "authorized" && status.state !== "connecting" && <form className="telegram-auth" onSubmit={event => submitCredential(event, status.state === "code" ? "code" : status.state === "password" ? "password" : "phone", status.state === "code" ? code.trim() : status.state === "password" ? password : phone.trim())} aria-label="登录 TG 账号">
        <div className="telegram-auth-fields">
        {status.state === "code" ? <label>登录验证码<input className="input" inputMode="numeric" autoComplete="one-time-code" required value={code} onChange={event => setCode(event.target.value)} aria-describedby={currentAuthError ? "tg-auth-error" : undefined} /></label> : status.state === "password" ? <label>两步验证密码<input className="input" name="telegram-password" type={showPassword ? "text" : "password"} autoComplete="off" required value={password} onChange={event => setPassword(event.target.value)} aria-describedby={currentAuthError ? "tg-password-hint tg-auth-error" : "tg-password-hint"} /></label> : <label>手机号（含国家区号）<input className="input" type="tel" autoComplete="tel" placeholder="+86…" required value={phone} onChange={event => setPhone(event.target.value)} aria-describedby={currentAuthError ? "tg-auth-error" : undefined} /></label>}
        {status.state === "password" && <p className="settings-hint" id="tg-password-hint">{status.password_hint ? `密码提示：${status.password_hint}` : "请输入你在 Telegram 设置的两步验证密码。"}</p>}
        {currentAuthError && <p className="settings-feedback is-err" id="tg-auth-error" role="alert">{currentAuthError}</p>}
        </div><div className="telegram-auth-actions">
        {status.state === "password" && <button type="button" className="btn btn-ghost" disabled={!!busy} aria-pressed={showPassword} onClick={() => setShowPassword(value => !value)}>{showPassword ? "隐藏密码" : "显示密码"}</button>}
        <button className="btn btn-crayon" disabled={!!busy}>{busy.startsWith("auth/") ? status.state === "password" ? "正在验证密码…" : status.state === "code" ? "正在核对验证码…" : "正在发送验证码…" : status.state === "code" ? "提交验证码" : status.state === "password" ? "验证并登录" : "发送登录验证码"}</button>
        {["code", "password"].includes(status.state) && <button type="button" className="btn btn-ghost" disabled={!!busy} onClick={() => void action("auth/restart", {}, "请重新输入手机号开始登录。")}>重新开始登录</button>}
        </div>
      </form>}
      {status.state === "connecting" && <p className="settings-note" role="status">正在连接 Telegram，状态每 3 秒更新。连接失败后可重新连接。</p>}
      {status.state === "authorized" && <div className="telegram-group">
        <h3>账号归档空间</h3>
        <p className="settings-note"><Link to="/admin/users">前往用户管理</Link>，可创建、启停用户并修改 TG 群名。</p>
        <p className="settings-note">管理员统一登录 TG。启用归档后，系统为每个 Pimg 账号自动创建专属超级群，相册对应群内话题；普通用户无需配置 TG。</p>
        {status.storage_enabled === false ? <p className="settings-note">当前未启用 Telegram 归档；已有群和归档关联保留。</p> : (status.archive_spaces || []).map(space => <div className="storage-instance-row" key={space.owner_id}>
          <div style={{overflowWrap:"anywhere"}}><strong>{space.owner_id === "admin:1" ? "管理员" : space.owner_id}</strong><p role="status">{space.state === "ready" ? `归档群 ${space.chat_id} · 已就绪` : ({pending:"等待自动分配",creating:"正在创建归档群",uncertain:"正在核对建群结果",failed:"分配失败，等待重试",blocked:"分配受限，需要管理员处理"}[space.state] || space.state)}</p>{space.last_error && <p className="settings-feedback is-err">{space.last_error}</p>}</div>
          {space.state !== "ready" && <button type="button" className="btn" disabled={!!busy || space.state === "creating"} onClick={() => void retrySpace(space.owner_id)}>{busy === `space:${space.owner_id}` ? "正在安排…" : "重试分配"}</button>}
        </div>)}
        <p className="settings-note">{status.upload_limit_bytes ? `当前账号单文件上限 ${fmtSize(status.upload_limit_bytes)}。` : "普通账号单文件上限 2 GB，Premium 为 4 GB。"}视频单片目标默认 512 MB，可在上传与保留中调整；超限时均衡拆为多个独立视频。不兼容编码自动转为可播放 MP4，再逐段归档。加密归档使用 PNG 载体，最终上传大小计入加密封装。上传还受站点上限限制。</p>
      </div>}
      {(status.state === "authorized" || status.state === "reauth_required" && !!status.account_id) && <div className="telegram-logout"><ConfirmButton type="button" className="btn btn-ghost" disabled={!!busy} confirmLabel="确认退出 TG" busyLabel="退出中…" resetKey={`${status.account_id}:${status.state}`} description="退出后，TG 归档读写需重新登录才可继续，已有群关联保留。" onConfirm={()=>action("logout", {}, "已退出 TG 登录")}>退出 TG 登录</ConfirmButton></div>}
    </>}
  </div>;
}
