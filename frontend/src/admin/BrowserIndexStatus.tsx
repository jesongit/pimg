import { useCallback, useEffect, useState } from "react";
import { api } from "../api";
import type { BrowserIndexStatus } from "../api";
import { IconRefresh } from "@tabler/icons-react";

export default function BrowserIndexStatusPanel() {
  const [status, setStatus] = useState<BrowserIndexStatus | null>(null);
  const [error, setError] = useState(""), [refreshing, setRefreshing] = useState(false);
  const load = useCallback(async (signal?: AbortSignal) => {
    try { const value = await api.get<BrowserIndexStatus>("/api/admin/file-browser/index", signal); if (!signal?.aborted) { setStatus(value); setError(""); } }
    catch (cause: any) { if (!signal?.aborted) setError(cause.message || "暂时无法读取索引状态，请稍后重试。"); }
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    const refresh = () => void load(controller.signal);
    refresh(); const timer = setInterval(refresh, 3000);
    window.addEventListener("pimg:browser-index-updated", refresh);
    return () => { controller.abort(); clearInterval(timer); window.removeEventListener("pimg:browser-index-updated", refresh); };
  }, [load]);
  const refresh = async () => {
    setRefreshing(true); setError("");
    try { await api.post("/api/admin/file-browser/index/refresh", {}); await load(); }
    catch (cause: any) { setError(cause.message); }
    finally { setRefreshing(false); }
  };
  return <div className="settings-file-index" aria-label="已保存的索引状态">
    <div className="settings-system-head"><h3>已保存目录的状态</h3><button type="button" className="btn btn-ghost" disabled={refreshing || !status?.roots.length || status.roots.some(root => root.updating)} onClick={refresh}><IconRefresh size={16} />{refreshing ? "提交中…" : "更新索引"}</button></div>
    {error && <p className="settings-feedback is-err" role="alert">{error}</p>}
    {status?.error && <p className="settings-feedback is-err" role="alert">{status.error}</p>}
    {status?.watch_warning && <p className="settings-note" role="status">{status.watch_warning}</p>}
    {!status ? <p className="settings-hint" role="status">正在读取索引状态…</p> : !status.roots.length ? <p className="settings-hint" role="status">未监听任何目录。填写目录并保存后开始建立索引。</p> : <div className="settings-services">{status.roots.map(root => <div className="settings-service settings-index-root" key={root.path}>
      <div><strong title={root.path}>{root.path}</strong><span>{root.entries.toLocaleString()} 项{root.updated_at ? ` · 更新于 ${new Date(root.updated_at * 1000).toLocaleString()}` : " · 首次建立中"}</span>{root.error && <span className="settings-feedback is-err" role="alert">{root.error} 请检查目录后更新索引。</span>}</div>
      <span className={`settings-service-state is-${root.error ? "err" : root.updating || !root.ready ? "warn" : "ok"}`}>{root.error ? "更新异常" : !root.ready ? "正在建立" : root.updating ? "正在更新" : "已就绪"}</span>
    </div>)}</div>}
  </div>;
}
