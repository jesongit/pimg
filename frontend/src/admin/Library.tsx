import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useSearchParams, useLocation, useNavigate } from "react-router-dom";
import { api as globalApi, fmtDur, galleryApi } from "../api";
import type { Album, FileItem, TempView } from "../api";
import { useToast } from "../App";
import ConfirmButton from "./ConfirmButton";
import DetailDrawer from "./DetailDrawer";
import { fileMediaTime } from "../api";
import { useUploads } from "./Uploads";
import SelectAllControl from "./SelectAllControl";
import FilterSelect from "./FilterSelect";
import { AlbumFilter, CreateAlbum } from "./AlbumManager";
import { BrowseFilters, UploadActions } from "./LibraryControls";
import { bucketKey, PhotoGroups, TimeIndex, useLibraryTimeline } from "./Timeline";
import type { DateGrouping, TimelineMonth } from "./Timeline";
import usePhotoNavigation from "./usePhotoNavigation";
import useViewerNavigation from "./useViewerNavigation";
import type { PhotoPageRange } from "./usePhotoNavigation";
import { archiveEncrypted, copyText, fileLink, fileStatus, filesUpdated, operationBusy } from "./interactions";

import UploadDialog from "./UploadDialog";
import { tasksUpdated } from "./TaskStatus";
import { clientID } from "../clientID";
import {openOperationDetails} from "./OperationDetails";

import { useGallery } from "./GalleryScope";
import useDragSelection from "./useDragSelection";
import BatchDateDialog from "./BatchDateDialog";
import BatchConversionDialog from "./BatchConversionDialog";
import SimilarImages from "./SimilarImages";
import useOperationRefresh from "./useOperationRefresh";
import {MoveAlbumSelect, moveAlbumAvailable, albumOwnerLabel, transferDescription} from "./MoveAlbumOptions";
import type {AlbumOwner} from "./MoveAlbumOptions";
import {rememberLibrary} from "./libraryMemory";
import useLibraryPosition from "./useLibraryPosition";
import FileEncryption from "./FileEncryption";

type Preview = TempView & { error?: string };
type BatchResult = { id: string; name?: string; status: string; reason?: string };
const PAGE_SIZE = 60;

