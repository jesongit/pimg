// API 客户端：统一错误处理与会话。
import {cacheableQuery,clearQueryCache,invalidateQueryRequests,queryEntry,queryEpoch,queryIdentityEpoch,queryKey,refreshQuery,setQuerySession,storeQuery} from "./queryCache";
export {setQuerySession as setApiSession};
export class ApiError extends Error {
  status: number;
  constructor(status: number, msg: string) {
    super(msg);
    this.status = status;
  }
}

async function req<T>(path: string, init?: RequestInit, retry = 0): Promise<T> {
  const method=init?.method||"GET",owner=new Headers(init?.headers).get("X-Pimg-Owner")||"";
  const cacheable=method==="GET"&&cacheableQuery(path);
  const key=queryKey(path,owner),cached=cacheable?queryEntry(key):undefined,epoch=queryEpoch(),identity=queryIdentityEpoch();
  const superseded=()=>{
    if(init?.signal?.aborted||identity!==queryIdentityEpoch())throw new DOMException("Aborted","AbortError");
    if(epoch===queryEpoch())return false;
    if(retry>=3)throw new DOMException("Read superseded by newer changes","AbortError");
    return true;
  };
  const headers=new Headers(init?.headers);
  if(init?.body&&!(init.body instanceof FormData)&&!headers.has("Content-Type"))headers.set("Content-Type","application/json");
  if(cached?.etag)headers.set("If-None-Match",cached.etag);
  const res = await fetch(path, {
    credentials: "same-origin",
    ...init,
    headers,cache:"no-store",
  });
  if(cacheable&&superseded())return req<T>(path,init,retry+1);
  if(res.status===304&&cached){refreshQuery(key);return cached.value as T}
  if(res.status===401)clearQueryCache();
  if (res.status === 401 && !path.includes("/auth/")) {
    window.dispatchEvent(new CustomEvent("pimg:unauthorized"));
  }
  let data: any = null;
  try {
    data = await res.json();
  } catch {
    /* 非 JSON */
  }
  if (!res.ok) {
    throw new ApiError(res.status, data?.error?.message || `请求失败 (${res.status})`);
  }
  if(method!=="GET"){
    // Keep visible snapshots while invalidating late pre-mutation responses.
    if(path.includes("/auth/"))clearQueryCache();
    else invalidateQueryRequests();
    return data as T;
  }
  if(cacheable&&superseded())return req<T>(path,init,retry+1);
  return cacheableQuery(path)?storeQuery(key,data,res.headers?.get("ETag")||undefined,epoch) as T:data as T;
}

export const api = {
  cached:<T>(p:string)=>queryEntry(queryKey(p))?.value as T|undefined,
  get: <T>(p: string, signal?: AbortSignal) => req<T>(p, { signal }),
  post: <T>(p: string, body?: any) =>
    req<T>(p, { method: "POST", body: body !== undefined ? JSON.stringify(body) : undefined }),
  patch: <T>(p: string, body: any) =>
    req<T>(p, { method: "PATCH", body: JSON.stringify(body) }),
  del: <T>(p: string, body?: unknown) => req<T>(p, { method: "DELETE", body: body !== undefined ? JSON.stringify(body) : undefined }),
  upload: <T>(p: string, form: FormData, onProgress?: (n: number) => void, owner?: string) =>
    new Promise<T>((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", p);
      xhr.withCredentials = true;
      if (owner) xhr.setRequestHeader("X-Pimg-Owner", owner);
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable && onProgress) onProgress(e.loaded / e.total);
      };
      xhr.onload = () => {
        let data: any = null;
        try {
          data = JSON.parse(xhr.responseText);
        } catch {}
        if (xhr.status >= 200 && xhr.status < 300) {invalidateQueryRequests();resolve(data as T);}
        else {
          if (xhr.status === 401) window.dispatchEvent(new CustomEvent("pimg:unauthorized"));
          reject(new ApiError(xhr.status, data?.error?.message || `上传失败 (${xhr.status})`));
        }
      };
      xhr.onerror = () => reject(new ApiError(0, "网络错误"));
      xhr.send(form);
    }),
};

export interface AuthUser { username: string; owner_id: string; role: "admin" | "user"; setup_required?: boolean; cache_scope?: string; }

