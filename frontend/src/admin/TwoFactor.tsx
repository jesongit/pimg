import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { api } from "../api";
import { useUnsavedChanges } from "./interactions";
import "./two-factor.css";

type Status = { enabled: boolean; recovery_remaining: number };
type Enrollment = { secret: string; qr_image: string; expires_at: number };
type Action = "setup" | "disable" | "recovery";

export default function TwoFactor() {
  const [status, setStatus] = useState<Status | null>(null);
  const [action, setAction] = useState<Action | null>(null);
  const [enrollment, setEnrollment] = useState<Enrollment | null>(null);
  const [password, setPassword] = useState(""), [code, setCode] = useState("");
  const [codes, setCodes] = useState<string[]>([]), [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false), [error, setError] = useState(""), [notice, setNotice] = useState("");
  useUnsavedChanges(!!password || !!enrollment || codes.length > 0 && !saved);
  const load = () => api.get<Status>("/api/auth/2fa").then(setStatus);
  useEffect(() => { let active = true; api.get<Status>("/api/auth/2fa").then(r => { if (active) setStatus(r); }).catch(e => { if (active) setError(e.message); }); return () => { active = false; }; }, []);
  const clear = () => { setAction(null); setEnrollment(null); setPassword(""); setCode(""); setError(""); };
  const start = (next: Action) => { clear(); setNotice(""); setAction(next); };
  const submit = async (event: FormEvent) => {
    event.preventDefault(); if (busy || !action) return;
    setBusy(true); setError("");
    try {
      if (action === "setup" && !enrollment) {
        setEnrollment(await api.post<Enrollment>("/api/auth/2fa/setup", { password }));
      } else {
        const result = await api.post<{ recovery_codes: string[] | null }>(`/api/auth/2fa/${action === "setup" ? "enable" : action}`, { password, code });
        setStatus({ enabled: action !== "disable", recovery_remaining: result.recovery_codes?.length || 0 });
        setCodes(result.recovery_codes || []); setSaved(false);
        setNotice(action === "disable" ? "两步验证已关闭，其他登录会话已退出。" : action === "setup" ? "两步验证已开启，其他登录会话已退出。请保存恢复码。" : "恢复码已更新，旧恢复码和其他登录会话已失效。");
        clear(); window.dispatchEvent(new Event("pimg:two-factor-updated"));
      }
    } catch (e: any) { setError(e.message || "操作失败，请重试。"); }
    finally { setBusy(false); }
  };
  const download = () => {
    const blob = new Blob(["PIMG 两步验证恢复码\n每个恢复码仅可使用一次，请保存在安全的离线位置。\n\n" + codes.join("\n") + "\n"], { type: "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob), link = document.createElement("a"); link.href = url; link.download = "pimg-recovery-codes.txt"; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
  };
  return <section className="two-factor" aria-labelledby="two-factor-title">
    <div className="two-factor-heading"><h2 id="two-factor-title">两步验证（2FA）</h2><span>{status ? status.enabled ? "已开启" : "未开启" : "正在读取…"}</span></div>
    <p>登录时，在密码之外验证动态码。支持 Google Authenticator、Microsoft Authenticator 等验证器应用。</p>
    {status?.enabled && !codes.length && <p>剩余 {status.recovery_remaining} 个恢复码。无法使用验证器时，可用恢复码登录。</p>}
    {notice && <p role="status" className="two-factor-notice">{notice}</p>}
    {error && <p className="two-factor-error" role="alert">{error}</p>}
    {!status && <button className="btn" onClick={() => { setError(""); void load().catch(e => setError(e.message)); }}>重新读取</button>}
    {!!codes.length && <div className="two-factor-recovery">
      <h3>保存一次性恢复码</h3><p>恢复码只显示这一次。请保存在验证器以外的安全位置；每次使用仍需输入账号密码。</p>
      <ul aria-label="一次性恢复码">{codes.map(value => <li key={value}><code>{value}</code></li>)}</ul>
      <button type="button" className="btn" onClick={download}>下载恢复码</button>
      <label className="two-factor-saved"><input type="checkbox" checked={saved} onChange={e => setSaved(e.target.checked)} />我已妥善保存恢复码</label>
      <button type="button" className="btn btn-crayon" disabled={!saved} onClick={() => setCodes([])}>完成</button>
    </div>}
    {status && !action && !codes.length && <div className="two-factor-actions">
      {status.enabled ? <><button type="button" className="btn" onClick={() => start("recovery")}>重新生成恢复码</button><button type="button" className="btn" onClick={() => start("disable")}>关闭两步验证</button></> : <button type="button" className="btn btn-crayon" onClick={() => start("setup")}>开启两步验证</button>}
    </div>}
    {action && <form onSubmit={submit}>
      <fieldset disabled={busy}>
        {enrollment && <div className="two-factor-enrollment"><img src={enrollment.qr_image} width="208" height="208" alt="使用验证器应用扫描此二维码，绑定 PIMG 账号" /><div><h3>使用验证器扫描二维码</h3><p>无法扫码时，在应用中选择手动输入密钥、基于时间。</p><code className="two-factor-secret">{enrollment.secret}</code><p>绑定有效期 10 分钟，确认动态码后才会开启。二维码和密钥请勿分享。</p></div></div>}
        {action === "disable" && <p>关闭后，登录只需账号密码。其他登录会话会退出，恢复码会失效。</p>}
        {action === "recovery" && <p>生成后，所有旧恢复码立即失效，其他登录会话会退出。请准备保存新恢复码。</p>}
        <label htmlFor="factor-password">当前密码</label><input id="factor-password" className="input" type="password" autoComplete="current-password" required value={password} onChange={e => setPassword(e.target.value)} />
        {(enrollment || action !== "setup") && <><label htmlFor="factor-code">{action === "setup" ? "6 位动态码" : "动态码或恢复码"}</label><input id="factor-code" className="input" autoComplete="one-time-code" inputMode={action === "setup" ? "numeric" : "text"} maxLength={action === "setup" ? 6 : 32} required value={code} onChange={e => setCode(e.target.value)} /><p>每个动态码只能使用一次；刚登录后请等待下一组动态码。</p></>}
        <div className="two-factor-actions"><button type="button" className="btn" onClick={clear}>取消</button><button className={`btn ${action === "disable" ? "btn-danger" : "btn-crayon"}`} disabled={!password || (!!enrollment || action !== "setup") && !code.trim()}>{busy ? "正在验证…" : action === "setup" ? enrollment ? "验证并开启" : "获取绑定二维码" : action === "disable" ? "验证并关闭" : "验证并生成恢复码"}</button></div>
      </fieldset>
    </form>}
    <p className="two-factor-scope">两步验证保护网页登录；已签发的 API 令牌继续按原权限使用，请单独保管或撤销。</p>
  </section>;
}
