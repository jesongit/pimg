import FloatingFeedback from "../FloatingFeedback";
import {useEffect,useRef,useState} from "react";
import ConfirmButton from "./ConfirmButton";
import {createPortal} from "react-dom";
import {Link} from "react-router-dom";
import type {OperationItem,TaskItem} from "../api";
import {fmtTime} from "../api";
import {useGallery} from "./GalleryScope";
import {filesUpdated,useDialog} from "./interactions";
import {tasksUpdated} from "./TaskStatus";
import {fileUnavailableText,isFileReference,taskCancelMessage,taskState,taskSummary} from "./taskPresentation";
import {logMessage} from './logPresentation';

export function openOperationDetails(id:number) {
 window.dispatchEvent(new CustomEvent("pimg:open-operation",{detail:{id}}));
}

export function OperationDetailsHost() {
 const [id,setID]=useState<number|null>(null);
 useEffect(()=>{const open=(event:Event)=>{const value=Number((event as CustomEvent).detail?.id);if(Number.isSafeInteger(value)&&value>0)setID(value)};window.addEventListener("pimg:open-operation",open);return()=>window.removeEventListener("pimg:open-operation",open)},[]);
 return id?<OperationDialog key={id} id={id} onClose={()=>setID(null)}/>:null;
}

function OperationDialog({id,onClose}:{id:number;onClose:()=>void}) {
 const ref=useRef<HTMLDivElement>(null);useDialog(ref,onClose);
 return createPortal(<><div className="overlay operation-overlay" onClick={onClose}/><div ref={ref} className="modal operation-dialog" role="dialog" aria-modal="true" aria-labelledby="operation-title"><div className="dhead"><h2 id="operation-title">操作详情</h2><button type="button" className="btn btn-ghost" onClick={onClose}>关闭</button></div><OperationDetails id={id} onNavigate={onClose}/></div></>,document.body);
}

const itemState:Record<string,string>={queued:"等待处理",pending:"等待处理",running:"处理中",done:"已完成",succeeded:"已完成",failed:"失败",skipped:"已跳过",canceled:"已取消",submitted:"处理中",accepted:"已接收",archiving:"归档中"};