export function galleryApi(actor: AuthUser, owner = actor.owner_id) {
  const base = actor.role === "admin" ? "/api/admin" : "/api/account";
  const address = (path: string) => {
    const url = new URL(base + path, window.location.origin);
    if (actor.role === "admin") url.searchParams.set("owner_id", owner);
    return url.pathname + url.search;
  };
  const send = <T>(path: string, method = "GET", body?: unknown, signal?: AbortSignal) => req<T>(address(path), {
    method, signal, headers: { "X-Pimg-Owner": actor.owner_id, ...(body !== undefined ? { "Content-Type": "application/json" } : {}) },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  return {
    cached:<T>(path:string)=>queryEntry(queryKey(address(path),actor.owner_id))?.value as T|undefined,
    cachedPreviewState:<T>()=>queryEntry(queryKey(address("/files/previews?view=snapshot"),actor.owner_id))?.value as T|undefined,
    rememberPreviewState:<T>(value:T)=>storeQuery(queryKey(address("/files/previews?view=snapshot"),actor.owner_id),value,undefined,queryEpoch()),
    get: <T>(path: string, signal?: AbortSignal) => send<T>(path, "GET", undefined, signal),
    post: <T>(path: string, body?: unknown) => send<T>(path, "POST", body),
    patch: <T>(path: string, body: unknown) => send<T>(path, "PATCH", body),
    del: <T>(path: string) => send<T>(path, "DELETE"),
    upload: <T>(path: string, form: FormData, progress?: (n: number) => void) => api.upload<T>(address(path), form, progress, actor.owner_id),
    url: address,
  };
}

// Bind requests to the account whose page is rendered, including stale tabs
// after the shared browser cookie has been replaced by a different login.
export function accountApi(owner: string) {
  const send = <T>(path: string, method = "GET", body?: unknown, signal?: AbortSignal) => req<T>("/api/account" + path, {
    method, signal, headers: { "X-Pimg-Owner": owner, ...(body !== undefined ? { "Content-Type": "application/json" } : {}) },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  return {
    get: <T>(path: string, signal?: AbortSignal) => send<T>(path, "GET", undefined, signal),
    post: <T>(path: string, body?: unknown) => send<T>(path, "POST", body),
    patch: <T>(path: string, body: unknown) => send<T>(path, "PATCH", body),
    del: <T>(path: string) => send<T>(path, "DELETE"),
    upload: <T>(path: string, form: FormData, progress: (n: number) => void) => api.upload<T>("/api/account" + path, form, progress, owner),
  };
}

// ---------- 类型 ----------

export interface SiteInfo {
  site_name: string;
  site_desc: string;
  logo_url: string;
  theme: string;
  footer_text: string;
  footer_links?: FooterLink[];
  icp_number: string;
  icp_url: string;
  police_number: string;
  police_url: string;
}

export interface FooterLink {
  id: string;
  label: string;
  icon: "github" | "website" | "link" | "mail" | "rss" | "telegram" | "heart" | "photo" | "custom";
  url: string;
  icon_url?: string;
}

export interface HomeItem {
  id: string;
  name: string;
  kind: "image" | "video" | "other";
  ext: string;
  width: number;
  height: number;
  duration: number;
  size: number;
  thumb_url: string;
  media_at?: number;
  created_at: number;
  published_at: number;
}

export interface StorageLocation {
 name?: string;
	 encrypted?: boolean;
	 state?: string;
	 part_count?: number;
	 saved_parts?: number;
	 original?: boolean;
  role: "archive" | "published" | "temporary";
  backend: string;
  kind?: string;
  expires_at?: number;
  confirmed?: boolean;
}

export interface MediaPart {
  index: number; count: number; start: number; duration: number; size: number;
  mime: string; ext: string; original: boolean;
}

export interface FileItem {
	media_version?: number;
	encryption_requested?: boolean;
	local_ready?: boolean;
	media_parts?: MediaPart[];
  owner_id?: string;
  id: string;
  name: string;
  orig_name: string;
  ext: string;
  mime: string;
  size: number;
  sha256: string;
  kind: string;
  width: number;
  height: number;
  duration: number;
  album_id: number | null;
  archive_state: "pending" | "running" | "done" | "failed";
  archive_error: string;
  publish_error?: string;
  purge_state?: "" | "pending" | "running" | "failed";
  purge_error?: string;
  task_references?: number[];
  has_thumb: boolean;
  thumb_w: number;
  thumb_h: number;
  has_bthumb: boolean;
  published: boolean;
  published_at: number;
  show_home: boolean;
  home_visibility?: "inherit" | "public" | "private";
  storage?: "tg" | "direct";
  encrypted?: boolean;
  permanent_publish_allowed?: boolean;
  conversion?: {
    kind?: "convert" | "transfer_owner";
    target_owner_id?: string;
    target_album_id?: number;
    task_id?: number;
    target_encrypted: boolean;
    phase: "prepare" | "archive" | "cleanup" | "done";
    state: "pending" | "running" | "failed" | "done" | "canceled";
    error?: string;
    target_file_id?: string;
    cleanup_pending?: boolean;
    cleanup_error?: string;
  };
  cold_storage: string;
  hot_storage: string;
  storage_locations?: StorageLocation[];
  available_links?: AvailableLinksResponse;
  created_at: number;
  updated_at: number;
  media_at?: number;
  media_time_source?: "exif" | "video_metadata" | "file_created" | "ingested" | "manual";
  url?: string;
  thumb_url?: string;
  blur_url?: string;
  error?: string;
  operation?: { id: number; type: string; state: string; error?: string; result?: string | { new_file_id?: string; [key: string]: unknown } | null };
}

export interface TempView {
  status: "ready" | "pending";
  url?: string;
  expires_at?: number;
  task_id?: number;
}

export interface AvailableLink {
  id: string;
  role: "archive" | "published" | "temporary";
  backend: string;
  kind: "original" | "thumb" | "blur";
  url: string;
  access?: "api_token";
  representation?: "stored";
  expires_at?: number;
  object_expires_at?: number;
}

export interface OriginalView {
	part?: MediaPart;
	compatible?: string;
  status: "loading" | "ready" | "failed";
  loaded: number;
  total: number;
  percent: number;
  source?: string;
  expires_at?: number;
  error?: string;
  url?: string;
}

export interface AvailableLinksResponse {
  items: AvailableLink[];
  warnings?: string[];
}

export interface TaskGroup {
  unsubmitted_files?: number;
  kind: "import"|"sync"|"archive"|"upload"|"purge"|"publish"|"unpublish"|"move_album"|"convert"|"maintenance"|"telegram_receive";
  operation_id?: number;
  task_id?: number;
  input_state?: string;
  batch_id: number;
  members: number;
  steps: number;
  completed_steps: number;
  failed_steps: number;
  total_files: number;
  processed_files: number;
  succeeded_files?: number;
  failed_files?: number;
  skipped_files?: number;
  canceled_files?: number;
  stage: string;
  source: string;
  can_retry?: boolean;
  can_cancel?: boolean;
}

export interface FileSuccessor {id:string;owner_id:string;kind:"transferred"|"converted"}
export interface OperationItem {
	file_successor?: FileSuccessor;
  owner_id?: string;
  key: string;
  file_id: string;
  file_name: string;
  state: string;
  error: string;
  file_exists: boolean;
}

export interface TaskItem {
	file_successor?: FileSuccessor;
  context?: {source?:string;description?:string;from_albums?:{id:number;name:string}[];to_album?:{id:number;name:string};storages?:string[]};
  owner_id?: string;
  group?: TaskGroup | null;
  file_exists?: boolean;
  can_retry?: boolean;
  can_cancel?: boolean;
  id: number;
  type: string;
  file_id: string;
  state: string;
  cancel_requested?: boolean;
  idle_only?: boolean;
  repository_name?: string;
  attempts: number;
  max_attempts: number;
  last_error: string;
  result: string | Record<string, unknown> | null;
  created_at: number;
  updated_at: number;
  run_after?: number;
  done_at?: number;
  file_name?: string;
  album_id?: number;
  album_name?: string;
}

export interface Album {
  displayed?: boolean;
  owner_id?: string;
  id: number;
  name: string;
  count: number;
  created_at: number;
  is_home?: boolean;
  is_unclassified?: boolean;
  telegram?: { chat_id: number; topic_id: number; state: string; error?: string; scan_date?: string };
}

export interface TelegramStatus {
  storage_enabled?: boolean;
  archive_spaces?: { owner_id: string; chat_id: number; state: string; last_error?: string }[];
  state: "unconfigured" | "connecting" | "phone" | "code" | "password" | "authorized" | "reauth_required" | "error";
  configured: boolean;
  account_id?: number;
  name?: string;
  error?: string;
  chat_id: number;
  password_hint?: string;
  upload_limit_bytes?: number;
}
export interface TelegramGroup { id: number; name: string; }
export interface TelegramTopic { id: number; name: string; closed: boolean; }

export interface TokenItem {
	scope?: "admin" | "diagnostics" | "uploader";
	topic_ids?: number[];
  id: string;
  name: string;
  prefix: string;
  created_at: number;
  last_used_at: number;
}

export interface AdminSettings {
	debug_enabled: boolean;
	debug_state?: { enabled: boolean; error?: string };
	log_retention_days: number;
	public_base_url: string;
  cold_storage: string;
  hot_storage: string;
  storage_options: { cold: StorageOption[]; hot: StorageOption[] };
  local_cold_dir: string;
  local_hot_dir: string;
  site_name: string;
  site_desc: string;
  logo_url: string;
  theme: string;
  footer_text: string;
  footer_links?: FooterLink[];
  icp_number: string;
  icp_url: string;
  police_number: string;
  police_url: string;
  home_cover_mode: "normal" | "blur" | "placeholder";
  home_page_size: number;
  upload_max_mb: number;
  original_cache_hours: number;
  allowed_exts: string;
  default_allowed_exts?: string[];
  effective_allowed_exts?: string[];
  retention_hours: number;
  presign_minutes: number;
  tg_configured?: boolean;
  r2_configured?: boolean;
  ffmpeg?: boolean;
}

export interface StorageOption { id: string; name: string; configured: boolean; }
const instanceNames = new Map<string,string>();
export function setStorageCatalog(items: {id:string;name:string}[]) {items.forEach(i=>instanceNames.set(i.id,i.name));}
export function storageName(provider: string, role?: string): string {
  if (provider === "r2" && role === "temporary") provider = "r2-temp";
 if(instanceNames.has(provider))return instanceNames.get(provider)!;
  return ({ local: "本地存储", telegram: "Telegram", r2: "Cloudflare R2", "r2-temp": "R2 临时存储" } as Record<string, string>)[provider] || provider || "未发布";
}

export interface StatusInfo {
  storage_checks: { instance_id: string; name: string; purpose: string; status: string }[];
  stats: {
    total: number;
    archived: number;
    failed: number;
    published: number;
    show_home: number;
    total_size: number;
    temp_live: number;
    tasks_failed: number;
    tasks_pending: number;
  };
  cold_storage: { provider: string; status: string };
  hot_storage: { provider: string; status: string };
  // Compatibility with a running server from before independent storage roles.
  tg?: { status: string };
  r2?: { status: string };
  ffmpeg: boolean;
  failed_tasks: { id: number; type: string; file_id: string; last_error: string; attempts: number }[];
  version: string;
  retention_hours: number;
  presign_minutes: number;
}

// ---------- 工具 ----------

export function displaySiteName(name?: string | null): string {
  const text = name?.trim();
  return !text || text.toLowerCase() === "pimg" ? "PIMG" : text;
}

export function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MB`;
  return `${(n / 1024 ** 3).toFixed(2)} GB`;
}

export function fileMediaTime(file: FileItem): number {
  return typeof file.media_at === "number" && Number.isFinite(file.media_at) ? file.media_at : file.created_at;
}

export function fileMediaTimeLabel(file: FileItem): string {
  if (file.media_time_source === "manual") return "手动设置日期";
  if (file.media_time_source === "exif") return "拍摄时间";
  if (file.media_time_source === "video_metadata") return "录制时间";
  if (file.media_time_source === "file_created") return "源文件创建时间";
  return "图库日期（无拍摄信息）";
}

export function fmtDur(sec: number): string {
  if (!sec) return "";
  const m = Math.floor(sec / 60);
  const s = Math.round(sec % 60);
  return `${m}:${String(s).padStart(2, "0")}`;
}

export function fmtDate(ts: number): string {
  if (!ts) return "—";
  const d = new Date(ts * 1000);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(
    d.getDate()
  ).padStart(2, "0")}`;
}

export function fmtTime(ts: number): string {
  if (!ts) return "—";
  const d = new Date(ts * 1000);
  return `${fmtDate(ts)} ${String(d.getHours()).padStart(2, "0")}:${String(
    d.getMinutes()
  ).padStart(2, "0")}`;
}

export function frameNo(item: { id: string }): string {
  // 帧编号：取文件 ID 后 6 位（稳定且唯一）
  return item.id.slice(-6);
}
