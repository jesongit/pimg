import {useEffect,useRef,useState} from "react";
import type {FormEvent} from "react";
import {api} from "../api";
import {useUnsavedChanges} from "./interactions";
import "./system-schedules.css";

type SystemSchedule={id:string;name:string;description:string;enabled:boolean;cron:string;timezone:string;next_run:number;state:"scheduled"|"disabled"|"running";last_run:number;last_status:""|"done"|"failed"|"interrupted";last_error:string;last_result:Record<string,unknown>};
type ScheduleDraft=Pick<SystemSchedule,"enabled"|"cron"|"timezone">;
const examples=[{cron:"0 6 * * *",label:"每天 06:00"},{cron:"0 6 * * 1",label:"每周一 06:00"},{cron:"*/5 * * * *",label:"每 5 分钟"}];
const zoneName=(zone:string)=>zone==="Asia/Shanghai"?"北京时间":zone==="UTC"?"UTC":zone;
function ruleText(schedule:SystemSchedule){
 const [minute,hour,day,month,week]=schedule.cron.trim().split(/\s+/);
 if(day==="*"&&month==="*"){
  if(minute==="*"&&hour==="*"&&week==="*")return "每分钟";
  if(/^\*\/\d+$/.test(minute)&&hour==="*"&&week==="*"&&Number(minute.slice(2))>0&&60%Number(minute.slice(2))===0)return `每 ${Number(minute.slice(2))} 分钟`;
  if(/^\d+$/.test(minute)&&/^\d+$/.test(hour)){
   const time=`${hour.padStart(2,"0")}:${minute.padStart(2,"0")}`;
   if(week==="*")return `每天 ${time}`;
   if(/^[0-7]$/.test(week))return `每周${["日","一","二","三","四","五","六","日"][Number(week)]} ${time}`;
  }
 }
 return `Cron ${schedule.cron}`;
}
function timeText(value:number,zone:string){
 if(!value)return "";
 try{return new Intl.DateTimeFormat("zh-CN",{timeZone:zone,year:"numeric",month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",hourCycle:"h23"}).format(value*1000)}catch{return new Date(value*1000).toISOString()}
}
function resultText(schedule:SystemSchedule){
 if(schedule.state==="running")return "正在运行";
 if(!schedule.last_run)return "尚未运行";
 return ({done:"上次已完成",failed:"上次失败",interrupted:"上次运行被中断"} as Record<string,string>)[schedule.last_status]||"上次运行";
}

function ScheduleEditor({schedule,onSave,onClose}:{schedule:SystemSchedule;onSave:(schedule:SystemSchedule)=>void;onClose:()=>void}){
 const [draft,setDraft]=useState<ScheduleDraft>({enabled:schedule.enabled,cron:schedule.cron,timezone:schedule.timezone}),[saving,setSaving]=useState(false),[error,setError]=useState("");
 const initial=useRef(draft),input=useRef<HTMLInputElement>(null),dirty=Object.keys(draft).some(key=>draft[key as keyof ScheduleDraft]!==initial.current[key as keyof ScheduleDraft]);
 useUnsavedChanges(dirty);
 useEffect(()=>{input.current?.focus()},[]);
 const update=(value:Partial<ScheduleDraft>)=>{setDraft(current=>({...current,...value}));setError("")};
 const save=async(event:FormEvent)=>{
  event.preventDefault();if(saving)return;
  const cron=draft.cron.trim().replace(/\s+/g," "),timezone=draft.timezone.trim();
  if(cron.split(" ").length!==5){setError("Cron 需要 5 个字段：分、时、日、月、周。");input.current?.focus();return}
  setSaving(true);setError("");
  try{const data=await api.patch<{schedule:SystemSchedule}>(`/api/admin/schedules/${encodeURIComponent(schedule.id)}`,{...draft,cron,timezone});onSave(data.schedule)}catch(e:any){setError(e.message||"保存失败，请重试。")}finally{setSaving(false)}
 };
 return <form className="schedule-editor" onSubmit={save} aria-label={`编辑${schedule.name}`}>
  <fieldset disabled={saving}>
   <div className="schedule-editor-fields"><label htmlFor={`schedule-cron-${schedule.id}`}>Cron 规则<input ref={input} id={`schedule-cron-${schedule.id}`} className="input" value={draft.cron} required maxLength={128} autoComplete="off" spellCheck={false} aria-describedby={`schedule-help-${schedule.id}`} onChange={event=>update({cron:event.target.value})}/></label>
    <label htmlFor={`schedule-zone-${schedule.id}`}>时区<input id={`schedule-zone-${schedule.id}`} className="input" value={draft.timezone} required maxLength={100} autoComplete="off" spellCheck={false} list="schedule-timezones" onChange={event=>update({timezone:event.target.value})}/></label>
    <label className="schedule-enabled"><input type="checkbox" checked={draft.enabled} onChange={event=>update({enabled:event.target.checked})}/>启用计划</label>
   </div>
   <p id={`schedule-help-${schedule.id}`} className="schedule-help">五个字段依次为分、时、日、月、周。时区使用 IANA 名称，如 Asia/Shanghai（北京时间）或 UTC。</p>
   <div className="schedule-examples"><span>常用规则</span>{examples.map(example=><button key={example.cron} type="button" className="text-button" onClick={()=>update({cron:example.cron})} title={example.cron}>{example.label}</button>)}</div>
   <p className="schedule-help">保存后会显示下一次执行时间，不会立即运行。停用或改规则不会中断当前运行。</p>
   {error&&<p className="schedule-error" role="alert">{error}</p>}
   <div className="schedule-editor-actions"><button className="btn btn-sm" type="submit" disabled={!dirty}>{saving?"保存中…":"保存计划"}</button><button className="btn btn-sm btn-ghost" type="button" onClick={onClose}>取消编辑</button></div>
  </fieldset>
 </form>;
}

export default function SystemSchedules(){
 const initial=api.cached<{items:SystemSchedule[]}>("/api/admin/schedules");
 const [items,setItems]=useState<SystemSchedule[]>(initial?.items||[]),[loading,setLoading]=useState(!initial),[error,setError]=useState(""),[editing,setEditing]=useState<string|null>(null),[revision,setRevision]=useState(0),[feedback,setFeedback]=useState("");
 const section=useRef<HTMLElement>(null);
 useEffect(()=>{
  const controller=new AbortController();let stopped=false,inFlight=false;
  setLoading(!api.cached("/api/admin/schedules"));
  const load=async()=>{if(inFlight)return;inFlight=true;try{const data=await api.get<{items:SystemSchedule[]}>("/api/admin/schedules",controller.signal);if(!stopped){setItems(data.items);setError("")}}catch(e:any){if(!stopped&&!controller.signal.aborted)setError(e.message||"系统计划读取失败")}finally{inFlight=false;if(!stopped)setLoading(false)}};
  void load();const timer=setInterval(()=>{if(!document.hidden)void load()},10000),visible=()=>{if(!document.hidden)void load()};document.addEventListener("visibilitychange",visible);
  return()=>{stopped=true;controller.abort();clearInterval(timer);document.removeEventListener("visibilitychange",visible)};
 },[revision]);
 const close=(id:string)=>{setEditing(null);requestAnimationFrame(()=>section.current?.querySelector<HTMLButtonElement>(`[data-schedule-id="${id}"] .schedule-edit`)?.focus({preventScroll:true}))};
 return <section className="system-schedules" ref={section} aria-labelledby="system-schedules-title" aria-busy={loading}>
  <div className="schedules-heading"><div><h2 id="system-schedules-title">系统定时任务</h2><p>每种维护计划只保留一条，按各自的规则自动运行。</p></div><button className="btn btn-sm btn-ghost" aria-label="刷新系统定时任务" disabled={loading} onClick={()=>setRevision(value=>value+1)}>刷新</button></div>
  {feedback&&<p className="schedule-feedback" role="status">{feedback}</p>}
  {error&&<div className="schedule-load-error" role="alert"><p>{error}{items.length>0?"；以下为上次读取的计划。":""}</p><button className="btn btn-sm" onClick={()=>setRevision(value=>value+1)}>重试读取计划</button></div>}
  {loading&&!items.length?<p className="schedule-empty" role="status">正在读取系统计划…</p>:!error&&!items.length?<p className="schedule-empty">暂无系统定时任务。</p>:<ul className="schedule-list">{items.map(schedule=><li key={schedule.id} data-schedule-id={schedule.id}>
   <div className="schedule-row"><div className="schedule-name"><h3>{schedule.name}</h3><p>{schedule.description}</p></div>
    <div className="schedule-rule"><span>{ruleText(schedule)} <span className="schedule-zone">{zoneName(schedule.timezone)}</span></span><small>{!schedule.enabled?schedule.state==="running"?"当前运行完成后停止":"已停用":schedule.next_run?<>下次 <time dateTime={new Date(schedule.next_run*1000).toISOString()}>{timeText(schedule.next_run,schedule.timezone)}</time></>:"等待安排下次执行"}</small></div>
    <div className={`schedule-result${schedule.last_status==="failed"?" is-failed":""}${schedule.state==="running"?" is-running":""}`}><span>{resultText(schedule)}</span>{schedule.last_run>0&&<small><time dateTime={new Date(schedule.last_run*1000).toISOString()}>{timeText(schedule.last_run,schedule.timezone)}</time></small>}{schedule.state!=="running"&&typeof schedule.last_result?.summary==="string"&&<small>{schedule.last_result.summary}</small>}{schedule.last_error&&<p>{schedule.last_error}</p>}</div>
    <button className="btn btn-sm btn-ghost schedule-edit" aria-label={`编辑${schedule.name}`} aria-expanded={editing===schedule.id} disabled={editing!==null&&editing!==schedule.id} onClick={()=>{if(editing===schedule.id)return;setFeedback("");setEditing(schedule.id)}}>编辑</button>
   </div>
   {editing===schedule.id&&<ScheduleEditor key={schedule.id} schedule={schedule} onClose={()=>close(schedule.id)} onSave={updated=>{setItems(current=>current.map(item=>item.id===updated.id?updated:item));setFeedback(updated.enabled?`${updated.name}已保存；下次 ${timeText(updated.next_run,updated.timezone)}（${zoneName(updated.timezone)}）。`:`${updated.name}已停用${updated.state==="running"?"，当前运行完成后停止。":"。"}`);close(schedule.id);setRevision(value=>value+1)}}/>}
  </li>)}</ul>}
  <datalist id="schedule-timezones"><option value="Asia/Shanghai"/><option value="UTC"/></datalist>
 </section>;
}
