import {useEffect,useRef,useState} from "react";
import {Link,useSearchParams} from "react-router-dom";
import {api,fmtTime} from "../api";
import type {LogEntry as Entry} from './logPresentation';
import {logActive as activeOf,logMessage as titleOf,logOutcome as summaryOf,logReason,logState as stateOf,logStatus as statusOf,logTone} from './logPresentation';
import LogFilesDialog from './LogFilesDialog';
import {useToast} from "../App";
import ConfirmButton from "./ConfirmButton";
import {fileUnavailableText,taskHasFile} from "./taskPresentation";
import {tasksUpdated} from "./TaskStatus";
import FilterSelect from "./FilterSelect";

type ClearResult={hidden:number;retained_active:number;retained_recovery:number;more?:boolean};
function clock(row:Entry){return row.done_at||row.updated_at||row.created_at}
function dayKey(row:Entry){const date=new Date((row.updated_at||clock(row))*1000);return `${date.getFullYear()}-${date.getMonth()+1}-${date.getDate()}`}
function dayLabel(row:Entry){const date=new Date((row.updated_at||clock(row))*1000),now=new Date(),yesterday=new Date();yesterday.setDate(now.getDate()-1);const same=(value:Date)=>value.getFullYear()===date.getFullYear()&&value.getMonth()===date.getMonth()&&value.getDate()===date.getDate();if(same(now))return "今天";if(same(yesterday))return "昨天";return date.toLocaleDateString("zh-CN",{...(date.getFullYear()!==now.getFullYear()?{year:"numeric" as const}:{}),month:"long",day:"numeric",weekday:"long"})}
function keyOf(row:Entry){return row.record_key||`${row.record}:${row.id}`}

