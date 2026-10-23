import type { OriginalView } from "../api";

export type BrowserEntry = {
  path: string; name: string; directory: boolean; size: number;
  ext: string; kind: string; mime: string; version: string;
  created_at: number; modified_at: number; relative_path: string; importable: boolean;
};
export type BrowserLocation = { path: string; name: string };
export type BrowserListing = {
  path: string; parent: string; breadcrumbs: BrowserLocation[];
  items: BrowserEntry[]; total: number; next_offset: number;
  recursive: boolean; skipped_directories: number; snapshot?: string;
  scan_limited?: boolean;
  indexed?: boolean; index_root?: string; index_building?: boolean; index_updating?: boolean; index_error?: string;
};
export const browserBase = "/api/admin/file-browser";

export function browserPathLabel(path: string) {
  if (!path.startsWith("pimg-path://")) return path;
  try {
    const bytes = atob(path.slice("pimg-path://".length).replaceAll("-", "+").replaceAll("_", "/"));
    return new TextDecoder().decode(Uint8Array.from(bytes, value => value.charCodeAt(0)));
  } catch { return path; }
}

export function browserContent(entry: BrowserEntry, compatible = "") {
  return `${browserBase}/content?${new URLSearchParams({ path: entry.path, version: entry.version, compatible })}`;
}

function pause(signal: AbortSignal) {
  return new Promise<void>((resolve, reject) => {
    const aborted = () => { clearTimeout(timer); reject(new DOMException("已取消", "AbortError")); };
    const timer = setTimeout(() => { signal.removeEventListener("abort", aborted); resolve(); }, 1000);
    signal.addEventListener("abort", aborted, { once: true });
    if (signal.aborted) aborted();
  });
}

export async function prepareBrowserMedia(entry: BrowserEntry, compatible: string, retry: boolean, signal: AbortSignal): Promise<OriginalView> {
  signal.throwIfAborted();
  if (entry.kind === "image" && !compatible) {
    return { status: "ready", loaded: entry.size, total: entry.size, percent: 100, source: "filesystem", url: browserContent(entry) };
  }
  for (;;) {
    signal.throwIfAborted();
    const response = await fetch(`${browserBase}/view`, {
      method: "POST", credentials: "same-origin", signal,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path: entry.path, version: entry.version, compatible, retry,
        native_hevc: !compatible && entry.kind === "video" && !!document.createElement("video").canPlayType('video/mp4; codecs="hvc1.1.6.L120.B0"'),
      }),
    });
    if (response.status === 429) { await pause(signal); continue; }
    const result = await response.json();
    if (!response.ok) {
      if (response.status === 401) window.dispatchEvent(new Event("pimg:unauthorized"));
      throw new Error(result.error?.message || (typeof result.error === "string" ? result.error : "文件无法读取，请刷新目录后重试。"));
    }
    return result;
  }
}

export async function compatibleBrowserImage(entry: BrowserEntry, signal: AbortSignal) {
  let retry = true;
  for (;;) {
    const result = await prepareBrowserMedia(entry, "image", retry, signal);
    retry = false;
    if (result.status === "failed") throw new Error(result.error || "图片无法显示");
    if (result.status === "ready" && result.url) return result.url;
    await pause(signal);
  }
}
