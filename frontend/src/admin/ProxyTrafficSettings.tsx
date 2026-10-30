import FloatingFeedback from "../FloatingFeedback";
import { useEffect, useState } from "react";
import { api } from "../api";
import { useToast } from "../App";

export type ProxyTraffic = {
  daily_limit_bytes: number; monthly_limit_bytes: number;
  daily_used_bytes: number; monthly_used_bytes: number;
  daily_reset_time: string; monthly_reset_day: number;
  day_start: number; month_start: number; daily_resets_at: number; monthly_resets_at: number;
  blocked: boolean; resume_at?: number; error?: string;
};
type Draft = Partial<Record<"daily_limit_bytes" | "monthly_limit_bytes" | "daily_reset_time" | "monthly_reset_day", string>>;
type Correction = { daily: string; monthly: string; dailyChanged?: boolean; monthlyChanged?: boolean; day_start: number; month_start: number };
const GB = 1_000_000_000;
const amount = (n: number) => `${(n / GB).toLocaleString("zh-CN", { maximumFractionDigits: 3 })} GB`;
const time = (n: number) => new Intl.DateTimeFormat("zh-CN", { timeZone: "Asia/Shanghai", month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false }).format(n * 1000);

export default function ProxyTrafficSettings({ initial, active, onDirtyChange }: { initial: ProxyTraffic; active: boolean; onDirtyChange: (dirty: boolean) => void }) {
  const [value, setValue] = useState(initial);
  const [draft, setDraft] = useState<Draft>({});
  const [correction, setCorrection] = useState<Correction | null>(null);
  const [busy, setBusy] = useState(false), [error, setError] = useState("");
  const toast = useToast();
  const dirty = Object.keys(draft).length > 0 || correction !== null;
  useEffect(() => { onDirtyChange(dirty); }, [dirty, onDirtyChange]);
  useEffect(() => () => onDirtyChange(false), [onDirtyChange]);
  useEffect(() => {
    let active = true;
    const refresh = async () => {
      try {
        const next = await api.get<{ connection: { proxy_traffic: ProxyTraffic } }>("/api/admin/storage");
        if (active) { setValue(next.connection.proxy_traffic); }
      } catch (e: any) { if (active) setError(e.message || "流量读取失败，请稍后重试"); }
    };
    const timer = window.setInterval(() => { if (active && !document.hidden) void refresh(); }, 5000);
    return () => { active = false; window.clearInterval(timer); };
  }, [active]);
  const field = (key: keyof Draft, fallback: string) => draft[key] ?? fallback;
  const set = (key: keyof Draft, next: string) => setDraft(old => ({ ...old, [key]: next }));
  async function save() {
    if (busy) return;
    setBusy(true); setError("");
    try {
      const patch: Record<string, number | string> = {};
      for (const [key, raw] of Object.entries(draft)) {
        if (key === "daily_reset_time") patch[key] = raw;
        else {
          const n = Number(raw);
          if (!raw.trim() || !Number.isFinite(n) || n <= 0 || n > (key === "monthly_reset_day" ? 31 : 1_000_000)) throw new Error("请填写有效的额度与刷新日期");
          patch[key] = key === "monthly_reset_day" ? n : Math.round(n * GB);
        }
      }
      if (correction) {
        for (const [key, raw, changed] of [["daily_used_bytes", correction.daily, correction.dailyChanged], ["monthly_used_bytes", correction.monthly, correction.monthlyChanged]] as const) {
          if (!changed) continue;
          const n = Number(raw);
          if (!raw.trim() || !Number.isFinite(n) || n < 0 || n > 1_000_000) throw new Error("已用流量应为 0–1000000 GB");
          patch[key] = Math.round(n * GB);
        }
        patch.day_start = correction.day_start; patch.month_start = correction.month_start;
      }
      const next = await api.patch<{ connection: { proxy_traffic: ProxyTraffic } }>("/api/admin/storage/connection", { proxy_traffic: patch });
      setValue(next.connection.proxy_traffic); setDraft({}); setCorrection(null);
      toast("代理流量配置已保存");
    } catch (e: any) { setError(e.message || "保存失败，请重试"); }
    finally { setBusy(false); }
  }
  return <form className="storage-subform" aria-label="代理流量限制" onSubmit={e => { e.preventDefault(); void save(); }}>
    <h3 className="storage-connection-heading">代理流量限制</h3>
    <p className="settings-note">R2 与 TG 共用额度，累计实际经过代理的上传与下载。1 GB = 10 亿字节；直连与本地复制不计入。</p>
    <p className="settings-note" role="status">本日 {amount(value.daily_used_bytes)} / {amount(value.daily_limit_bytes)}，下次刷新 {time(value.daily_resets_at)}<br />本月 {amount(value.monthly_used_bytes)} / {amount(value.monthly_limit_bytes)}，下次刷新 {time(value.monthly_resets_at)}</p>
    {value.blocked && <FloatingFeedback className="settings-feedback is-err" role="status">{value.error || `代理流量已达上限，相关任务等待至 ${time(value.resume_at!)} 后自动继续。提高额度或校准已用量也可恢复。`}</FloatingFeedback>}
    <fieldset className="settings-fields" disabled={busy}>
      <div className="settings-field"><label htmlFor="proxy-daily-limit">日流量上限（GB）</label><div className="settings-field-control"><input id="proxy-daily-limit" className="input" type="number" min="0.000000001" max="1000000" step="any" required value={field("daily_limit_bytes", String(value.daily_limit_bytes / GB))} onChange={e => set("daily_limit_bytes", e.target.value)} /></div></div>
      <div className="settings-field"><label htmlFor="proxy-monthly-limit">月流量上限（GB）</label><div className="settings-field-control"><input id="proxy-monthly-limit" className="input" type="number" min="0.000000001" max="1000000" step="any" required value={field("monthly_limit_bytes", String(value.monthly_limit_bytes / GB))} onChange={e => set("monthly_limit_bytes", e.target.value)} /></div></div>
      <div className="settings-field"><label htmlFor="proxy-daily-reset">日额度刷新时间</label><div className="settings-field-control"><input id="proxy-daily-reset" className="input" type="time" required aria-describedby="proxy-daily-reset-hint" value={field("daily_reset_time", value.daily_reset_time)} onChange={e => set("daily_reset_time", e.target.value)} /></div><p className="settings-hint" id="proxy-daily-reset-hint">北京时间，每天到点重新累计。</p></div>
      <div className="settings-field"><label htmlFor="proxy-monthly-reset">月额度刷新日期</label><div className="settings-field-control"><input id="proxy-monthly-reset" className="input" type="number" min="1" max="31" step="1" required aria-describedby="proxy-monthly-reset-hint" value={field("monthly_reset_day", String(value.monthly_reset_day))} onChange={e => set("monthly_reset_day", e.target.value)} /></div><p className="settings-hint" id="proxy-monthly-reset-hint">每月所选日期北京时间 00:00；没有该日期时按月底刷新。修改刷新规则会保留当前已用量。</p></div>
    </fieldset>
    {correction ? <fieldset className="settings-fields" disabled={busy}><legend>校准当前已用流量</legend><p className="settings-hint">保存时覆盖当前周期的已用量，之后继续累加。跨周期后需要重新校准。</p>
      <div className="settings-field"><label htmlFor="proxy-daily-used">本日已用（GB）</label><div className="settings-field-control"><input id="proxy-daily-used" className="input" type="number" min="0" max="1000000" step="any" required value={correction.daily} onChange={e => setCorrection({ ...correction, daily: e.target.value, dailyChanged: true })} /></div></div>
      <div className="settings-field"><label htmlFor="proxy-monthly-used">本月已用（GB）</label><div className="settings-field-control"><input id="proxy-monthly-used" className="input" type="number" min="0" max="1000000" step="any" required value={correction.monthly} onChange={e => setCorrection({ ...correction, monthly: e.target.value, monthlyChanged: true })} /></div></div>
    </fieldset> : <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => setCorrection({ daily: String(value.daily_used_bytes / GB), monthly: String(value.monthly_used_bytes / GB), day_start: value.day_start, month_start: value.month_start })}>校准已用流量</button>}
    {error && <FloatingFeedback className="settings-feedback is-err" role="alert">{error}</FloatingFeedback>}
    <div className={`settings-actions ${dirty ? "is-dirty" : "has-feedback"}`}><p className="settings-feedback" role="status">{dirty ? "有未保存的更改" : "达到任一上限暂停，额度恢复后自动继续"}</p><div className="settings-action-buttons">{dirty && <button type="button" className="btn btn-ghost" disabled={busy} onClick={() => { setDraft({}); setCorrection(null); setError(""); }}>还原更改</button>}<button className="btn btn-crayon" disabled={busy || !dirty}>{busy ? "保存中…" : "保存流量配置"}</button></div></div>
  </form>;
}