export default function LogHistory(){
 const [params,setParams]=useSearchParams(),toast=useToast(),search=params.get("q")||"",fileID=params.get("file_id")||"",groupID=params.get("group_id")||"",page=Math.max(1,Number(params.get("page"))||1);
 const state=params.get("state")||"";
 const query=new URLSearchParams({record:"all",q:search,file_id:fileID,group_id:groupID,...(state?{state}:{}),page:String(page)}).toString();
 const initial=api.cached<{items:Entry[];total:number;next_page:number}>(`/api/admin/logs?${query}`);
 const [draft,setDraft]=useState(search),[items,setItems]=useState<Entry[]>(initial?.items||[]),[total,setTotal]=useState(initial?.total||0),[next,setNext]=useState(initial?.next_page||0),[loading,setLoading]=useState(!initial),[error,setError]=useState(""),[revision,setRevision]=useState(0),[opened,setOpened]=useState<Entry|null>(null),[clearing,setClearing]=useState(false);
 const list=useRef<HTMLElement>(null),previousQuery=useRef(initial?query:""),loadedQuery=useRef(initial?query:""),moveToList=useRef(false),hasActive=useRef(false),clearLock=useRef(false);

 const navigation=(values:{q?:string;page?:number;state?:string;clearContext?:boolean})=>{const updated=new URLSearchParams();const q=values.q??search;if(q)updated.set("q",q);const selected=values.state??state;if(selected)updated.set("state",selected);if(!values.clearContext){if(fileID)updated.set("file_id",fileID);if(groupID)updated.set("group_id",groupID)}if((values.page||1)>1)updated.set("page",String(values.page));setParams(updated)};
 useEffect(()=>setDraft(search),[search]);
 useEffect(()=>{if(draft.trim()===search)return;const timer=setTimeout(()=>navigation({q:draft.trim()}),300);return()=>clearTimeout(timer)},[draft,search,fileID,groupID,state]);
 useEffect(()=>{
  const controller=new AbortController();let stopped=false,inFlight=false,queued=false;
  if(previousQuery.current!==query){const cached=api.cached<{items:Entry[];total:number;next_page:number}>(`/api/admin/logs?${query}`);previousQuery.current=query;loadedQuery.current=cached?query:"";setItems(cached?.items||[]);setTotal(cached?.total||0);setNext(cached?.next_page||0);setOpened(null)}
  setLoading(loadedQuery.current!==query);setError("");
  const load=async()=>{if(stopped)return;if(inFlight){queued=true;return}inFlight=true;try{const data=await api.get<{items:Entry[];total:number;next_page:number}>(`/api/admin/logs?${query}`,controller.signal);if(stopped)return;loadedQuery.current=query;setItems(data.items||[]);setTotal(data.total||0);setNext(data.next_page||0);setError("");hasActive.current=(data.items||[]).some(activeOf);if(moveToList.current){moveToList.current=false;requestAnimationFrame(()=>{if(!stopped){list.current?.focus({preventScroll:true});list.current?.scrollIntoView({block:"start"})}})}}catch(cause:any){if(!stopped&&!controller.signal.aborted&&cause.name!=="AbortError")setError(cause.message||"日志读取失败，请重试。")}finally{inFlight=false;if(!stopped){setLoading(false);if(queued){queued=false;void load()}}}};
  const visible=()=>{if(!document.hidden)void load()};void load();const timer=setInterval(()=>{if(hasActive.current&&!document.hidden)void load()},5000);document.addEventListener("visibilitychange",visible);window.addEventListener("pimg:tasks-updated",visible);
  return()=>{stopped=true;controller.abort();clearInterval(timer);document.removeEventListener("visibilitychange",visible);window.removeEventListener("pimg:tasks-updated",visible)};
 },[query,revision]);
 const clear=async()=>{if(clearLock.current)return;clearLock.current=true;setClearing(true);try{const result=await api.post<ClearResult>("/api/admin/logs/clear",{confirm:"DELETE_ALL_LOGS"});setOpened(null);if(page>1)navigation({page:1});else setRevision(value=>value+1);tasksUpdated();toast(result.hidden?"已删除现有日志，运行与恢复所需数据已保留":"当前没有可删除的历史日志，运行与恢复所需数据已保留")}catch(cause:any){toast(cause.message||"日志删除失败，请重试。","err")}finally{clearLock.current=false;setClearing(false)}};
 const initialLoading=previousQuery.current!==query||loading&&loadedQuery.current!==query;
 const days:{key:string;label:string;rows:Entry[]}[]=[];
 for(const row of items){const key=dayKey(row),last=days[days.length-1];if(last?.key===key)last.rows.push(row);else days.push({key,label:dayLabel(row),rows:[row]})}
 const filtered=!!(search||fileID||groupID||state);
 return <div className="log-history">
  <header className="history-heading"><h1>日志</h1><span role="status">{initialLoading?"读取中…":`${total} 条操作记录`}</span></header>
  <div className="history-toolbar"><label className="history-search"><svg viewBox="0 0 20 20" width="17" height="17" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true"><circle cx="8.5" cy="8.5" r="5.5"/><path d="m13 13 4 4"/></svg><input className="input" aria-label="搜索日志" placeholder="搜索文件、相册、操作编号或错误" value={draft} onChange={event=>setDraft(event.target.value)} /></label><FilterSelect label="日志状态" value={state} active={!!state} width={168} onChange={value=>navigation({state:value})} options={[{value:"",label:"全部状态"},{value:"done",label:"已完成"},{value:"issues",label:"有异常 / 失败"},{value:"canceled",label:"已取消 / 忽略"}]}/><button type="button" className="btn btn-ghost" disabled={loading||clearing} onClick={()=>setRevision(value=>value+1)}>刷新日志</button><ConfirmButton className="btn btn-ghost history-clear" disabled={loading||clearing} confirmLabel="确认删除" busyLabel="删除中…" resetKey={query} description="删除全部已有日志，包含搜索之外的记录。文件、连接及等待、运行和恢复所需的数据会保留。" onConfirm={clear}>删除全部</ConfirmButton></div>
  {(fileID||groupID)&&<p className="history-context">{groupID?`操作 #${groupID}`:`文件 ${fileID.slice(-8)}`}<button type="button" className="text-button" onClick={()=>navigation({clearContext:true})}>取消限定</button></p>}
  {error&&<div className="history-load-error" role="alert"><p>{error}{items.length?" 当前显示上次读取的结果。":""}</p><button type="button" className="btn btn-sm" onClick={()=>setRevision(value=>value+1)}>重试读取日志</button></div>}
  <section ref={list} className="history-stream" aria-label="操作日志" aria-busy={loading} tabIndex={-1}>
   {initialLoading?<div className="history-loading" role="status"><span>正在读取日志…</span><div aria-hidden="true"/><div aria-hidden="true"/><div aria-hidden="true"/></div>:!error&&!items.length?<div className="history-empty"><h2>{filtered?"没有找到相关日志":"暂无日志"}</h2><p>{filtered?"试试文件名称或操作编号。":"操作完成后会在这里留下记录。"}</p>{filtered&&<button type="button" className="btn btn-ghost" onClick={()=>{setDraft("");navigation({q:"",state:"",clearContext:true})}}>查看全部日志</button>}</div>:days.map(day=><section className="history-day" key={day.key} aria-label={day.label}><h2>{day.label}</h2><ol>{day.rows.map(row=><HistoryRow key={keyOf(row)} row={row} onFiles={()=>setOpened(row)}/>)}</ol></section>)}
  </section>
  {(page>1||next>0)&&<nav className="history-paging" aria-label="日志分页"><span>第 {page} 页</span><button type="button" className="btn btn-sm btn-ghost" disabled={loading||clearing||page===1} onClick={()=>{moveToList.current=true;navigation({page:page-1})}}>上一页</button><button type="button" className="btn btn-sm" disabled={loading||clearing||!next} onClick={()=>{moveToList.current=true;navigation({page:next})}}>下一页</button></nav>}
  {opened&&<LogFilesDialog key={keyOf(opened)} entry={opened} onClose={()=>setOpened(null)}/>}
 </div>;
}

