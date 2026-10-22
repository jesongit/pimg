import { useEffect, useRef, useState } from "react";
import { api, fmtTime } from "../api";
import type { TokenItem } from "../api";
import { useToast } from "../App";
import { copyText, useDialog, useUnsavedChanges } from "./interactions";
import ConfirmButton from "./ConfirmButton";
import "./tokens-revision.css";

export default function Tokens() {
  const toast = useToast();
  const [tokens, setTokens] = useState<TokenItem[]>([]);
  const [creating, setCreating] = useState(false);
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");

  const load = async () => {
    setLoadError("");
    try {
      const res = await api.get<{ items: TokenItem[] }>("/api/admin/tokens");
      setTokens(res.items || []);
    } catch (e: any) { setLoadError(e.message); }
    finally { setLoading(false); }
  };
  useEffect(() => { void load(); }, []);

  return <div className="token-page">
    <h1 className="sr-only">API 令牌</h1>
    <div className="token-page-toolbar"><span>{loading ? "正在读取令牌…" : `${tokens.length} 个令牌`}</span><button className="btn btn-crayon" onClick={() => setCreating(true)}><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>创建令牌</button></div>
    {loadError && <div className="token-load-error" role="alert"><span>{loadError}</span><button className="btn btn-sm" onClick={() => void load()}>重试</button></div>}
    <section className="token-registry" aria-label="令牌列表">
      {!loading && !loadError && tokens.length === 0 ? <div className="empty"><p>还没有令牌</p><span>需要连接上传工具时，创建一个访问令牌。</span><button className="btn" onClick={() => setCreating(true)}>创建令牌</button></div> : tokens.length > 0 && <div className="panel" style={{ overflow: "auto" }}>
        <table className="sheet-table responsive-table">
          <thead><tr><th>名称</th><th>权限</th><th>前缀</th><th>创建</th><th>最后使用</th><th></th></tr></thead>
          <tbody>{tokens.map(t => <tr key={t.id}>
            <td data-label="名称">{t.name}</td>
            <td data-label="权限"><div>{t.scope === "uploader" ? "已停用的旧上传器令牌" : t.scope === "diagnostics" ? "只读诊断" : "管理员"}{t.scope === "uploader" && <p className="detail-hint">话题 {(t.topic_ids || []).join("、") || "未授权"}</p>}</div></td>
            <td data-label="前缀" className="mono">{t.prefix}…</td>
            <td data-label="创建" className="mono">{fmtTime(t.created_at)}</td>
            <td data-label="最后使用" className="mono">{t.last_used_at ? fmtTime(t.last_used_at) : "—"}</td>
            <td data-label="操作"><ConfirmButton className="btn btn-sm btn-danger" disabled={busy} confirmLabel="确认吊销" busyLabel="吊销中…" resetKey={t.id} description={`吊销「${t.name}」后，使用此令牌的 Pimg 接口请求将被拒绝；已有 TG 授权不会失效。`} onConfirm={async () => {
              setBusy(true);
              try { await api.del(`/api/admin/tokens/${t.id}`); toast("已吊销"); await load(); }
              catch (e: any) { toast(e.message, "err"); } finally { setBusy(false); }
            }}>吊销</ConfirmButton></td>
          </tr>)}</tbody>
        </table>
      </div>}
    </section>
    {creating && <TokenCreator onCreated={() => void load()} onClose={() => setCreating(false)} />}
  </div>;
}

function TokenCreator({ onCreated, onClose }: { onCreated: () => void; onClose: () => void }) {
  const toast = useToast();
  const ref = useRef<HTMLDivElement>(null);
  const [name, setName] = useState("");
  const [scope, setScope] = useState<"admin" | "diagnostics">("admin");
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState("");
  const [copied, setCopied] = useState(false);
  const dirty = !created && (!!name.trim() || scope !== "admin");
  useUnsavedChanges(dirty || (!!created && !copied));
  const close = () => {
    if (busy) return;
    if (created && !copied && !confirm("令牌只显示这一次。确认已保存后关闭？")) return;
    if (dirty && !confirm("放弃尚未创建的令牌？")) return;
    onClose();
  };
  useDialog(ref, close);

  return <div className="token-dialog-shade"><div ref={ref} className="token-dialog" role="dialog" aria-modal="true" aria-labelledby="token-dialog-title">
    <header><h2 id="token-dialog-title">{created ? "令牌已创建" : "创建令牌"}</h2><button className="btn btn-ghost token-dialog-close" aria-label="关闭创建令牌" disabled={busy} onClick={close}><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" aria-hidden="true"><path d="m6 6 12 12M18 6 6 18" /></svg></button></header>
    {created ? <section className="token-created" aria-label="新令牌">
      <p className="token-secret-warning">令牌只显示这一次，请立即保存。</p>
      <div className="copyline"><input className="val" aria-label="新创建的令牌" readOnly value={created} onFocus={e => e.target.select()} /><button className="btn" onClick={async () => { try { await copyText(created); setCopied(true); toast("已复制，令牌只显示这一次"); } catch (e: any) { toast(e.message, "err"); } }}>{copied ? "已复制" : "复制"}</button></div>
      <div className="token-dialog-actions">{copied ? <button className="btn btn-crayon" onClick={onClose}>已保存，关闭</button> : <ConfirmButton className="btn btn-crayon" confirmLabel="确认关闭" description="令牌只显示这一次。请先保存，关闭后无法再次查看。" resetKey={created} onConfirm={onClose}>已保存，关闭</ConfirmButton>}</div>
    </section> : <form className="token-dialog-form" onSubmit={async e => {
      e.preventDefault();
      if (busy || !name.trim()) return;
      setBusy(true);
      try {
        const res = await api.post<{ id: string; token: string; scope?: string }>("/api/admin/tokens", { name: name.trim(), scope });
        setCreated(res.token);
        onCreated();
      } catch (e: any) { toast(e.message, "err"); } finally { setBusy(false); }
    }}>
      <label htmlFor="token-name">名称<input id="token-name" className="input" placeholder="用途，如：PicGo" disabled={busy} value={name} onChange={e => setName(e.target.value)} /></label>
      <label htmlFor="token-scope">权限<select id="token-scope" aria-label="权限" className="input select" disabled={busy} value={scope} onChange={e => setScope(e.target.value as "admin" | "diagnostics")}><option value="admin">管理员</option><option value="diagnostics">只读诊断</option></select></label>
      <p className="detail-hint">{scope === "diagnostics" ? "仅可读取诊断接口中的运行日志、任务记录和服务状态，用于远程排障。可随时吊销。" : "用于 PicGo 或 API 调用，拥有管理员权限，包括读取、发布、删除文件和修改设置。"}</p>
      <div className="token-dialog-actions"><button type="button" className="btn" disabled={busy} onClick={close}>取消</button><button className="btn btn-crayon" type="submit" disabled={!name.trim() || busy}>{busy ? "创建中…" : "创建令牌"}</button></div>
    </form>}
  </div></div>;
}