export default function Library({ fixedAlbum, onBack, onAlbumChanged }: { fixedAlbum?: Album | null; onBack?: () => void; onAlbumChanged?: () => void }) {
  const toast = useToast(), uploads = useUploads();
  const [uploading, setUploading] = useState(false), [uploadFiles, setUploadFiles] = useState<File[]>([]);
  const { api, actor, admin, owner, readOnly, key: galleryKey } = useGallery();
  const operationRefresh=useOperationRefresh(api);
  const [users, setUsers] = useState<AlbumOwner[]>([]);
  useEffect(() => { if (!admin) return; let active=true,sequence=0; const load=()=>{const request=++sequence;void globalApi.get<{items:AlbumOwner[]}>("/api/admin/accounts").then(r=>{if(active&&request===sequence)setUsers(r.items||[])}).catch(()=>{})}; load(); window.addEventListener("pimg:files-updated",load); return()=>{active=false;window.removeEventListener("pimg:files-updated",load)}; }, [admin]);
  const [params] = useSearchParams();
  const location = useLocation(), navigate = useNavigate();
  const setParams = useCallback((next: URLSearchParams | Record<string, string>, options?: { replace?: boolean }) => {
    navigate({ pathname: location.pathname, search: new URLSearchParams(next).toString() }, options);
  }, [navigate, location.pathname]);
  const [restored]=useState(()=>{
    const q=new URLSearchParams();for(const key of ["q","kind","state","encryption","sort","seed"])if(params.get(key))q.set(key,params.get(key)!);
    if(fixedAlbum)q.set("album_id",String(fixedAlbum.id));else if(params.get("album_id"))q.set("album_id",params.get("album_id")!);q.set("per_page",String(PAGE_SIZE));
    const first=Math.max(1,Math.min(1000000,Math.floor(Number(params.get("start_page")))||1)),count=Math.max(1,Math.min(1000,Math.floor(Number(params.get("pages")))||1));
    const pages=Array.from({length:count},(_,i)=>api.cached<{items:FileItem[];total:number}>(`/files?${q}&page=${first+i}`));
    return pages.every(Boolean)?{query:q.toString(),first,count,pages:pages as {items:FileItem[];total:number}[],files:pages.flatMap(p=>p!.items),total:pages[0]!.total}:null;
  });
  const [files, setFiles] = useState<FileItem[]>(restored?.files||[]), [total, setTotal] = useState(restored?.total||0), [albums, setAlbums] = useState<Album[]>(()=>api.cached<{items:Album[]}>("/albums")?.items||[]);
  const [search, setSearch] = useState(params.get("q") || ""), [loading, setLoading] = useState(!restored), [error, setError] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set()), [detail, setDetail] = useState<string | null>(params.get("file"));
  const [moveAlbum, setMoveAlbum] = useState<number | null>(null);
  const [conversionIDs,setConversionIDs]=useState<string[]|null>(null);
  const [dedupeQuery,setDedupeQuery]=useState<string|null>(null);
  const [dateEditing,setDateEditing]=useState<{ids:string[];initial:number}|null>(null);
  const resultsRoot=useRef<HTMLDivElement>(null);
  const [previews, setPreviews] = useState<Record<string, Preview>>(()=>api.cachedPreviewState<Record<string,Preview>>()||{});
  useEffect(()=>{api.rememberPreviewState(previews)},[api,previews]);
  const [visibleIds, setVisibleIds] = useState<Set<string>>(new Set());
  const markVisible = useCallback((id: string) => setVisibleIds((previous) => previous.has(id) ? previous : new Set([...previous, id])), []);
  const [previewTick, setPreviewTick] = useState(0), [batchBusy, setBatchBusy] = useState(false);
  const [conversionNotices, setConversionNotices] = useState<Record<string, string>>({});
  const [linkFormat, setLinkFormat] = useState<"url" | "md">("url"), [dragging, setDragging] = useState(false);
  const openUpload = (files: File[] = []) => { setUploadFiles(files); setUploading(true); };
  const tools = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const bar = tools.current, root = bar?.parentElement;
    if (!bar || !root) return;
    const update = () => root.style.setProperty("--library-tools-height", `${bar.offsetHeight}px`);
    update();
    const observer = new ResizeObserver(update);
    observer.observe(bar);
    return () => observer.disconnect();
  }, []);
  const anchor = useRef<string | null>(null), sequence = useRef(0), controller = useRef<AbortController>();
  const selectionCache = useRef(new Map<string, FileItem>()), selectionController = useRef<AbortController>(), selectionSequence = useRef(0);
  const [selectingGroup, setSelectingGroup] = useState("");
  const [createForSelection, setCreateForSelection] = useState(false);
  const [creatingAlbum,setCreatingAlbum]=useState(false);
  const allApi=useMemo(()=>galleryApi(actor,"all"),[actor.owner_id,actor.role]);
  const [allMoveAlbums,setAllMoveAlbums]=useState<Album[]>(()=>allApi.cached<{items:Album[]}>("/albums")?.items||[]);
  const [moveAlbumsError,setMoveAlbumsError]=useState("");
  const [moveAlbumsReady,setMoveAlbumsReady]=useState(()=>!!allApi.cached<{items:Album[]}>("/albums"));
  const [moveAlbumsRefreshing,setMoveAlbumsLoading]=useState(false);
  const moveAlbumsLoading=admin&&!moveAlbumsReady&&moveAlbumsRefreshing;
  const moveAlbumsSequence=useRef(0);
  const moveNeeded=admin&&(selected.size>0||!!detail||createForSelection);
  const refreshMoveAlbums=useCallback(async(signal?:AbortSignal)=>{
    if(!admin)return;
    const request=++moveAlbumsSequence.current;setMoveAlbumsLoading(true);setMoveAlbumsError("");
    try{const response=await allApi.get<{items:Album[]}>("/albums",signal);if(!signal?.aborted&&request===moveAlbumsSequence.current){setAllMoveAlbums(response.items||[]);setMoveAlbumsReady(true);return true}return false}
    catch(error:any){if(!signal?.aborted&&request===moveAlbumsSequence.current&&error.name!=="AbortError")setMoveAlbumsError(error.message||"全部相册读取失败，请重试");return false}
    finally{if(!signal?.aborted&&request===moveAlbumsSequence.current)setMoveAlbumsLoading(false)}
  },[admin,allApi]);
  useEffect(()=>{if(!moveNeeded)return;const abort=new AbortController();void refreshMoveAlbums(abort.signal);const updated=()=>void refreshMoveAlbums(abort.signal);window.addEventListener("pimg:files-updated",updated);return()=>{abort.abort();window.removeEventListener("pimg:files-updated",updated)}},[moveNeeded,refreshMoveAlbums]);
  const moveAlbums=admin?allMoveAlbums:albums;
  const moveCandidate=moveAlbum===0?{id:0,name:"未分类",owner_id:owner,displayed:false}:moveAlbums.find(album=>album.id===moveAlbum);
  const moveTarget=moveAlbumAvailable(moveCandidate,users)?moveCandidate:undefined;
  useEffect(()=>{if(moveAlbum!==null&&moveAlbumsReady&&!moveAlbumsRefreshing&&!moveAlbumsError&&!moveTarget)setMoveAlbum(null)},[moveAlbum,moveAlbumsReady,moveAlbumsRefreshing,moveAlbumsError,moveTarget]);
  const crossMove=!!moveTarget?.owner_id&&moveTarget.owner_id!==owner;
  const moveDescription=crossMove?transferDescription(albumOwnerLabel(moveTarget!.owner_id!,users,actor),moveTarget!.name):`将选中的 ${selected.size} 项移入「${moveTarget?.name||"目标相册"}」，用户归属保持不变。${moveTarget?.displayed?"目标相册已公开，未单独隐藏的文件移入后会公开缩略图。":""}`;
  useEffect(()=>{setDateEditing(null);setConversionIDs(null);setDedupeQuery(null)},[galleryKey,params.get("q"),params.get("album_id"),params.get("kind"),params.get("state"),params.get("encryption"),params.get("sort"),params.get("seed")]);
  const dragSelection=useDragSelection({root:resultsRoot,enabled:!readOnly&&!batchBusy&&!selectingGroup&&!detail&&!dateEditing&&!conversionIDs&&!uploading&&files.length>0,scope:`${galleryKey}:${params.get("q")}:${params.get("album_id")}:${params.get("kind")}:${params.get("state")}:${params.get("encryption")}:${params.get("sort")}:${params.get("seed")}:${params.get("group")}`,selected,onHit:ids=>{const byId=new Map(files.map(file=>[file.id,file]));for(const id of ids){const file=byId.get(id);if(file)selectionCache.current.set(id,file)}setSelected(previous=>{const next=new Set(previous);for(const id of ids){if(next.has(id))next.delete(id);else next.add(id)}return next});anchor.current=null},onRestore:setSelected});
  const scope = useRef(restored?.query||"");
  const pageCache = useRef(new Map<number, { items: FileItem[]; total: number }>()), dataRange = useRef<PhotoPageRange | null>(restored?{query:restored.query,first:restored.first,count:restored.count}:null);
  const firstPage = Math.max(1, Math.min(1000000, Math.floor(Number(params.get("start_page"))) || 1));
  const pages = Math.max(1, Math.min(1000, Math.floor(Number(params.get("pages"))) || 1));
  const kind = params.get("kind") || "", state = params.get("state") || "", encryption = params.get("encryption") || "", albumID = params.get("album_id") || "", sort = params.get("sort") || "newest";
  const query = useMemo(() => {
    const next = new URLSearchParams();
    for (const key of ["q", "kind", "state", "encryption", "sort", "seed"]) if (params.get(key)) next.set(key, params.get(key)!);
    if (fixedAlbum) next.set("album_id", String(fixedAlbum.id)); else if (albumID) next.set("album_id", albumID);
    next.set("per_page", String(PAGE_SIZE)); return next.toString();
  }, [params, fixedAlbum?.id, albumID]);
  const selectedKey = [...selected].sort().join("|");
  useEffect(() => { setMoveAlbum(null); }, [selectedKey, query]);
  const grouping: DateGrouping = params.get("group") === "year" ? "year" : params.get("group") === "week" ? "week" : params.get("group") === "month" ? "month" : "day";
  const chronological = sort === "newest" || sort === "oldest";
  const timeline = useLibraryTimeline(query, chronological, grouping);
  const position=useLibraryPosition(`${galleryKey}:${fixedAlbum?.id||"all"}:${query}:${grouping}`,!loading,resultsRoot);
  useEffect(() => { selectionController.current?.abort(); selectionSequence.current++; setSelectingGroup(""); return () => selectionController.current?.abort(); }, [query, grouping]);
  const albumSequence=useRef(0),currentGallery=useRef(galleryKey);currentGallery.current=galleryKey;
  const refreshAlbums = useCallback(async () => {
    const request=++albumSequence.current;
    try { const response = await api.get<{ items: Album[] }>("/albums"); if(request===albumSequence.current&&currentGallery.current===galleryKey)setAlbums(response.items || []); }
    catch (e: any) { if(e.name!=="AbortError"&&request===albumSequence.current&&currentGallery.current===galleryKey)toast(e.message || "相册读取失败，请重试。", "err"); }
  }, [toast, api,galleryKey]);
  const loadedSnapshot=useRef({key:"",value:"",ids:new Set<string>()});
  const refresh = useCallback(async (quiet = false, reuse = false) => {
    controller.current?.abort(); const abort = new AbortController(); controller.current = abort;
    const snapshots=Array.from({length:pages},(_,i)=>api.cached<{items:FileItem[];total:number}>(`/files?${query}&page=${firstPage+i}`));
    const available=snapshots.every(Boolean);
    const request = ++sequence.current; if (!quiet) setLoading(!available); setError("");
    if(available){setFiles(snapshots.flatMap(p=>p!.items));setTotal(snapshots[0]!.total)}
    if (scope.current !== query) { if(!available)setFiles([]); setSelected(new Set()); anchor.current = null; selectionCache.current.clear(); pageCache.current.clear(); scope.current = query; }
    try {
      if (!reuse) pageCache.current.clear();
      const responses = await Promise.all(Array.from({ length: pages }, (_, index) => {
        const page = firstPage + index;
        return api.get<{ items: FileItem[]; total: number }>(`/files?${query}&page=${page}`, abort.signal);
      }));
      if (request !== sequence.current) return;
      const next = Array.from(new Map(responses.flatMap((response) => response.items || []).map((file) => [file.id, file])).values());
      const snapshotKey=`${galleryKey}:${query}:${firstPage}:${pages}`;
      const snapshotValue=JSON.stringify([responses[0].total,next.map(file=>[file.id,file.owner_id,file.album_id,file.name,file.archive_state,file.purge_state,file.published,file.show_home,file.home_visibility,file.media_at,file.operation?.id,file.operation?.state,file.conversion?.state,file.conversion?.phase])]);
      const changed=loadedSnapshot.current.key===snapshotKey&&loadedSnapshot.current.value!==snapshotValue;
      const previousIDs=loadedSnapshot.current.key===snapshotKey?loadedSnapshot.current.ids:new Set<string>();
      loadedSnapshot.current={key:snapshotKey,value:snapshotValue,ids:new Set(next.map(file=>file.id))};
      responses.forEach((response, index) => pageCache.current.set(firstPage + index, response));
      dataRange.current = { query, first: firstPage, count: pages };
      setFiles(next); setTotal(responses[0].total);
      for (const file of next) if (selectionCache.current.has(file.id)) selectionCache.current.set(file.id, file);
      setSelected((previous) => new Set([...previous].filter((id) => { const file = next.find(file => file.id === id) || (!previousIDs.has(id)?selectionCache.current.get(id):undefined); if(!file)selectionCache.current.delete(id);return file && !file.purge_state; })));
      if(changed)queueMicrotask(filesUpdated);
    } catch (e: any) { if (e.name !== "AbortError" && request === sequence.current) setError(e.message || "图库暂时无法读取，请重试。"); }
    finally { if (request === sequence.current) setLoading(false); }
  }, [query, pages, firstPage, api, galleryKey]);
  useLayoutEffect(()=>{rememberLibrary(actor,params)},[actor.owner_id,actor.role,params]);
  useEffect(() => { setSearch(params.get("q") || ""); }, [params.get("q")]);
  useEffect(() => { void refresh(false, true); return () => controller.current?.abort(); }, [refresh]);
  useEffect(() => { void refreshAlbums(); }, [refreshAlbums]);
  const onTimeJump = useCallback((rangeChanges: boolean) => {
    // A date jump changes the loaded page window, not the selection scope.
    for (const file of files) if (selected.has(file.id)) selectionCache.current.set(file.id, file);
    anchor.current = null; setDetail(null); position.cancelRestore();
    // Keep the measured grid mounted while seeking another page window. A
    // remount initially uses its fallback width and shifts the scroll anchor.
    if (rangeChanges) { sequence.current++; controller.current?.abort(); setLoading(true); }
  }, [files, selected]);
  const navigation = usePhotoNavigation({ params, setParams, query, first: firstPage, count: pages, pageSize: PAGE_SIZE, total, files, groups: timeline.groups, grouping, loading, indexLoading: timeline.loading, error, chronological, paused: !position.ready || !!detail || batchBusy || !!selectingGroup, dataRange, onJump: onTimeJump });
  useEffect(() => {
    const update = () => { void refresh(true); void refreshAlbums(); };
    const visible=()=>{if(!document.hidden)update()},stored=(event:StorageEvent)=>{if(event.key==="pimg:files-updated")update()};
    window.addEventListener("pimg:files-updated", update);window.addEventListener("storage",stored);document.addEventListener("visibilitychange",visible);
    return () => {window.removeEventListener("pimg:files-updated", update);window.removeEventListener("storage",stored);document.removeEventListener("visibilitychange",visible)};
  }, [refresh, refreshAlbums]);
  const busy = files.some((f) => operationBusy(f) || ["pending", "running"].includes(f.archive_state) || ["pending", "running"].includes(f.purge_state || ""));
  useEffect(() => { if (!busy||operationRefresh.watching) return; const timer = setInterval(() => { void refresh(true); }, 1500); return () => clearInterval(timer); }, [busy, refresh,operationRefresh.watching]);
  useEffect(() => { if (!albums.some(album => ["pending", "creating", "running", "syncing", "moving", "deleting"].includes(album.telegram?.state || ""))) return; const timer = setInterval(() => void refreshAlbums(), 3000); return () => clearInterval(timer); }, [albums, refreshAlbums]);
  const changeFilter = (key: string, value: string) => {
    sequence.current++; controller.current?.abort(); const next = new URLSearchParams(params); for (const field of ["pages", "start_page", "file", "before", "after", "seek"]) next.delete(field);
    if (key === "owner_id") next.delete("album_id");
    if (key === "sort") { if (value === "random") next.set("seed", clientID()); else next.delete("seed"); }
    if (value) next.set(key, value); else next.delete(key);
    setSelected(new Set()); anchor.current = null; setDetail(null); setParams(next);
  };
  useEffect(() => {
    const target = search.trim(); if (target === (params.get("q") || "")) return;
    const timer = setTimeout(() => changeFilter("q", target), 250); return () => clearTimeout(timer);
  }, [search, params]);
  const clearFilters = () => { const next = new URLSearchParams(params); for (const key of ["q","kind","state","encryption","sort","seed","owner_id","album_id","before","after","seek","start_page","pages","file"]) next.delete(key); next.set("group", "day"); setSearch(""); setSelected(new Set()); setDetail(null); anchor.current = null; setParams(next); window.scrollTo({ top: 0, behavior: "instant" as ScrollBehavior }); };
  const needPreviews = files.filter((f) => visibleIds.has(f.id) && f.archive_state === "done" && (f.has_thumb || f.kind === "image" || f.kind === "video") && !f.purge_state && !(f.published && f.thumb_url) && (!previews[f.id] || previews[f.id].status === "pending" || !!previews[f.id].expires_at && previews[f.id].expires_at! * 1000 <= Date.now())).map((f) => f.id);
  const previewKey = needPreviews.join(",");
  useEffect(() => {
    if (!previewKey) return; let stopped = false; const ids = previewKey.split(",");
    const load = async () => {
      const loaded: Record<string, Preview> = {};
      await Promise.all(Array.from({ length: Math.ceil(ids.length / 60) }, async (_, index) => {
        const chunk = ids.slice(index * 60, (index + 1) * 60);
        try {
          const response = await api.get<{ previews: Record<string, TempView> }>(`/files/previews?ids=${chunk.join(",")}`);
          for (const id of chunk) loaded[id] = response.previews[id] || { status: "ready", error: "预览不可用" };
        }
        catch { for (const id of chunk) loaded[id] = { status: "ready", error: "预览加载失败" }; }
      }));
      if (stopped) return;
      setPreviews((previous) => ({ ...previous, ...loaded }));
      if (Object.values(loaded).some((preview) => preview.status === "ready" && preview.expires_at)) void refresh(true);
    };
    const timer = setTimeout(load, 32); return () => { stopped = true; clearTimeout(timer); };
  }, [previewKey, previewTick, refresh]);
  useEffect(() => { const timer = setInterval(() => setPreviewTick((tick) => tick + 1), Object.values(previews).some((p) => p.status === "pending") ? 2000 : 15000); return () => clearInterval(timer); }, [previews]);
  const retryPreview = (id: string) => setPreviews((previous) => { const next = { ...previous }; delete next[id]; return next; });
  const select = (id: string, range = false) => {
    const previousAnchor = anchor.current;
    setSelected((previous) => {
      const next = new Set(previous), start = files.findIndex((f) => f.id === previousAnchor), end = files.findIndex((f) => f.id === id);
      if (range && start >= 0) files.slice(Math.min(start, end), Math.max(start, end) + 1).forEach((f) => { if (!f.purge_state) next.add(f.id); });
      else if (next.has(id)) next.delete(id); else next.add(id); return next;
    }); anchor.current = id;
  };
  const actionFiles = Array.from(new Map([...selectionCache.current, ...files.map(file => [file.id, file] as const)]).values());
  const selectGroup = async (group: TimelineMonth, clear: boolean) => {
    if (batchBusy || selectingGroup) return;
    if (clear) { setSelected(previous => new Set([...previous].filter(id => { const file = actionFiles.find(file => file.id === id); return !file || bucketKey(fileMediaTime(file), grouping) !== group.key; }))); return; }
    const serial = ++selectionSequence.current, abort = new AbortController(); selectionController.current?.abort(); selectionController.current = abort; setSelectingGroup(group.key);
    try {
      const q = new URLSearchParams(query); q.delete("page"); q.set("after", String(group.start)); q.set("before", String(group.end)); q.set("per_page", "200");
      const first = await api.get<{ items: FileItem[]; total: number }>(`/files?${q}&page=1`, abort.signal);
      const all = [...(first.items || [])];
      for (let page = 2; page <= Math.ceil(first.total / 200); page++) { const response = await api.get<{ items: FileItem[] }>(`/files?${q}&page=${page}`, abort.signal); all.push(...(response.items || [])); }
      if (abort.signal.aborted || serial !== selectionSequence.current) return;
      for (const file of all) selectionCache.current.set(file.id, file);
      const eligible = all.filter(file => !file.purge_state);
      setSelected(previous => new Set([...previous, ...eligible.map(file => file.id)])); anchor.current = null;
      if (eligible.length !== all.length) toast(`已选择 ${eligible.length} 项，已跳过 ${all.length - eligible.length} 项正在清理的文件`);
    } catch (error: any) { if (!abort.signal.aborted && serial === selectionSequence.current) toast(error.message || "分区未能全选，请重试。", "err"); }
    finally { if (serial === selectionSequence.current) setSelectingGroup(""); }
  };
  const eligible = files.filter((file) => !file.purge_state), allSelected = eligible.length > 0 && eligible.every((file) => selected.has(file.id));
  const selectLoaded = () => {
    for (const file of eligible) selectionCache.current.set(file.id, file);
    setSelected(previous => {
      const next = new Set(previous);
      for (const file of eligible) if (allSelected) next.delete(file.id); else next.add(file.id);
      return next;
    });
    anchor.current = null;
  };
  const cancelSelection = () => { selectionController.current?.abort(); selectionSequence.current++; setSelectingGroup(""); setSelected(new Set()); selectionCache.current.clear(); anchor.current = null; };
  const batch = async (action: string, extra?: { album_id?: number; media_at?:number; encrypted?:boolean; confirm?: string }, selectedIDs?:string[]) => {
    if (readOnly || !(selectedIDs?.length??selected.size) || batchBusy || selectingGroup) return false;
    setBatchBusy(true);
    let operationID:number|undefined;
    try {
      const results: BatchResult[] = [],ids=selectedIDs?[...selectedIDs]:[...selected];
      const accepted=await api.post<{operation_id:number;task_id:number}>("/operations",{key:clientID(),kind:action,total_files:ids.length,...(extra?.album_id!==undefined?{album_id:extra.album_id}:{}),...(extra?.media_at!==undefined?{media_at:extra.media_at}:{}),...(extra?.encrypted!==undefined?{encrypted:extra.encrypted}:{})});
      operationID=accepted.operation_id;
      for (let offset = 0; offset < ids.length; offset += 200) {
        const chunk = ids.slice(offset, offset + 200);
        try {
          const part = await api.post<{ done?: number; skipped?: number; task_id?: number; task_ids?: number[]; results?: BatchResult[] }>("/batch", { action, operation_id:operationID, ids: chunk, ...(action === "purge" ? {confirm:"PURGE"} : {}), ...extra });
          const queued = !!part.task_id || !!part.task_ids?.length;
          for (const id of chunk) {
            const result = part.results?.find(result => result.id === id);
            if (result && ["done", "submitted", "skipped", "failed"].includes(result.status)) results.push(result);
            else results.push({ id, name: actionFiles.find(file => file.id === id)?.name, status: queued ? "submitted" : "failed", reason: queued ? "任务已提交" : "服务器未返回确认结果，请刷新后核对。" });
          }
        } catch (error: any) { results.push(...chunk.map((id) => ({ id, name: actionFiles.find((file) => file.id === id)?.name, status: "failed", reason: `未能确认提交：${error.message || "请刷新后核对任务状态。"}` }))); if(error.status===409){results.push(...ids.slice(offset+200).map(id=>({id,status:"failed",reason:"本次操作已停止，未继续提交。"})));break} }
      }
      setSelected(new Set(results.filter(result => result.status === "failed").map(result => result.id)));
      const count = (status: string) => results.filter(result => result.status === status).length;
      const completed = count("done"), submitted = count("submitted"), skipped = count("skipped"), failed = count("failed");
      const issues = results.filter(result => result.status === "failed" || result.status === "skipped");
      const summary = [completed && `已完成 ${completed} 项`, submitted && `已提交 ${submitted} 项`, skipped && `跳过 ${skipped} 项`, failed && `${failed} 项未处理`].filter(Boolean).join("，");
      toast(summary || "未收到处理结果，请刷新后核对。", completed + submitted === 0 ? "err" : "ok", {
        key:`operation:${operationID}`,
        details: issues.map(result => ({ label: result.name || result.id, description: `${result.status === "skipped" ? "已跳过" : "未处理"}：${result.reason || "请核对文件状态后重试。"}` })),
        onAction:()=>openOperationDetails(operationID!),actionLabel:"查看操作详情",
        persistent: issues.length > 0,
      });
      return true;
    } catch (e: any) { if(e.name!=="AbortError")toast(e.message, "err"); return false; } finally { if(operationID)try{await api.post(`/operations/${operationID}/seal`,{})}catch(error:any){toast(`操作已提交，结束提交确认失败：${error.message||"请查看操作详情。"}`,"err",{key:`operation:${operationID}`,persistent:true,onAction:()=>openOperationDetails(operationID!),actionLabel:"查看操作详情"})}setBatchBusy(false);if(operationID){operationRefresh.watch(operationID);tasksUpdated();filesUpdated()} }
  };
  const copySelected = async () => {
    if (selectingGroup || batchBusy) return;
    const chosen = actionFiles.filter((f) => selected.has(f.id) && f.published && f.url && !f.purge_state && !archiveEncrypted(f) && !f.conversion);
    if (!chosen.length) { toast("选中的图片尚未发布，请先选择要公开的图片并发布。", "err"); return; }
    try { await copyText(chosen.map((file) => fileLink(file, linkFormat)).join("\n")); toast(`已复制 ${chosen.length} 项${selected.size > chosen.length ? `，已跳过 ${selected.size - chosen.length} 项未发布或不可用文件` : ""}`); } catch (e: any) { toast(e.message, "err"); }
  };
  const open = (id: string) => setDetail(id);
  const close = useCallback(() => setDetail(null), []);
  const viewer = useViewerNavigation({ id: detail, query, scope: galleryKey, api, onOpen: setDetail });
  const filterActive = !!(params.get("q") || kind || state || encryption || sort !== "newest" || grouping !== "day" || owner !== actor.owner_id || (!fixedAlbum && albumID));
  const shownAlbum = fixedAlbum ? albums.find((album) => album.id === fixedAlbum.id) || fixedAlbum : albums.find(album => String(album.id) === albumID||albumID==="-1"&&album.is_unclassified) || null;
  const dateFile=actionFiles.find(file=>selected.has(file.id));
  return <div className={`library${readOnly ? " is-readonly" : ""}${selected.size ? " has-selection" : ""}${chronological ? " has-timeline" : ""}`}>
    <h1 className="sr-only">图库</h1>
    <div ref={tools} className="library-tools-surface">
    <div className="library-toolbar">
      {!readOnly && <div className="library-selection"><SelectAllControl checked={allSelected} indeterminate={eligible.some(file=>selected.has(file.id))} disabled={loading||batchBusy||!!selectingGroup||!eligible.length} scope="全选已加载图片" onChange={selectLoaded}/></div>}
      <div className="library-search-group">
      <label className={`library-search${params.get("q") ? " is-active" : ""}`}><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" aria-hidden="true"><circle cx="10" cy="10" r="6.5"/><path d="m15 15 5 5"/></svg><input className="input" placeholder={shownAlbum?"在相册中搜索":"搜索所有照片"} aria-label="搜索图片名称或编号" value={search} onChange={(e)=>setSearch(e.target.value)}/></label>
      <span className="library-scope-count" role="status">{loading&&!files.length?"读取中…":`${total} 项`}</span>
      </div>
      <div className="library-toolbar-actions">
      {fixedAlbum&&<button className="btn btn-ghost" onClick={onBack}>返回</button>}
      {!readOnly&&<button className="btn library-dedupe-button" disabled={batchBusy||loading||!total} onClick={()=>setDedupeQuery(query)}><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M8 3H3v5m13-5h5v5M3 16v5h5m13-5v5h-5M8 7h8v10H8z"/></svg>查重</button>}
      {sort==="random"&&<button className="btn btn-ghost library-shuffle-button" onClick={()=>changeFilter("sort","random")}>重新随机排列</button>}
      {(admin || !readOnly) && <UploadActions dragging={dragging} onDragging={setDragging} onUpload={()=>void openUpload()} onFiles={openUpload} onCreateAlbum={()=>setCreatingAlbum(true)}/>}
      </div>
    </div>
    <div className="library-filter-row">
      <BrowseFilters kind={kind} state={state} encryption={encryption} sort={sort} grouping={grouping} groupingDisabled={!chronological||!!selectingGroup} reset={filterActive&&<button type="button" className="btn btn-ghost browse-clear" onClick={clearFilters}>重置</button>} onChange={changeFilter} onGroupingChange={value=>{const next=new URLSearchParams(params);next.set('group',value);for(const key of ['before','after','seek','start_page','pages','file'])next.delete(key);setParams(next);setSelected(new Set());selectionCache.current.clear()}}>
        {admin && <FilterSelect label="图库用户" prefix="用户" className="gallery-user-filter" value={owner} active={owner!==actor.owner_id} width={200} onChange={value=>changeFilter("owner_id",value===actor.owner_id?"":value)} options={[{value:actor.owner_id,label:"自己"},...users.filter(user=>user.id!==actor.owner_id).map(user=>({value:user.id,label:user.username}))]}/>}
        {fixedAlbum ? <span className="library-fixed-album" title={shownAlbum?.name}><span className="library-filter-prefix">相册</span><span>{shownAlbum?.name}</span></span> : <AlbumFilter albums={albums} value={albumID} onSelect={id=>changeFilter("album_id",id)}/>}
      </BrowseFilters>
    </div>
    </div>
    {dedupeQuery!==null&&<SimilarImages query={dedupeQuery} albums={albums} onClose={()=>setDedupeQuery(null)}/>}
    {creatingAlbum&&(admin||!readOnly)&&<CreateAlbum albums={albums} onClose={()=>setCreatingAlbum(false)} onCreated={async(response,existing)=>{setCreatingAlbum(false);await refreshAlbums();onAlbumChanged?.();filesUpdated();toast(response.owner_id&&response.owner_id!==actor.owner_id?"已为所选用户创建相册，可在导入时选择":existing?"相册已关联，可在相册设置中选择同步起点":"相册已创建")}}/>}
    {uploading && (admin || !readOnly) && <UploadDialog albums={albums} initialFiles={uploadFiles} onClose={()=>{setUploading(false);setUploadFiles([])}} onUpload={(files,encrypted,albumID)=>{uploads.enqueue(files,!encrypted,albumID)}}/>}
    {shownAlbum?.displayed && <p className="gallery-owner-notice">此相册已公开缩略图，单独取消公开的文件仍会隐藏。原件访问权限保持不变。</p>}
    {owner!==actor.owner_id && <p className="gallery-owner-notice" role="status">正在管理{users.find(u=>u.id===owner)?.username || "所选用户"}的图库</p>}
    {error && <div className="library-notice" role="alert"><p>{error}</p><button className="btn" onClick={() => refresh()}>重新加载</button></div>}
    <div ref={navigation.top} className="library-scroll-sentinel" aria-hidden="true" />
    {loading && !files.length ? <div className="empty" role="status">正在读取图库…</div> : !error && !files.length ? <div className="empty"><p>{filterActive ? "没有符合筛选条件的图片" : fixedAlbum || shownAlbum ? "相册还是空的" : "还没有归档的图片"}</p>{(!readOnly || filterActive) && <button className="btn" onClick={filterActive ? clearFilters : () => openUpload()}>{filterActive ? "清除筛选" : "上传第一张图片"}</button>}</div> : <div ref={resultsRoot} className={`library-results${dragSelection.active?" is-drag-selecting":""}${dragSelection.mode?" is-selection-mode":""}`} aria-busy={loading} {...dragSelection.bind}><PhotoGroups files={files} chronological={chronological} grouping={grouping} index={timeline.groups} selected={selected} selectionFiles={actionFiles} selecting={selectingGroup} disabled={loading||batchBusy||timeline.loading||!!timeline.error} onSelectGroup={(group,clear)=>void selectGroup(group,clear)} render={(file) => <FileCard key={file.id} file={file} preview={file.published && file.thumb_url && !previews[file.id]?.error ? { status: "ready", url: file.thumb_url } : previews[file.id]} selected={selected.has(file.id)} selectionMode={!readOnly&&selected.size>0} onVisible={markVisible} onSelect={(range) => select(file.id, range)} onOpen={() => open(file.id)} onRetry={() => retryPreview(file.id)} onPreviewError={() => setPreviews((previous) => ({ ...previous, [file.id]: { status: "ready", error: "预览失效，点击重试" } }))} />} /></div>}
    <div ref={navigation.bottom} className="library-scroll-sentinel" aria-hidden="true" />
    {!!files.length && (navigation.hasNewer || navigation.hasOlder || !!error) && <div className="library-pagination"><span className="sub">已加载 {files.length} 项</span>{error ? <button className="btn" disabled={loading} onClick={() => void refresh()}>重试加载</button> : <>{navigation.hasNewer && <button className="btn" disabled={loading} onClick={navigation.loadNewer}>加载前面的图片</button>}{navigation.hasOlder && <button className="btn" disabled={loading} onClick={navigation.loadOlder}>{loading ? "正在加载…" : "加载更多"}</button>}</>}</div>}
    {!!selected.size && !readOnly && <div className="batch-bar" aria-label="批量整理" aria-busy={batchBusy}><div className="batch-heading"><span className="count" role="status" aria-live="polite">已选 {selected.size} 项</span><button className="btn btn-ghost" disabled={batchBusy} onClick={cancelSelection}>取消选择</button></div><fieldset disabled={batchBusy||!!selectingGroup} className="batch-actions"><ConfirmButton className="btn btn-crayon batch-publish" title="永久发布选中的可发布文件" confirmLabel="确认发布" resetKey={selectedKey} description="发布后可通过公开链接访问原件。不符合条件的文件会跳过。" onConfirm={() => batch("publish")}>永久发布</ConfirmButton>{admin&&<><ConfirmButton className="btn batch-public" confirmLabel="确认批量公开" resetKey={selectedKey} description={`将选中的 ${selected.size} 项缩略图公开到首页，不开放原件或原视频；未就绪或正在删除的文件会跳过。`} onConfirm={()=>batch("show_home")}>批量公开</ConfirmButton><ConfirmButton className="btn batch-private" confirmLabel="确认批量取消公开" resetKey={selectedKey} description={`将选中的 ${selected.size} 项从首页隐藏，即使所属相册公开也不再展示。原件的永久发布链接独立管理。`} onConfirm={()=>batch("hide_home")}>批量取消公开</ConfirmButton></>}<ConfirmButton className="btn btn-danger batch-purge" confirmLabel="确认删除" resetKey={selectedKey} description={`永久删除选中的 ${selected.size} 项及其存储副本和索引，无法恢复。服务器导入源文件会保留。`} onConfirm={() => batch("purge")}>批量删除</ConfirmButton><button className="btn batch-unpublish" onClick={() => batch("unpublish")}>撤销永久发布</button><button type="button" className="btn batch-date" onClick={()=>setDateEditing({ids:[...selected],initial:dateFile?fileMediaTime(dateFile):Math.floor(Date.now()/1000)})}>修改日期</button><button type="button" className="btn batch-convert" onClick={()=>setConversionIDs([...selected])}>加密转换</button><span className="choice-confirm-slot"><MoveAlbumSelect className="select" aria-label="批量移动相册" albums={moveAlbums} grouped={admin} users={users} actor={actor} owner={owner} disabled={moveAlbumsLoading} value={moveAlbum ?? ""} onChange={e => {
      if(e.target.value === "__create") { setCreateForSelection(true); return; }
      if(!e.target.value) return;
      const target = Number(e.target.value);
      setMoveAlbum(target);
    }}><option value="">移动到相册…</option><option value="__create">新建相册并移入…</option><option value="0">移至未分类</option></MoveAlbumSelect><ConfirmButton disabled={moveAlbum === null || !moveTarget || moveAlbumsLoading || !!moveAlbumsError || batchBusy || !!selectingGroup} confirmLabel={crossMove?"确认转移归属":"确认移入"} busyLabel="提交中…" resetKey={JSON.stringify([galleryKey,selectedKey,query,moveTarget?.id,moveTarget?.owner_id,moveTarget?.name,moveTarget?.displayed])} armKey={moveAlbum ?? undefined} description={moveDescription} onConfirm={()=>moveAlbum === null || !moveTarget ? undefined : batch("move_album", {album_id:moveAlbum,...(crossMove?{confirm:"TRANSFER_OWNER"}:{})})}>{crossMove?"转移归属":"移入相册"}</ConfirmButton></span><select className="select" aria-label="批量链接格式" value={linkFormat} onChange={(e) => setLinkFormat(e.target.value as "url" | "md")}><option value="url">直链</option><option value="md">Markdown</option></select><button className="btn" onClick={copySelected}>复制链接</button></fieldset>{actionFiles.some((file) => selected.has(file.id) && archiveEncrypted(file)) && <p className="batch-policy-note">加密原件仅支持临时发布，永久发布时会逐项跳过。</p>}{moveAlbumsLoading&&<span role="status">正在更新全部相册…</span>}{moveAlbumsError&&<span role="alert">{moveAlbumsError} <button className="btn" onClick={()=>void refreshMoveAlbums()}>重试读取相册</button></span>}{batchBusy && <span role="status">正在提交，请稍候…</span>}</div>}
    {conversionIDs&&!readOnly&&<BatchConversionDialog ids={conversionIDs} busy={batchBusy} onClose={()=>setConversionIDs(null)} onSubmit={(encrypted,ids)=>batch("convert",{encrypted,confirm:"CONVERT"},ids)}/>}
    {dateEditing&&!readOnly&&<BatchDateDialog count={dateEditing.ids.length} initial={dateEditing.initial} busy={batchBusy} onClose={()=>setDateEditing(null)} onSubmit={stamp=>batch("set_media_time",{media_at:stamp},dateEditing.ids)}/>}
    {createForSelection&&<CreateAlbum albums={albums} allowOwnerSelection={admin} onClose={()=>setCreateForSelection(false)} onCreated={async response=>{setCreateForSelection(false);await refreshAlbums();const ready=await refreshMoveAlbums();if(ready)setMoveAlbum(response.id);else toast("相册已创建，请重试读取相册后选择移动目标","err")}}/>}
    {detail && <DetailDrawer key={detail} id={detail} albums={albums} moveAlbums={moveAlbums} albumOwners={users} moveAlbumsLoading={moveAlbumsLoading} moveAlbumsError={moveAlbumsError} onRetryMoveAlbums={()=>void refreshMoveAlbums()} selected={selected.has(detail)} position={viewer.position} onToggle={file => { selectionCache.current.set(file.id, file); select(file.id); }} onClose={close} onChanged={filesUpdated} onTransferred={id=>{selectionCache.current.delete(id);setSelected(previous=>{const next=new Set(previous);next.delete(id);return next})}} conversionNotice={conversionNotices[detail]} onConverted={(nextID, notice, encrypted) => { const excluded=!!encryption&&typeof encrypted==="boolean"&&(encryption==="encrypted")!==encrypted; selectionCache.current.delete(detail); setSelected((previous) => { if (!previous.has(detail)) return previous; const next = new Set(previous); next.delete(detail); if(!excluded)next.add(nextID); return next; }); if (notice) setConversionNotices((previous) => ({ ...previous, [nextID]: notice })); setDetail(excluded?null:nextID); const nextParams = new URLSearchParams(params); if(excluded)nextParams.delete("file");else nextParams.set("file", nextID); setParams(nextParams, { replace: true }); filesUpdated(); }} onPrev={viewer.onPrev} onNext={viewer.onNext} />}
    {chronological && <TimeIndex key={`${query}|${grouping}`} groups={timeline.groups} grouping={grouping} current={timeline.visibleGroup} loading={timeline.loading} error={timeline.error} onRetry={timeline.retry} onJump={navigation.jump} />}
    {(admin || !readOnly) && !uploading && <PasteUpload onUpload={openUpload} />}
  </div>;
}