function HistoryRow({row,onFiles}:{row:Entry;onFiles:()=>void}){
 const toast=useToast(),[retrying,setRetrying]=useState(false),[retryError,setRetryError]=useState('');
 const lock=useRef(false),state=stateOf(row),active=activeOf(row),outcome=summaryOf(row),reason=logReason(row),status=statusOf(row),time=clock(row),realFile=taskHasFile(row);
 const canRetry=row.type!=='system_schedule'&&row.record==='task'&&(row.can_retry??row.group?.can_retry??['failed','canceled'].includes(state));
 const hasFiles=!!row.group?.total_files||realFile,taskID=row.group?.task_id||row.id;
 const query=new URLSearchParams({file:row.file_successor?.id||row.file_id});const fileOwner=row.file_successor?.owner_id||row.owner_id;if(fileOwner)query.set('owner_id',fileOwner);
 const retry=async()=>{if(lock.current)return;lock.current=true;setRetrying(true);setRetryError('');try{await api.post(`/api/admin/tasks/${taskID}/retry${row.group?'?group=1':''}`);tasksUpdated();toast('已重新提交未完成项')}catch(cause:any){setRetryError(cause.message||'重试提交失败，请稍后再试。')}finally{lock.current=false;setRetrying(false)}};
 return <li className={`history-event is-${logTone(row)}`} data-record-key={keyOf(row)} data-task-id={taskID}>
  <time className="history-row-time" dateTime={new Date(time*1000).toISOString()} title={`${active?'更新于':'结束于'} ${fmtTime(time)} · 操作 #${taskID}`}>{new Date(time*1000).toLocaleTimeString('zh-CN',{hour:'2-digit',minute:'2-digit',hour12:false})}</time>
  <div className="history-event-content"><h3>{titleOf(row)}</h3>{outcome&&<span className="history-event-summary">{outcome}</span>}{reason&&<span className={state==='failed'?'history-event-error':'history-event-note'}>{reason}</span>}{realFile&&row.file_successor?<span className="history-event-note">{row.file_successor.kind==="transferred"?"已转移归属，原件保留在新位置":"已完成加密转换，使用新文件编号"}</span>:realFile&&row.file_exists===false&&<span className="history-event-note">{fileUnavailableText}</span>}{retryError&&<span className="history-event-error" role="alert">{retryError}</span>}</div>
  <span className="history-event-state">{status}</span><div className="history-row-actions">{canRetry&&<button type="button" className="btn btn-sm btn-ghost history-retry" disabled={retrying} onClick={()=>void retry()}>{retrying?'提交中…':state==='canceled'?'继续剩余项':'重试失败项'}</button>}{realFile&&(row.file_exists===true||row.file_successor)&&<Link className="text-button" to={`/admin?${query}`}>{row.file_successor?"查看新位置":"查看文件"}</Link>}{hasFiles&&<button type="button" className="text-button" aria-haspopup="dialog" onClick={onFiles}>文件{row.group?.total_files?`（${row.group.total_files}）`:''}</button>}{row.type==='system_schedule'&&<Link className="text-button" to="/admin/tasks#system-schedules-title">定时设置</Link>}{active&&<Link className="text-button" to={`/admin/tasks?group_id=${taskID}`}>当前任务</Link>}</div>
 </li>;
}