export default function OperationDetails({id,actions=true,refresh=0,onNavigate}:{id:number;actions?:boolean;refresh?:number;onNavigate?:()=>void}) {
 const {ownApi:api,admin,actor}=useGallery();
 const [operation,setOperation]=useState<TaskItem|null>(null),[items,setItems]=useState<OperationItem[]>([]),[filter,setFilter]=useState("failed"),[page,setPage]=useState(1),[next,setNext]=useState(0),[total,setTotal]=useState(0),[error,setError]=useState(""),[itemError,setItemError]=useState(""),[busy,setBusy]=useState(true),[acting,setActing]=useState(false),[revision,setRevision]=useState(0);
 const alive=useRef(true);
 const [actionNotice,setActionNotice]=useState(""),[actionError,setActionError]=useState("");
 useEffect(()=>{alive.current=true;return()=>{alive.current=false}},[]);
 useEffect(()=>{
  const controller=new AbortController();let stopped=false,timer:ReturnType<typeof setTimeout>|undefined,inFlight=false,queued=false;
  setBusy(true);setError("");setItemError("");
  const load=async()=>{
   if(stopped)return;if(inFlight){queued=true;return}clearTimeout(timer);inFlight=true;
   const result=await Promise.allSettled([api.get<{operation:TaskItem}>(`/operations/${id}`,controller.signal),api.get<{items:OperationItem[];total:number;next_page:number}>(`/operations/${id}/items?state=${filter}&page=${page}`,controller.signal)]);
   if(stopped)return;
   let active=false;
   if(result[0].status==="fulfilled"){const value=result[0].value.operation;setOperation(value);setError("");active=["pending","running"].includes(value.state)||["open","receiving"].includes(value.group?.input_state||"")}
   else if(!controller.signal.aborted)setError(result[0].reason.message||"操作详情读取失败，请重试。");
   if(result[1].status==="fulfilled"){const data=result[1].value;setItems(data.items||[]);setNext(data.next_page||0);setTotal(data.total||0);setItemError("")}
   else if(!controller.signal.aborted)setItemError(result[1].reason.message||"文件明细读取失败，请重试。");
   setBusy(false);inFlight=false;
   if(queued){queued=false;void load()}else if(active)timer=setTimeout(()=>{if(!document.hidden)void load()},3000);
  };
  const visible=()=>{if(!document.hidden){clearTimeout(timer);void load()}};
  const updated=()=>void load();
  void load();document.addEventListener("visibilitychange",visible);window.addEventListener("pimg:tasks-updated",updated);
  return()=>{stopped=true;controller.abort();clearTimeout(timer);document.removeEventListener("visibilitychange",visible);window.removeEventListener("pimg:tasks-updated",updated)};
 },[api,id,filter,page,revision,refresh]);
 const act=async(action:"retry"|"cancel")=>{
  if(acting)return;setActing(true);setActionNotice("");setActionError("");
  try{await api.post(`/operations/${id}/${action}`,{});if(!alive.current)return;tasksUpdated();filesUpdated();setActionNotice(action==="retry"?"已重新提交未完成项":"已请求停止剩余操作")}
  catch(cause:any){if(alive.current)setActionError(cause.message||"提交失败，请重试。")}
  finally{if(alive.current)setActing(false)}
 };
 const group=operation?.group,totalFiles=group?.total_files||0,processed=group?.processed_files||0;
 const canRetry=operation&&(operation.can_retry??group?.can_retry??["failed","canceled"].includes(operation.state));
 const canCancel=operation&&(operation.can_cancel??group?.can_cancel??(["pending","running"].includes(operation.state)&&!operation.cancel_requested));
 return <section className="operation-details" aria-label={`操作 ${id} 的处理结果`} aria-busy={busy}>
  {error&&<FloatingFeedback className="detail-error" role="alert">{error}<button type="button" className="btn btn-sm" onClick={()=>setRevision(value=>value+1)}>重新读取</button></FloatingFeedback>}
  {!operation&&!error&&<p role="status">正在读取操作…</p>}
  {operation&&<>
   <div className="operation-heading"><strong>{logMessage({...operation,record:'task'})}</strong><span className={`chip ${operation.state==="failed"?"chip-err":operation.state==="done"?"chip-ok":"chip-warn"}`}>{taskState(operation)}</span><small>操作 #{operation.id}</small></div>
   {taskSummary(operation)&&<p className="operation-summary">{taskSummary(operation)}</p>}
   {["pending","running"].includes(operation.state)&&totalFiles>0&&!["scanning","ready","paused"].includes(group?.stage||"")&&<div className="task-group-progress"><span>{processed} / {totalFiles} 项</span><progress aria-label="操作处理进度" max={totalFiles} value={processed}/></div>}
   {operation.last_error&&<p className="detail-error">{operation.last_error}</p>}
   {actionNotice&&<FloatingFeedback className="detail-hint" role="status">{actionNotice}</FloatingFeedback>}{actionError&&<FloatingFeedback className="detail-error" role="alert">{actionError}</FloatingFeedback>}
   {actions&&<div className="operation-actions">{canRetry&&<button type="button" className="btn btn-sm" disabled={acting} onClick={()=>void act("retry")}>{acting?"提交中…":operation.state==="canceled"?"继续剩余项":"仅重试失败项"}</button>}{canCancel&&<ConfirmButton className="btn btn-sm btn-ghost" disabled={acting} confirmLabel="确认取消" resetKey={id} description={taskCancelMessage(operation)} onConfirm={()=>act("cancel")}>取消剩余操作</ConfirmButton>}<button type="button" className="btn btn-sm btn-ghost" disabled={busy} onClick={()=>setRevision(value=>value+1)}>刷新</button></div>}
   <div className="operation-item-controls" role="group" aria-label="文件明细范围"><button type="button" className={`btn btn-sm ${filter==="failed"?"":"btn-ghost"}`} aria-pressed={filter==="failed"} onClick={()=>{setFilter("failed");setPage(1)}}>失败项{group?.failed_files?`（${group.failed_files}）`:""}</button><button type="button" className={`btn btn-sm ${filter==="all"?"":"btn-ghost"}`} aria-pressed={filter==="all"} onClick={()=>{setFilter("all");setPage(1)}}>全部文件</button><small>{total} 项</small></div>
   {itemError&&<FloatingFeedback className="detail-error" role="alert">{itemError}<button type="button" className="btn btn-sm" onClick={()=>setRevision(value=>value+1)}>重试读取明细</button></FloatingFeedback>}
   {!busy&&!itemError&&!items.length&&<p className="detail-hint">{filter==="failed"?"当前没有失败项。":"尚未接收文件。"}</p>}
   <ul className="operation-items">{items.map(item=>{const file=isFileReference(item.file_id),query=new URLSearchParams({file:item.file_successor?.id||item.file_id});const owner=item.file_successor?.owner_id||item.owner_id||operation.owner_id;if(admin&&owner&&owner!==actor.owner_id)query.set("owner_id",owner);return <li key={item.key}><div><strong>{item.file_name||"未命名文件"}</strong>{item.file_successor&&<small>{item.file_successor.kind==="transferred"?"已转移归属，原件保留":"已完成加密转换"}</small>}{file&&(item.file_exists===true||item.file_successor)?<Link to={`${admin?"/admin":"/library"}?${query}`} onClick={onNavigate}>{item.file_successor?"查看新位置":"查看文件"}</Link>:file&&item.file_exists===false?<small>{fileUnavailableText}</small>:null}</div><span>{itemState[item.state]||item.state}</span>{item.error&&<p>{item.error}</p>}</li>})}</ul>
   {(page>1||next>0)&&<nav className="log-paging" aria-label="文件明细分页"><span>第 {page} 页</span><button type="button" className="btn btn-sm" disabled={busy||page===1} onClick={()=>setPage(value=>value-1)}>上一页</button><button type="button" className="btn btn-sm" disabled={busy||!next} onClick={()=>setPage(next)}>下一页</button></nav>}
   <details className="activity-technical"><summary>技术详情</summary><p>创建时间：{fmtTime(operation.created_at)} · 最近更新：{fmtTime(operation.updated_at)}</p><pre>{JSON.stringify({operation_id:id,task_id:operation.id,attempts:operation.attempts,max_attempts:operation.max_attempts,result:operation.result},null,2)}</pre></details>
  </>}
 </section>;
}
