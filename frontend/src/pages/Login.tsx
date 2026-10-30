import FloatingFeedback from "../FloatingFeedback";
import { useContext, useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api, displaySiteName } from "../api";
import type { AuthUser, HomeItem } from "../api";
import { SiteCtx } from "../App";
import "./login.css";

export default function Login() {
  const nav = useNavigate(), site = useContext(SiteCtx);
  const [username, setUsername] = useState(""), [password, setPassword] = useState("");
  const [show, setShow] = useState(false), [err, setErr] = useState(""), [busy, setBusy] = useState(false);
  const [cover, setCover] = useState<HomeItem | null>(null);
  const [secondStep, setSecondStep] = useState(false), [recovery, setRecovery] = useState(false), [code, setCode] = useState("");
  const codeInput = useRef<HTMLInputElement>(null);
  useEffect(() => { const c = new AbortController(); api.get<{ items: HomeItem[] }>("/api/home?per_page=1", c.signal).then(r => setCover(r.items[0] || null)).catch(() => {}); return () => c.abort(); }, []);
  useEffect(() => { if (secondStep) codeInput.current?.focus(); }, [secondStep, recovery]);
  const submit = async (e: React.FormEvent) => {
    e.preventDefault(); if (busy) return; setBusy(true); setErr("");
    try {
      const result = await api.post<AuthUser & { two_factor_required?: boolean }>("/api/auth/login", { username, password, code: secondStep ? code : undefined });
      if (result.two_factor_required) { setSecondStep(true); setCode(""); return; }
      setPassword(""); setCode(""); nav(result.role === "admin" ? "/admin" : "/library", { replace: true });
    } catch (e: any) { setErr(e.message || "登录失败，请重试。"); }
    finally { setBusy(false); }
  };
  const back = () => { setSecondStep(false); setRecovery(false); setCode(""); setPassword(""); setErr(""); };
  return <div className="signin">
    <header className="public-header-shell"><div className="public-header page-width"><Link className="public-brand" to="/">{site?.logo_url && <img src={site.logo_url} alt="" />}{displaySiteName(site?.site_name)}</Link><Link className="public-admin" to="/">返回照片首页</Link></div></header>
    <main className={`signin-layout page-width${cover ? " has-cover" : ""}`}>
      {cover && <figure className="signin-photo"><img src={cover.thumb_url} alt={cover.name} onError={() => setCover(null)} /><figcaption>{cover.name}</figcaption></figure>}
      <section className="signin-form-area"><div className="signin-form">
        <h1>{secondStep ? "两步验证" : "欢迎回来"}</h1>
        <p>{secondStep ? recovery ? "输入一个未使用的恢复码，完成登录。" : "输入验证器应用中的 6 位动态码。" : "登录后，继续整理你的照片。"}</p>
        <form onSubmit={submit}>
          <fieldset className="signin-fields" disabled={busy}>
            {!secondStep ? <>
              <div className="field"><label htmlFor="u">用户名</label><input id="u" className="input" autoComplete="username" autoFocus required maxLength={64} value={username} onChange={e => setUsername(e.target.value)} /></div>
              <div className="field"><label htmlFor="p">密码</label><div className="signin-password"><input id="p" className="input" type={show ? "text" : "password"} autoComplete="current-password" required value={password} onChange={e => setPassword(e.target.value)} /><button type="button" aria-label={show ? "隐藏密码" : "显示密码"} aria-pressed={show} onClick={() => setShow(!show)}><svg width="19" height="19" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden="true"><path d="M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12Z" /><circle cx="12" cy="12" r="3" />{show && <path d="m3 3 18 18" />}</svg></button></div></div>
            </> : <>
              <div className="signin-factor-account">{username}</div>
              <div className="field"><label htmlFor="login-code">{recovery ? "一次性恢复码" : "6 位动态码"}</label><input key={String(recovery)} ref={codeInput} id="login-code" className="input" autoComplete="one-time-code" inputMode={recovery ? "text" : "numeric"} maxLength={recovery ? 32 : 6} pattern={recovery ? undefined : "[0-9]{6}"} required value={code} onChange={e => setCode(e.target.value)} aria-describedby="login-code-hint" /><small id="login-code-hint">{recovery ? "使用后，该恢复码立即失效。" : "请确保手机时间准确；已用动态码需要等下一组。"}</small></div>
              <button type="button" className="signin-factor-link" onClick={() => { setRecovery(!recovery); setCode(""); setErr(""); }}>{recovery ? "使用验证器动态码" : "无法使用验证器？使用恢复码"}</button>
            </>}
            {err && <FloatingFeedback className="signin-error" role="alert">{err}</FloatingFeedback>}
            <button className="btn btn-crayon signin-submit" disabled={!username || !password || secondStep && !code.trim()}>{busy ? "正在验证…" : secondStep ? "验证并登录" : "登录"}<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" aria-hidden="true"><path d="M4 12h16m-6-6 6 6-6 6" /></svg></button>
            {secondStep && <button type="button" className="signin-factor-link" onClick={back}>返回账号登录</button>}
          </fieldset>
        </form>
        <Link className="signin-return" to="/">先看看公开作品</Link>
      </div></section>
    </main>
  </div>;
}