function PasteUpload({ onUpload }: { onUpload: (files: File[]) => void }) {
  const current = useRef(onUpload); current.current = onUpload;
  useEffect(() => { const paste = (event: ClipboardEvent) => {
    if ((event.target as HTMLElement)?.closest("input,textarea,[contenteditable=true],.drawer")) return;
    const files = Array.from(event.clipboardData?.items || []).filter((item) => item.kind === "file").map((item) => item.getAsFile()).filter((file): file is File => !!file);
    if (files.length) { event.preventDefault(); current.current(files); }
  }; window.addEventListener("paste", paste); return () => window.removeEventListener("paste", paste); }, []); return null;
}

function FileCard({ file: f, preview, selected, selectionMode, onVisible, onSelect, onOpen, onRetry, onPreviewError }: { file: FileItem; preview?: Preview; selected: boolean; selectionMode: boolean; onVisible: (id: string) => void; onSelect: (range: boolean) => void; onOpen: () => void; onRetry: () => void; onPreviewError: () => void }) {
  const card = useRef<HTMLElement>(null);
  useEffect(() => {
    if (!card.current) return;
    if (!window.IntersectionObserver) { onVisible(f.id); return; }
    const observer = new IntersectionObserver((entries) => { if (entries.some((entry) => entry.isIntersecting)) { onVisible(f.id); observer.disconnect(); } }, { rootMargin: "240px 0px" });
    observer.observe(card.current); return () => observer.disconnect();
  }, [f.id, onVisible]);
  const status = fileStatus(f);
  const failed = f.archive_state === "failed" || f.purge_state === "failed" || f.conversion?.state === "failed" || f.operation?.state === "failed";
  const ordinaryStatus = ["已发布", "已发布 · 首页", "未发布"].includes(status);
  return <article ref={card} data-file-id={f.id} data-selection-disabled={f.purge_state?"true":undefined} className={`fcard${selected ? " selected" : ""}`}>
    <button type="button" className="card-check" role="checkbox" aria-checked={selected} aria-label={`${selected ? "取消选中" : "选中"} ${f.name}`} disabled={!!f.purge_state} onClick={(event) => onSelect(event.shiftKey)}><span className="check" data-on={selected}><svg width="13" height="13" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true"><path d="m3 8 3 3 7-7" /></svg></span></button>
    <button type="button" className="card-open" onClick={event=>{if(selectionMode){if(!f.purge_state)onSelect(event.shiftKey)}else onOpen()}} aria-label={`${selectionMode?selected?"取消选中":"选中":"预览"} ${f.name}`} aria-pressed={selectionMode?selected:undefined}>
      <div className="media">
        {preview?.status === "ready" && preview.url && !preview.error ? <img src={preview.url} alt={f.name} loading="lazy" draggable={false} onError={onPreviewError} /> : <div className="nothumb"><span>{preview?.error || (f.archive_state === "failed" ? "归档失败" : f.archive_state !== "done" ? "归档中…" : f.has_thumb ? "准备预览…" : f.ext.toUpperCase())}</span></div>}
        <FileEncryption file={f} />
        {(!ordinaryStatus || failed) && <span className={`card-status${failed ? " failed" : ""}`} title={ordinaryStatus ? "操作失败" : status}>{ordinaryStatus ? "操作失败" : status}</span>}
        {f.kind === "video" && <span className="play-badge">视频 {fmtDur(f.duration)}</span>}
      </div>
    </button>
    {preview?.error && <button className="btn btn-sm card-retry" onClick={onRetry}>重试预览</button>}
  </article>;
}
