import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useGallery } from "./GalleryScope";
import type { AdminSettings, FileItem, TaskItem } from "../api";
import { useToast } from "../App";
import { filesUpdated } from "./interactions";
import { clientID } from "../clientID";
import {openOperationDetails,OperationDetailsHost} from "./OperationDetails";
import {tasksUpdated} from "./TaskStatus";

export interface UploadJob {
  key: string; name: string; progress: number; fileID?: string; albumID?: number;
  operationKey?: string; operationID?: number;
  direct: boolean; publish?: boolean; state: "queued" | "uploading" | "processing" | "archiving" | "publishing" | "done" | "failed"; error?: string;
}
interface UploadState {
  jobs: UploadJob[]; enqueue: (files: File[], direct: boolean, albumID: number) => void;
  retry: (job: UploadJob) => Promise<void>; clear: () => void; policy: Partial<AdminSettings>;
}
type UploadResult = Partial<FileItem> & { duplicate?: boolean };
type UploadBatch = {key:string;total:number;completed:number;rejected:number;operationID?:number;taskID?:number;sealed:boolean;finished?:boolean;issues:{label:string;description:string}[]};
function receivedJob(job: UploadJob, file: UploadResult, key: string, index: number, count: number): UploadJob {
  const error = file.error || (file.archive_state === "failed" ? file.archive_error || "归档失败，请重试。" : (job.publish ?? job.direct) && !file.published && (file.publish_error || file.operation?.state === "failed") ? file.publish_error || file.operation?.error || "发布失败，请重试。" : undefined);
  return {
    ...job, key, name: file.name || file.orig_name || (count > 1 ? `${job.name} · 第 ${index + 1} 段` : job.name),
    fileID: file.id, progress: 1,
    state: error || !file.id ? "failed" : file.archive_state === "done" ? (job.publish ?? job.direct) && !file.published ? "publishing" : "done" : "archiving",
    error: error || (!file.id ? "服务器未返回此文件的归档编号，请重新选择文件上传。" : undefined),
  };
}
const UploadContext = createContext<UploadState>(null!);
const savedJobs = (key: string) => {
  try { return (JSON.parse(sessionStorage.getItem(key) || "[]") as UploadJob[]).filter(job=>job.state!=="done").map((job) => job.state === "uploading" || job.state === "processing" || job.state === "queued" ? { ...job, state: "failed" as const, error: "页面已刷新，请重新选择此文件上传。" } : job); }
  catch { return []; }
};
export function UploadProvider({ children }: { children: ReactNode }) {
  const toast = useToast();
  const { ownApi: api, actor, admin } = useGallery();
  const memoryKey = `pimg:uploads:${actor.owner_id}`;
  const [jobs, setJobs] = useState<UploadJob[]>(() => savedJobs(memoryKey));
  const [policy, setPolicy] = useState<Partial<AdminSettings>>({});
  const originals = useRef(new Map<string, File>());
  const queue = useRef<UploadJob[]>([]);
  const processing = useRef(false);
  const batches = useRef(new Map<string,UploadBatch>());
  const update = useCallback((key: string, patch: Partial<UploadJob>) => setJobs((previous) => previous.map((job) => job.key === key ? { ...job, ...patch } : job)), []);
  useEffect(() => { try { sessionStorage.setItem(memoryKey, JSON.stringify(jobs.filter(job=>job.state!=="done"))); } catch {} }, [jobs, memoryKey]);
  useEffect(() => {
    const load = () => admin ? api.get<AdminSettings>("/settings").then(p=>setPolicy({...p,allowed_exts:p.effective_allowed_exts?.join(",") ?? p.allowed_exts})).catch(() => {}) : api.get<{upload_max_mb:number;allowed_exts:string[]}>("/me").then(p=>setPolicy({upload_max_mb:p.upload_max_mb,allowed_exts:p.allowed_exts.join(",")})).catch(()=>{});
    load(); window.addEventListener("pimg:policy-updated", load);
    return () => window.removeEventListener("pimg:policy-updated", load);
  }, [api,admin]);
  const runQueue = useCallback(async () => {
    if (processing.current) return;
    processing.current = true;
    try {
      while (queue.current.length) {
        const job = queue.current.shift()!;
        const batch = job.operationKey ? batches.current.get(job.operationKey) : undefined;
        const notification = {key:job.operationKey?`upload:${job.operationKey}`:undefined,persistent:true,onAction:batch?.operationID?()=>openOperationDetails(batch.operationID!):undefined,actionLabel:"查看上传详情"};
        if(batch&&!batch.operationID){
          try{
            const accepted=await api.post<{operation_id:number;task_id:number}>("/operations",{key:batch.key,kind:"upload",total_files:batch.total,album_id:job.albumID||0,encrypted:!job.direct});
            batch.operationID=accepted.operation_id;batch.taskID=accepted.task_id;
            for(const queued of queue.current)if(queued.operationKey===batch.key)queued.operationID=accepted.operation_id;
            setJobs(previous=>previous.map(value=>value.operationKey===batch.key?{...value,operationID:accepted.operation_id}:value));
            tasksUpdated();
          }catch(cause:any){
            queue.current=queue.current.filter(value=>value.operationKey!==batch.key);
            setJobs(previous=>previous.map(value=>value.operationKey===batch.key?{...value,state:"failed",error:cause.message}:value));
            toast("上传未开始","err",{...notification,description:cause.message||"请重新选择文件重试。",details:[{label:"文件尚未发送",description:"请重新选择文件上传，服务器尚未接收本次内容。"}]});
            continue;
          }
        }
        if(batch?.operationID){job.operationID=batch.operationID;notification.onAction=()=>openOperationDetails(batch.operationID!)}
        const file = originals.current.get(job.key);
        if (!file) { update(job.key, { state: "failed", error: "请重新选择此文件上传。" }); batch?.issues.push({label:job.name,description:"请重新选择此文件上传。"}); }
        else {
        update(job.key, { state: "uploading", error: undefined, progress: 0 });
        const ordinal=(batch?.completed||0)+1,total=batch?.total||1;
        toast("正在上传","ok",{...notification,description:`第 ${ordinal} / ${total} 个文件`,progress:(ordinal-1)/total,progressLabel:`${ordinal} / ${total}`,details:batch?.issues.length?batch.issues:undefined});
        const form = new FormData(); form.append("files", file, file.name);
        const params = new URLSearchParams();
        params.set("encrypt", String(!job.direct));
        if (job.albumID) params.set("album_id", String(job.albumID));
        if(job.operationID){params.set("operation_id",String(job.operationID));params.set("item_key",job.key)}
        try {
          let lastPercent=-1;
          const response = await api.upload<{ files?: UploadResult[]; error?: string }>(`/upload?${params}`, form, (progress) => {update(job.key, { progress, state: progress >= 1 ? "processing" : "uploading" });const percent=Math.floor(progress*10)*10;if(percent!==lastPercent){lastPercent=percent;toast(progress>=1?"正在接收文件":"正在上传","ok",{...notification,description:`第 ${ordinal} / ${total} 个文件`,progress:progress>=1?null:(ordinal-1+progress)/total,progressLabel:progress>=1?`${ordinal} / ${total}`:`${ordinal} / ${total} · ${percent}%`,details:batch?.issues.length?batch.issues:undefined})}});
          if (!response.files?.length) throw new Error(response.error || "服务器未返回归档记录，请重试。");
          const received = response.files.map((file, index) => receivedJob(job, file, index === 0 ? job.key : clientID(), index, response.files!.length));
          setJobs(previous => previous.flatMap(current => current.key === job.key ? received : [current]));
          const failed = received.filter(part => part.state === "failed");
          if (failed.length) batch?.issues.push(...failed.map(part=>({label:part.name,description:part.error||"接收失败"})));
          // Each part retries its own server record. Do not retain the large input
          // for sibling jobs or replay successful parts after a partial failure.
          if (received.length > 1 || received.some(part => !!part.fileID)) originals.current.delete(job.key);
          if (received.some(part => !!part.fileID)) filesUpdated();
        } catch (error: any) {
          const message = error.message || "上传失败，请重新选择文件重试。"; update(job.key, { state: "failed", error: message });batch?.issues.push({label:job.name,description:message});
          if(error.status===409&&batch){const remaining=queue.current.filter(value=>value.operationKey===batch.key);queue.current=queue.current.filter(value=>value.operationKey!==batch.key);setJobs(previous=>previous.filter(value=>!remaining.some(item=>item.key===value.key)))}
        }
        }
        if(batch)batch.completed++;
        if(batch?.operationID&&!queue.current.some(value=>value.operationKey===batch.key)&&!batch.sealed){
          try{await api.post(`/operations/${batch.operationID}/seal`,{});batch.sealed=true;tasksUpdated();if(!batch.finished)toast(batch.issues.length?"部分文件需要处理":"正在完成归档",batch.issues.length?"err":"ok",{...notification,description:batch.issues.length?`${batch.issues.length} 项未处理，可查看原因`:`本次提交 ${batch.total} 个文件`,progress:batch.issues.length?undefined:null,details:batch.issues.length?batch.issues:undefined})}
          catch(cause:any){if(!batch.finished)toast("提交确认未完成","err",{...notification,description:cause.message||"文件可能已接收，请查看上传详情。"})}
        }
      }
    } finally { processing.current = false; }
  }, [update, toast, api]);
  const enqueue = (files: File[], direct: boolean, albumID: number) => {
    if (!Number.isSafeInteger(albumID) || albumID < 0) { toast("请选择目标相册", "err"); return; }
    const allowed = policy.allowed_exts?.split(",").map((ext) => ext.trim().replace(/^\./, "").toLowerCase()).filter(Boolean);
    const operationKey=clientID();
    const next = files.map((file): UploadJob => {
      const job: UploadJob = { key: clientID(), operationKey, name: file.name, progress: 0, state: "queued", albumID, direct, publish: false };
      const ext = file.name.split(".").pop()?.toLowerCase();
      if (policy.upload_max_mb && file.size > policy.upload_max_mb * 1024 * 1024) { job.state = "failed"; job.error = `超过单文件上限 ${policy.upload_max_mb} MB。`; }
      else if (allowed?.length && !allowed.includes(ext || "")) { job.state = "failed"; job.error = "不支持此文件格式，请检查上传策略。"; }
      else { originals.current.set(job.key, file); queue.current.push(job); }
      return job;
    });
    const rejected = next.filter(job => job.state === "failed");
    const valid=next.length-rejected.length;
    if(valid)batches.current.set(operationKey,{key:operationKey,total:valid,completed:0,rejected:rejected.length,sealed:false,issues:rejected.map(job=>({label:job.name,description:job.error||"无法上传"}))});
    toast(valid?"等待上传":"文件无法上传",rejected.length?"err":"ok",{key:`upload:${operationKey}`,description:valid?`${valid} 个文件${rejected.length?`，${rejected.length} 个未提交`:""}`:`${rejected.length} 个文件不符合上传要求`,persistent:true,...(valid?{progress:null}:{}),details:rejected.length?rejected.map(job=>({label:job.name,description:job.error||"无法上传"})):undefined});
    setJobs((previous) => [...previous, ...next]); void runQueue();
  };
  const retry = async (job: UploadJob) => {
    if (job.fileID) {
      const file = await api.get<FileItem>(`/files/${job.fileID}`);
	  if (file.archive_state === "done" && !(job.publish ?? job.direct)) { update(job.key, {state:"done", error:undefined}); filesUpdated(); return; }
      await api.post(`/files/${job.fileID}/${file.archive_state === "done" && (job.publish ?? job.direct) ? "publish" : "retry"}`);
      update(job.key, { state: "archiving", error: undefined }); filesUpdated();
    } else if (originals.current.has(job.key)) {
      update(job.key, { state: "queued", error: undefined }); queue.current.push(job); void runQueue();
    } else throw new Error("请重新选择此文件上传。");
  };
  const operationIDs=[...new Set(jobs.map(job=>job.operationID).filter((id):id is number=>!!id))];
  const operationKey=operationIDs.join(",");
  useEffect(()=>{
    if(!operationKey)return;let stopped=false,timer:ReturnType<typeof setTimeout>;const controller=new AbortController();
    const poll=async()=>{
      await Promise.all(operationIDs.map(async id=>{try{
        const {operation}=await api.get<{operation:TaskItem}>(`/operations/${id}`,controller.signal);if(stopped)return;
        const batchJob=jobs.find(job=>job.operationID===id),key=batchJob?.operationKey||String(id),batch=batches.current.get(key);
        const open=["open","receiving"].includes(operation.group?.input_state||"");
        const terminal=["done","failed","canceled"].includes(operation.state)&&!open;
        // Native upload progress owns this notification while a file is being sent.
        const group=operation.group,total=group?.total_files||batch?.total||0,processed=group?.processed_files||0,failed=Math.max((group?.failed_files||0)+(batch?.rejected||0),batch?.issues.length||0),skipped=group?.skipped_files||0,succeeded=group?.succeeded_files||0;
        const title=terminal?operation.state==="canceled"?"上传已停止":operation.state==="failed"||failed?"上传有未处理项":"上传完成":open?"正在接收文件":operation.state==="pending"?"等待归档":"正在归档";
        const description=terminal?[succeeded&&`成功 ${succeeded} 个`,skipped&&`跳过 ${skipped} 个`,failed&&`${failed} 个需处理`,operation.state==="canceled"&&"已接收文件继续归档"].filter(Boolean).join(" · ")||`本次 ${total} 个文件`:`已处理 ${processed} / ${total} 个`;
        if(!processing.current||!open||terminal)toast(title,operation.state==="failed"||failed>0?"err":"ok",{key:`upload:${key}`,description,persistent:!terminal||operation.state!=="done"||failed>0,...(!terminal?{progress:total?Math.min(1,processed/total):null,progressLabel:`${processed} / ${total}`} : {}),onAction:()=>openOperationDetails(id),actionLabel:"查看上传详情",details:batch?.issues.length?batch.issues:undefined});
        if(terminal){if(batch)batch.finished=true;if(operation.state==="canceled")queue.current=queue.current.filter(job=>job.operationID!==id);setJobs(previous=>previous.filter(job=>job.operationID!==id));batches.current.delete(key);filesUpdated();tasksUpdated()}
      }catch{/* Keep the current operation visible; detail disclosure exposes retry. */}}));
      if(!stopped)timer=setTimeout(()=>{if(!document.hidden)void poll()},2500);
    };
    const visible=()=>{if(!document.hidden){clearTimeout(timer);void poll()}};void poll();document.addEventListener("visibilitychange",visible);
    return()=>{stopped=true;controller.abort();clearTimeout(timer);document.removeEventListener("visibilitychange",visible)};
  },[operationKey,api,toast]);
  const pending = jobs.filter((job) => !job.operationID && job.fileID && (job.state === "archiving" || job.state === "publishing"));
  const pendingKey = pending.map((job) => job.key).join(",");
  useEffect(() => {
    if (!pendingKey) return;
    let stopped = false, timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      let changed = false;
      await Promise.all(pending.map(async (job) => {
        try {
          const f = await api.get<FileItem>(`/files/${job.fileID}`);
          if (stopped) return;
          if (f.archive_state === "failed" || (job.publish ?? job.direct) && !f.published && (f.publish_error || f.operation?.state === "failed")) { const message = f.archive_error || f.publish_error || f.operation?.error || "归档或发布失败，请在任务页重试。"; update(job.key, { state: "failed", error: message }); toast("文件归档未完成", "err",{key:`upload-file:${job.key}`,description:job.name,details:[{label:job.name,description:message}]}); changed = true; }
          else if (f.archive_state === "done" && (!(job.publish ?? job.direct) || f.published)) { update(job.key, { state: "done", error: undefined }); changed = true; }
          else if (f.archive_state === "done" && job.state !== "publishing") update(job.key, { state: "publishing" });
        } catch (error: any) { if (!stopped && error.status === 404) { update(job.key, { state: "failed", error: "归档记录已不存在。" }); toast("归档记录已不存在", "err",{key:`upload-file:${job.key}`,description:job.name}); changed = true; } }
      }));
      if (changed) filesUpdated();
      if (!stopped) timer = setTimeout(poll, 1500);
    };
    void poll(); return () => { stopped = true; clearTimeout(timer); };
  }, [pendingKey, update, toast, api]);
  return <UploadContext.Provider value={{ jobs, enqueue, retry, clear: () => setJobs((previous) => previous.filter((job) => job.state !== "done")), policy }}>{children}<OperationDetailsHost/></UploadContext.Provider>;
}
export function useUploads() { return useContext(UploadContext); }
