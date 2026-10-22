import { useEffect, useRef, useState } from "react";
import { fmtTime, storageName } from "../api";
import type { FileItem, StorageLocation } from "../api";
import { useToast } from "../App";
import { copyText } from "./interactions";
import { useGallery } from "./GalleryScope";
import { fetchLinks, linkVersion, locationLink, savedLinks } from "./fileLinks";

// One clock per view keeps every visible label current without polling the API.
export function useStorageClock() {
  const [now, setNow] = useState(() => Date.now() / 1000);
  useEffect(() => {
    const update = () => setNow(Date.now() / 1000);
    const timer = setInterval(update, 15000);
    document.addEventListener("visibilitychange", update);
    return () => { clearInterval(timer); document.removeEventListener("visibilitychange", update); };
  }, []);
  return now;
}

export function expiryLabel(expiresAt: number | undefined, now: number, compact = false) {
  if (!expiresAt || !Number.isFinite(expiresAt)) return "过期时间未知";
  const remaining = expiresAt - now;
  if (remaining <= 0) return "已过期";
  if (remaining < 60) return compact ? "<1分钟" : "不到 1 分钟后过期";
  const seconds = remaining >= 86400 ? 86400 : remaining >= 3600 ? 3600 : 60;
  const unit = seconds === 86400 ? "天" : seconds === 3600 ? "小时" : "分钟";
  return compact ? `约${Math.round(remaining / seconds)}${unit}` : `约 ${Math.round(remaining / seconds)} ${unit}后过期`;
}

export function StorageLegend() {
  return <div className="storage-legend" role="group" aria-label="存储标签说明">
    <span className="storage-legend-item storage-legend-archive"><span aria-hidden="true" />归档</span>
    <span className="storage-legend-item storage-legend-published"><span aria-hidden="true" />发布</span>
    <span className="storage-legend-item storage-legend-temporary"><span aria-hidden="true" />临时</span>
  </div>;
}

export function archiveEncryption(file: FileItem) {
	if (file.media_version === 2) return { encrypted: null, label: "加密状态按存储分别记录", failed: false, state: "unknown" };
  const encrypted = typeof file.encrypted === "boolean" ? file.encrypted : file.storage === "direct" ? false : file.storage === "tg" ? true : null;
  const pending = encrypted === true && file.archive_state !== "done";
  const label = encrypted === null ? "加密状态未知" : encrypted === false ? "未加密" : pending ? file.archive_state === "failed" ? "加密待确认" : "待加密" : "已加密";
  return { encrypted, label, failed: pending && file.archive_state === "failed", state: encrypted === null ? "unknown" : encrypted === false ? "plaintext" : pending ? "pending" : "encrypted" };
}

export function EncryptionIcon({ file, location }: { file: FileItem; location?: StorageLocation }) {
  const actual = location && typeof location.encrypted === "boolean";
  const encrypted = actual ? location.encrypted! : archiveEncryption(file).encrypted;
  const pending = actual && location.confirmed === false && !location.saved_parts;
  const label = pending ? "副本尚未就绪" : encrypted === null ? "加密状态未知" : encrypted ? "已加密" : "未加密";
  const failed = location?.state === "failed";
  const state = pending ? "pending" : encrypted === null ? "unknown" : encrypted ? "encrypted" : "plaintext";
  return <span className={`encryption-icon${failed ? " failed" : ""}`} data-encryption-state={state} role="img" aria-label={label} title={label}>
    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{pending || encrypted === null ? <><circle cx="12" cy="12" r="8" /><path d="M12 8v4m0 4h.01" /></> : <><rect x="5" y="10" width="14" height="11" rx="2" />{encrypted === false ? <path d="M9 10V6a4 4 0 0 1 7.5-2" /> : <path d="M8 10V6a4 4 0 0 1 8 0v4" />}</>}</svg>
  </span>;
}

function locationsFor(file: FileItem): StorageLocation[] {
  if (file.storage_locations) return file.storage_locations;
  // Older servers expose only the two permanent locations. Never guess a temp
  // location from the configured publishing provider or the preview URL.
  const locations: StorageLocation[] = [];
  if (file.archive_state === "done" && file.cold_storage) locations.push({ role: "archive", backend: file.cold_storage });
  if (file.published && file.hot_storage) locations.push({ role: "published", backend: file.hot_storage });
  return locations;
}

