import { useEffect, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { api } from "../api";
import { useToast } from "../App";
import { useUnsavedChanges } from "./interactions";

export default function AdminCredentials({ username, setup = false, onDirtyChange }: { username: string; setup?: boolean; onDirtyChange?: (dirty: boolean) => void }) {
  const toast = useToast();
  const [name, setName] = useState(username);
  const [pw, setPw] = useState({ old: "", neu: "", confirm: "" });
  const [show, setShow] = useState(false), [busy, setBusy] = useState(false);
  const [factor, setFactor] = useState<boolean | null>(null), [code, setCode] = useState("");
  const [factorError, setFactorError] = useState(false);
  useEffect(() => { let active=true; const check=()=>{void api.get<{enabled:boolean}>("/api/auth/2fa").then(r=>{if(active){setFactor(r.enabled);setFactorError(false);}}).catch(()=>{if(active){setFactor(null);setFactorError(true);}});}; if(!setup)check();window.addEventListener("pimg:two-factor-updated",check);return()=>{active=false;window.removeEventListener("pimg:two-factor-updated",check);}; },[setup]);
  const [feedback, setFeedback] = useState<{ kind: "ok" | "err"; text: string } | null>(null);
  const dirty = name !== username || !!(pw.old || pw.neu || pw.confirm || code);
  useUnsavedChanges(dirty);
  useEffect(() => { setName(username); }, [username]);
  useEffect(() => { onDirtyChange?.(dirty); return () => onDirtyChange?.(false); }, [dirty, onDirtyChange]);
  const valid = !!name.trim() && !!pw.old && (!setup || pw.neu.length >= 8) && (!pw.neu || pw.neu.length >= 8) && pw.neu === pw.confirm && (name.trim() !== username || !!pw.neu);
  const update = (key: keyof typeof pw, value: string) => { setPw(previous => ({ ...previous, [key]: value })); setFeedback(null); };
  const save = async (event: FormEvent) => {
    event.preventDefault();
    if (busy || !valid) return;
    setBusy(true); setFeedback(null);
    try {
      await api.post("/api/auth/account", { username: name.trim(), old_password: pw.old, new_password: pw.neu, code });
      setCode("");
      setPw({ old: "", neu: "", confirm: "" }); setShow(false);
      setFeedback({ kind: "ok", text: "账号设置已保存" }); toast("账号设置已保存");
      window.dispatchEvent(new Event("pimg:account-updated"));
    } catch (error: any) { setFeedback({ kind: "err", text: error.message || "保存失败，请重试。" }); }
    finally { setBusy(false); }
  };
  const field = (id: string, label: string, input: ReactNode, hint?: string) => <div className="settings-field"><label htmlFor={id}>{label}</label><div className="settings-field-control">{input}</div>{hint && <p className="settings-hint" id={`${id}-hint`}>{hint}</p>}</div>;
  return <form onSubmit={save} aria-label="管理员账号设置">
    <fieldset className="settings-fields" disabled={busy}>
      {field("admin-username", "管理员用户名", <input id="admin-username" className="input" value={name} maxLength={64} autoComplete="username" required onChange={event => { setName(event.target.value); setFeedback(null); }} aria-describedby="admin-username-hint" />, "可保留 admin；修改名称不影响图片、相册和 TG 归档群。")}
      {field("current-password", "当前密码", <input id="current-password" className="input" type={show ? "text" : "password"} value={pw.old} onChange={event => update("old", event.target.value)} autoComplete="current-password" required aria-describedby={setup ? "current-password-hint" : undefined} />, setup ? "首次使用默认密码 admin。" : undefined)}
      <div className="settings-columns">
        {field("new-password", "新密码", <input id="new-password" className="input" type={show ? "text" : "password"} value={pw.neu} minLength={8} maxLength={72} onChange={event => update("neu", event.target.value)} autoComplete="new-password" required={setup} aria-describedby="new-password-hint" />, setup ? "至少 8 位，设置后进入后台。" : "至少 8 位；只改用户名时留空。")}
        {field("confirm-password", "确认新密码", <input id="confirm-password" className="input" type={show ? "text" : "password"} value={pw.confirm} onChange={event => update("confirm", event.target.value)} autoComplete="new-password" required={setup || !!pw.neu} aria-invalid={!!pw.confirm && pw.confirm !== pw.neu} aria-describedby="confirm-password-hint" />, pw.confirm && pw.confirm !== pw.neu ? "两次输入的新密码不一致。" : "再次输入新密码。")}
      </div>
      {!setup && factor !== false && field("credential-code", "动态码或恢复码", <input id="credential-code" className="input" autoComplete="one-time-code" value={code} maxLength={32} required={factor === true} onChange={e=>setCode(e.target.value)} />, factor === true ? "使用未使用的验证码；刚登录后请等待下一组动态码。" : "已开启两步验证时请填写，否则留空。")}
      {factorError && <p className="settings-feedback is-err" role="alert">两步验证状态读取失败。<button type="button" className="btn btn-ghost" onClick={()=>window.dispatchEvent(new Event("pimg:two-factor-updated"))}>重新读取验证状态</button></p>}
      {!setup && <p className="settings-note">修改密码后，其他登录会话和管理员令牌将失效，当前网页保持登录。</p>}
    </fieldset>
    <div className="settings-actions">
      <div className={`settings-feedback${feedback ? ` is-${feedback.kind}` : ""}`} role={feedback?.kind === "err" ? "alert" : "status"}>
        <button type="button" className="settings-password-toggle" aria-pressed={show} onClick={() => setShow(!show)}>{show ? "隐藏密码" : "显示密码"}</button>{feedback && <span>{feedback.text}</span>}
      </div>
      <button className="btn btn-crayon" disabled={busy || !valid}>{busy ? "保存中…" : setup ? "保存并进入后台" : "保存账号设置"}</button>
    </div>
  </form>;
}
