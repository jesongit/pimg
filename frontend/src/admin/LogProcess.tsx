import FloatingFeedback from "../FloatingFeedback";
import {useEffect,useState} from "react";
import {api,fmtTime} from "../api";
import type {TaskItem} from "../api";
import {taskState,typeNames} from "./taskPresentation";
import {Link} from "react-router-dom";

interface IntakeEvent{id:number;type:string;reason:string;file_id:string;tool_file_id:string;upload_id:string;created_at:number;details:Record<string,unknown>}
const names:Record<string,string>={accepted:"已接收",adopted:"已接管入库",verified:"恢复校验通过",duplicate:"重复，沿用已有文件",failed:"接收或校验失败",retired_ignored:"已忽略旧身份"};
export default function LogProcess({id,fileID,refresh=0}:{id:number;fileID:string;refresh?:number}){
 const [data,setData]=useState<{events:IntakeEvent[];tasks:TaskItem[]}|null>(null),[error,setError]=useState(""),[revision,setRevision]=useState(0);
 useEffect(()=>{const controller=new AbortController();setError("");api.get<{events:IntakeEvent[];tasks:TaskItem[]}>(`/api/admin/logs/receipts/${id}`,controller.signal).then(value=>{if(!controller.signal.aborted)setData(value)}).catch(e=>{if(!controller.signal.aborted)setError(e.message)});return()=>controller.abort()},[id,revision,refresh]);
 if(error)return <FloatingFeedback role="alert">{error}<button className="btn btn-sm" onClick={()=>setRevision(v=>v+1)}>重试</button></FloatingFeedback>;
 if(!data)return <p role="status">正在读取处理过程…</p>;
 const stages=[...data.events.map(event=>{const label=names[event.type]||event.type;return {key:`e${event.id}`,at:event.created_at,label,detail:event.reason?.trim()===label?"":event.reason}}),...data.tasks.map(task=>({key:`t${task.id}`,at:task.created_at,label:`任务 #${task.id} · ${typeNames[task.type]||task.type} · ${taskState(task)}`,detail:task.last_error}))].sort((a,b)=>a.at-b.at);
 return <div className="log-process"><ol className="activity-stages">{stages.map(stage=><li key={stage.key}><time>{fmtTime(stage.at)}</time><div>{stage.label}{stage.detail&&<p>{stage.detail}</p>}</div></li>)}</ol>{fileID&&<Link to={`/admin/logs?file_id=${encodeURIComponent(fileID)}`}>查看运行记录</Link>}<details className="activity-technical"><summary>来源字段</summary><pre>{JSON.stringify(data.events.map(event=>({event_id:event.id,type:event.type,tool_file_id:event.tool_file_id||undefined,upload_id:event.upload_id||undefined,...event.details})),null,2)}</pre></details></div>;
}