export default function StorageLabels({ file, now }: { file: FileItem; now: number }) {
  const {api, admin} = useGallery();
  const toast = useToast(), [copying, setCopying] = useState("");
  const pending = useRef(false), current = useRef(""), mounted = useRef(true);
  const version = linkVersion(file); current.current = version;
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  const copy = async (location: StorageLocation, key: string) => {
    if (pending.current) return;
    pending.current = true; setCopying(key);
    try {
      let link = locationLink(savedLinks(file)?.items || [], location);
      if (!link) link = locationLink((await fetchLinks(file, undefined, false, api)).items, location);
      if (!mounted.current || current.current !== version) return;
      if (!link) { toast("当前没有可复制的外链，请在详情中刷新链接。", "err"); return; }
      await copyText(link.url);
      toast(location.role === "archive" ? "已复制归档数据链接（需 API 令牌）" : "已复制链接");
    } catch (cause: any) { if (mounted.current && current.current === version) toast(cause.message || "链接复制失败，请在详情中重试。", "err"); }
    finally { pending.current = false; if (mounted.current) setCopying(""); }
  };
  if (file.purge_state) return <span className="storage-labels" role="group" aria-label="存储位置"><span className="storage-link-labels" tabIndex={0} role="group" aria-label="存储状态"><span className={`chip storage-label${file.purge_state === "failed" ? " chip-err" : " chip-warn"}`}>{file.purge_state === "failed" ? "存储清理未完成" : "正在清理存储"}</span></span></span>;
  const locations = locationsFor(file);
  return <span className="storage-labels" role="group" aria-label="存储位置">
    <span className="storage-link-labels" tabIndex={0} role="group" aria-label="存储链接">
    {locations.length ? locations.map((location, index) => {
      const temporary = location.role === "temporary";
      const role = location.role === "archive" ? "归档" : location.role === "published" ? "发布" : location.kind === "thumb" ? "临时封面" : location.kind === "original" ? file.media_version === 2 ? "临时查看" : "临时原件" : "临时副本";
      const provider = location.name || ({ telegram: "TG", r2: "R2", "r2-temp": "R2", local: "本地" } as Record<string, string>)[location.backend] || location.backend;
      const expired = temporary && !!location.expires_at && location.expires_at <= now;
      const unconfirmed = temporary && location.confirmed === false;
      const label = temporary ? expiryLabel(location.expires_at, now, true) : "";
      const title = `${role}存储：${storageName(location.backend, location.role)}${`；${typeof location.encrypted === "boolean" ? location.confirmed === false ? "副本尚未就绪" : location.encrypted ? "已加密" : "未加密" : archiveEncryption(file).label}`}${temporary ? `；${unconfirmed ? "副本是否生成尚待确认；" : ""}${location.expires_at ? `${fmtTime(location.expires_at)} 到期${expired ? "，等待清理" : ""}` : "过期时间未知"}` : ""}`;
      const color = location.role === "archive" ? "chip-ok" : location.role === "published" ? "chip-crayon" : "chip-warn";
      const key = `${location.role}-${location.backend}-${location.kind || ""}-${index}`;
      const canCopy = (admin || location.role !== "archive") && !file.conversion && !unconfirmed && !expired && (!temporary || !!location.expires_at) && (location.role !== "archive" || file.media_version === 2 || location.backend === "telegram");
      const content = <><EncryptionIcon file={file} location={location} /><span className="storage-label-place">{provider}</span>{!!location.part_count && location.part_count > 1 && <span> · {location.saved_parts}/{location.part_count} 段</span>}{(temporary || copying === key) && <span className="storage-label-expiry">· {copying === key ? "复制中…" : `${unconfirmed ? "待确认 · " : ""}${label}`}</span>}</>;
      const hint = `${title}；点击复制${role}链接${location.role === "archive" ? "（需 API 令牌，原样返回存储数据）" : ""}`;
      return canCopy ? <button type="button" key={key} className={`chip storage-label storage-label-copy ${color}`} data-storage-role={location.role} aria-label={hint} title={hint} aria-busy={copying === key} disabled={!!copying} onClick={() => void copy(location, key)}>{content}</button>
        : <span key={key} className={`chip storage-label ${color}`} data-storage-role={location.role} role="group" aria-label={title} title={title}>{content}</span>;
    }) : <span className={`chip storage-label${file.archive_state === "failed" ? " chip-err" : ""}`}>{file.archive_state === "failed" ? "归档未完成" : file.archive_state !== "done" ? "等待归档完成" : "存储位置待确认"}</span>}
    </span>
  </span>;
}
