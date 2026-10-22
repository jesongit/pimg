import { useCallback, useEffect, useRef, useState } from "react";
import { fmtTime, storageName } from "../api";
import type { AvailableLink, FileItem, TempView } from "../api";
import { useToast } from "../App";
import { copyText, fileLink, operationBusy } from "./interactions";
import type { LinkFormat } from "./interactions";
import { expiryLabel, useStorageClock } from "./StorageLabels";
import { fetchLinks, linkVersion, live, savedLinks, servingLink } from "./fileLinks";

import { useGallery } from "./GalleryScope";

interface Props { file: FileItem; onChanged?: () => void; }

function linkName(link: AvailableLink) {
  const kind = link.kind === "original" ? "原件" : link.kind === "thumb" ? "封面" : "模糊封面";
  if (link.role === "archive") return `TG直链 · ${kind}`;
  return `${link.role === "published" ? "发布" : "临时"} · ${storageName(link.backend, link.role)} · ${kind}`;
}

function escapeHTML(value: string) {
  return value.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function formattedLink(file: FileItem, link: AvailableLink, format: LinkFormat, includeAccessHint: boolean) {
  if (link.role !== "archive") return fileLink({ ...file, url: link.url, kind: link.kind === "original" ? file.kind : "image" }, format);
  const name = `${file.name} · ${link.kind === "original" ? "原件" : link.kind === "thumb" ? "封面" : "模糊封面"}原始数据（需 API 令牌）`;
  if (format === "md") {
    const label = name.replace(/[\r\n]/g, " ").replace(/([\\\[\]])/g, "\\$1");
    const result = `[${label}](${link.url.replace(/\(/g, "%28").replace(/\)/g, "%29")})`;
    return result + (includeAccessHint ? "\n需请求头：`Authorization: Bearer <API令牌>`；原样返回 TG 存储数据。" : "");
  }
  if (format === "html") {
    const result = `<a href="${escapeHTML(link.url)}" download>${escapeHTML(name)}</a>`;
    return result + (includeAccessHint ? "\n<p>需请求头：<code>Authorization: Bearer &lt;API令牌&gt;</code>；原样返回 TG 存储数据。</p>" : "");
  }
  return link.url + (includeAccessHint ? "\n需请求头：Authorization: Bearer <API令牌>；原样返回 TG 存储数据。" : "");
}

function useAvailableLinks(file: FileItem, onChanged?: () => void) {
  const toast = useToast(), now = useStorageClock();
  const {api, admin} = useGallery();
  const version = linkVersion(file), initial = savedLinks(file);
  const [items, setItems] = useState<AvailableLink[]>(initial?.items || []), [warnings, setWarnings] = useState<string[]>(initial?.warnings || []);
  const [loading, setLoading] = useState(!initial), [error, setError] = useState("");
  const [generating, setGenerating] = useState(false), [generationError, setGenerationError] = useState("");
  const mounted = useRef(false), request = useRef(0), generation = useRef(0), generatingRef = useRef(false);
  const controller = useRef<AbortController>(), pause = useRef<{ timer: ReturnType<typeof setTimeout>; resume: () => void }>();
  const renewal = useRef("");
  const changed = useRef(onChanged); changed.current = onChanged;
  const refresh = useCallback(async (verify = true) => {
    controller.current?.abort();
    const abort = new AbortController(), serial = ++request.current; controller.current = abort;
    setLoading(true); setError("");
    try {
      const response = await fetchLinks(file, abort.signal, verify, api);
      if (!mounted.current || request.current !== serial) return false;
      setItems(response.items); setWarnings(response.warnings || []);
      return response.items;
    } catch (cause: any) {
      if (mounted.current && request.current === serial && cause.name !== "AbortError") setError(cause.message || "暂时无法获取链接，请重试。");
      return false;
    } finally { if (mounted.current && request.current === serial) setLoading(false); }
  }, [version, api]);
  useEffect(() => {
    mounted.current = true; setItems([]); setWarnings([]); setGenerationError(""); setGenerating(false); generatingRef.current = false;
    const saved = savedLinks(file);
    if (saved) { setItems(saved.items); setWarnings(saved.warnings || []); setLoading(false); setError(""); }
    else void refresh();
    return () => {
      mounted.current = false; request.current++; generation.current++; controller.current?.abort();
      if (pause.current) { clearTimeout(pause.current.timer); pause.current.resume(); pause.current = undefined; }
    };
  }, [refresh]);
  useEffect(() => {
    const expired = items.filter((item) => item.expires_at && item.expires_at <= now && item.object_expires_at && item.object_expires_at > now + 30);
    const key = expired.length ? JSON.stringify([version, expired.map((item) => [item.id, item.expires_at])]) : "";
    if (!loading && !error && key && renewal.current !== key) { renewal.current = key; void refresh(false); }
  }, [now, items, loading, error, refresh]);
  const generate = async () => {
    if (generatingRef.current || file.archive_state !== "done" || file.purge_state || operationBusy(file)) return;
    generatingRef.current = true; setGenerating(true); setGenerationError("");
    const serial = ++generation.current, deadline = Date.now() + 45000;
    const active = () => mounted.current && generation.current === serial;
    try {
      while (active()) {
        if (Date.now() >= deadline) throw new Error("临时外链准备时间较长，请稍后重试或查看任务状态。");
        const response = await api.post<TempView>(`/files/${encodeURIComponent(file.id)}/temp`, { kind: "original", share: true });
        if (!active()) return;
        if (response.status === "ready") {
          const shared = servingLink({ id: "generated", role: "temporary", backend: "temporary", kind: "original", url: response.url || "", expires_at: response.expires_at });
          if (!shared || !live(shared)) throw new Error("未获取到可分享的临时外链，请确认后端已更新后重试。");
          const refreshed = await refresh();
          if (!active()) return;
          changed.current?.();
          if (!refreshed) { toast("临时发布已就绪，链接列表获取失败，请刷新重试。", "err"); return; }
          // Signing may change query parameters between POST /temp and GET /links.
          // Bind the selected source to the refreshed object's ID, not its signature.
          const objectURL = new URL(shared.url);
          const generated = refreshed.find((link) => {
            const currentURL = new URL(link.url);
            return link.role === "temporary" && link.kind === "original" && live(link) && currentURL.origin === objectURL.origin && currentURL.pathname === objectURL.pathname;
          });
          if (!generated) { setGenerationError("临时发布已就绪，列表中暂未找到新链接，请刷新后重试。"); return; }
          toast("临时发布已就绪"); return generated.id;
        }
        if (Date.now() >= deadline) throw new Error("临时外链准备时间较长，请稍后重试或查看任务状态。");
        await new Promise<void>((resolve) => {
          const resume = () => { pause.current = undefined; resolve(); };
          pause.current = { timer: setTimeout(resume, 1500), resume };
        });
      }
    } catch (cause: any) { if (active()) setGenerationError(cause.message || "临时外链生成失败，请重试。"); }
    finally { if (active()) { generatingRef.current = false; setGenerating(false); } }
  };
  const copy = async (links: AvailableLink[], format: LinkFormat = "url", includeAccessHint = false) => {
    const current = links.filter((link) => live(link));
    if (!current.length) { toast("链接已过期，请刷新后重新获取。", "err"); return; }
    try {
      await copyText(current.map((link) => formattedLink(file, link, format, includeAccessHint)).join("\n"));
      toast(`已复制${current.length > 1 ? ` ${current.length} 条` : ""}链接${current.length < links.length ? "，已跳过过期链接" : ""}`);
    } catch (cause: any) { toast(cause.message || "复制失败，请手动复制。", "err"); }
  };
  const available = items.filter((item) => live(item, now) && (admin || item.role !== "archive"));
  return { available, now, loading, error, warnings, generating, generationError, refresh, generate, copy };
}

function Notices({ error, warnings, generationError, onRetry }: { error: string; warnings: string[]; generationError: string; onRetry: () => void }) {
  return <>
    {error && <div className="available-links-warning" role="alert"><p>{error}</p><button type="button" className="btn btn-sm" onClick={onRetry}>重试获取</button></div>}
    {!!warnings.length && <div className="available-links-warning" role="status">{warnings.map((warning, index) => <p key={index}>{warning}</p>)}</div>}
    {generationError && <p className="available-links-warning" role="alert">{generationError}</p>}
  </>;
}

function EmptyLinks({ file }: { file: FileItem }) {
  return <p className="available-links-empty">{file.archive_state !== "done" ? "归档完成后可获取外链。" : "当前没有可用外链。"}</p>;
}

export default function AvailableLinks({ file, onChanged }: Props) {
  const links = useAvailableLinks(file, onChanged), [format, setFormat] = useState<LinkFormat>("url");
  const {readOnly} = useGallery();
  const canGenerate = !readOnly && file.archive_state === "done" && !file.purge_state && !file.conversion;
  return <section className="available-links" aria-label="可用链接">
    <div className="available-links-head"><h3>可用链接</h3><button type="button" className="btn btn-sm" disabled={links.loading || links.generating} onClick={() => void links.refresh()}>刷新链接</button></div>
    <div className="available-links-controls"><select className="select" aria-label="链接复制格式" value={format} onChange={(event) => setFormat(event.target.value as LinkFormat)}><option value="url">直链</option><option value="md">Markdown</option><option value="html">HTML</option></select><button type="button" className="btn" disabled={!links.available.length} onClick={() => void links.copy(links.available, format, true)}>复制全部链接</button></div>
    <Notices {...links} onRetry={() => void links.refresh()} />
    {links.loading && <p className="available-links-empty" role="status">正在获取可用链接…</p>}
    {!links.loading && !links.available.length && <EmptyLinks file={file} />}
    {!!links.available.length && <div className="available-links-list">{links.available.map((link) => <div className="available-link" key={link.id}>
      <div className="available-link-meta"><span className={`chip ${link.role === "archive" ? "chip-ok" : link.role === "published" ? "chip-crayon" : "chip-warn"}`}>{linkName(link)}</span>{link.role === "temporary" && <span title={link.expires_at ? `链接于 ${fmtTime(link.expires_at)} 到期${link.object_expires_at ? `；临时副本于 ${fmtTime(link.object_expires_at)} 到期` : ""}` : "过期时间未知"}>{expiryLabel(link.expires_at, links.now)}</span>}</div>
      <div className="copyline available-link-url"><input className="val" readOnly aria-label={`${linkName(link)}链接`} value={link.url} onFocus={(event) => event.target.select()} /><button type="button" className="btn" onClick={() => void links.copy([link], format)}>复制链接</button></div>
    </div>)}</div>}
    {canGenerate && <div className="available-links-generate"><button type="button" className="btn" disabled={links.loading || links.generating || operationBusy(file)} onClick={() => void links.generate()}>{links.generating ? "正在临时发布…" : "临时发布"}</button></div>}
  </section>;
}

