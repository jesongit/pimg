import { api, galleryApi } from "../api";
import type { AvailableLink, AvailableLinksResponse, FileItem, StorageLocation } from "../api";
import { archiveEncrypted } from "./interactions";

// Keep fetched addresses in this page's memory. A new file snapshot invalidates
// them; signed links are checked against wall time again immediately on copy.
const cache = new Map<string, { version: string; links: AvailableLinksResponse }>();
export function linkVersion(file: FileItem) {
  return JSON.stringify([file.id, file.updated_at, file.archive_state, file.storage, file.encrypted, file.published, file.purge_state, file.conversion, file.cold_storage, file.hot_storage, file.has_thumb, file.has_bthumb, file.storage_locations, file.available_links]);
}

// Never copy bot credentials or authenticated admin preview routes.
export function servingLink(item: AvailableLink, fileID?: string): AvailableLink | null {
  if (!item || typeof item.url !== "string" || !item.url.trim() || !["archive", "published", "temporary"].includes(item.role) || !["original", "thumb", "blur"].includes(item.kind)) return null;
  try {
    const url = new URL(item.url, window.location.origin), path = decodeURIComponent(url.pathname);
    if (!["http:", "https:"].includes(url.protocol) || url.username || url.password || /(^|\/)api\/(?:admin|account)(?:\/|$)/i.test(path) || /\/file\/bot[^/]+/i.test(path) || url.hostname.toLowerCase() === "api.telegram.org") return null;
    if (item.role === "archive" || item.backend === "telegram") {
      const proxy = /^\/media\/tg\/([^/]+)\/(original|thumb|blur)$/.exec(path);
      const credentialKeys = ["token", "access_token", "api_key", "key", "signature"];
      if (item.role !== "archive" || item.backend !== "telegram" || item.access !== "api_token" || item.representation !== "stored" || !proxy || (fileID !== undefined && proxy[1] !== fileID) || proxy[2] !== item.kind || url.hash || [...url.searchParams.keys()].some((key) => credentialKeys.includes(key.toLowerCase()))) return null;
    }
    if (item.role === "temporary" && (!item.expires_at || !Number.isFinite(item.expires_at))) return null;
    return { ...item, url: url.href };
  } catch { return null; }
}

export function live(link: AvailableLink, now = Date.now() / 1000) {
  return (link.expires_at === undefined || Number.isFinite(link.expires_at) && link.expires_at > now)
    && (link.object_expires_at === undefined || Number.isFinite(link.object_expires_at) && link.object_expires_at > now);
}

export function cleanLinks(file: FileItem, response: AvailableLinksResponse): AvailableLinksResponse {
  const source = Array.isArray(response.items) ? response.items : [];
  const items = source.map((item) => servingLink(item, file.id)).filter((item): item is AvailableLink => !!item && (item.role !== "published" || file.published && !archiveEncrypted(file)));
  return { items, warnings: [...(response.warnings || []), ...(items.length < source.length ? ["部分链接无法用于外部访问，已隐藏。"] : [])] };
}

export function savedLinks(file: FileItem) {
  if (file.purge_state || file.conversion) return { items: [], warnings: [] };
  const saved = cache.get(file.id);
  if (saved?.version === linkVersion(file)) return saved.links;
  return file.available_links ? cleanLinks(file, file.available_links) : undefined;
}

export async function fetchLinks(file: FileItem, signal?: AbortSignal, verify = true, client?: ReturnType<typeof galleryApi>) {
  const path = `${client ? "" : "/api/admin"}/files/${encodeURIComponent(file.id)}`;
  const reader = client || api;
  const response = !verify && file.available_links
    ? (await reader.get<FileItem>(path, signal)).available_links || { items: [] }
    : await reader.get<AvailableLinksResponse>(`${path}/links`, signal);
  const links = cleanLinks(file, response);
  if (!signal?.aborted) {
    cache.delete(file.id); cache.set(file.id, { version: linkVersion(file), links });
    if (cache.size > 200) cache.delete(cache.keys().next().value!);
  }
  return links;
}

export function locationLink(items: AvailableLink[], location: StorageLocation) {
  return items.find((link) => live(link) && link.role === location.role && link.backend === location.backend && link.kind === (location.kind || "original") && (location.role !== "temporary" || link.object_expires_at === location.expires_at));
}
