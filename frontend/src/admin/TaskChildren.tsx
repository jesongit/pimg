import {useEffect,useState} from "react";
import type {ReactNode} from "react";
import {api} from "../api";
import type {TaskItem} from "../api";
import {fileUnavailableText,RelatedContent,taskHasFile,taskState,typeNames} from "./taskPresentation";
import OperationDetails from "./OperationDetails";

export default function TaskChildren({id,operationID,active=false,refresh=0,actions=false}:{id:number;operationID?:number;active?:boolean;refresh?:number;actions?:boolean}) {
 return operationID?<OperationDetails id={operationID} actions={actions} refresh={refresh}/>:<LegacySteps key={id} id={id} active={active} refresh={refresh}/>;
}

function LegacySteps({id,active,refresh}:{id:number;active:boolean;refresh:number}) {
 const [items,setItems]=useState<TaskItem[]>([]),[page,setPage]=useState(1),[next,setNext]=useState(0),[error,setError]=useState(""),[busy,setBusy]=useState(true),[revision,setRevision]=useState(0);
 useEffect(()=>{
  const controller=new AbortController();let stopped=false,inFlight=false,timer:ReturnType<typeof setTimeout>|undefined;
  setBusy(true);setError("");
  const load=async()=>{if(stopped||inFlight)return;inFlight=true;try{const data=await api.get<{items:TaskItem[];next_page:number}>(`/api/admin/tasks/${id}/children?page=${page}`,controller.signal);if(!stopped){setItems(data.items||[]);setNext(data.next_page||0);setError("")}}catch(cause:any){if(!stopped&&!controller.signal.aborted)setError(cause.message)}finally{inFlight=false;if(!stopped){setBusy(false);if(active)timer=setTimeout(()=>{if(!document.hidden)void load()},3000)}}};
  const visible=()=>{if(!document.hidden){clearTimeout(timer);void load()}};
  void load();document.addEventListener("visibilitychange",visible);
  return()=>{stopped=true;controller.abort();clearTimeout(timer);document.removeEventListener("visibilitychange",visible)};
 },[id,page,active,refresh,revision]);
 const ordered=[...items].sort((a,b)=>Number(b.state==="failed")-Number(a.state==="failed")||a.id-b.id);
 return <div className="task-steps" aria-label="任务步骤" aria-busy={busy}>
  {error&&<p role="alert">{error}<button type="button" className="btn btn-sm" onClick={()=>setRevision(value=>value+1)}>重新读取</button></p>}
  {busy&&!items.length&&<p role="status">正在读取处理明细…</p>}
  <ul>{ordered.map(item=>{
    const action=typeNames[item.type]||item.type,name=item.file_name?.trim(),hasName=!!name&&name!==action,file=taskHasFile(item),details:ReactNode[]=[];
    if(hasName)details.push(action);
    if(file&&item.file_exists===false)details.push(fileUnavailableText);
    else if(file&&item.file_exists===true)details.push(<RelatedContent id={item.file_id} owner={item.owner_id} label="查看文件"/>);
    else if(/^tg-album:[1-9]\d*$/.test(item.file_id||""))details.push(<RelatedContent id={item.file_id} owner={item.owner_id}/>);
    return <li key={item.id}><span><strong>{hasName?name:action}</strong>{details.length>0&&<small>{details.map((detail,index)=><span key={index}>{index>0?" · ":""}{detail}</span>)}</small>}</span><span>{taskState(item)}</span>{item.last_error&&<p>{item.last_error}</p>}</li>;
  })}</ul>
  {(page>1||next>0)&&<nav className="log-paging" aria-label="处理明细分页"><span>第 {page} 页</span><button type="button" className="btn btn-sm" disabled={busy||page===1} onClick={()=>setPage(value=>value-1)}>上一页</button><button type="button" className="btn btn-sm" disabled={busy||!next} onClick={()=>setPage(next)}>下一页</button></nav>}
 </div>;
}
