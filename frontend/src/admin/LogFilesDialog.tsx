import FloatingFeedback from "../FloatingFeedback";
import {useEffect,useRef,useState} from 'react';
import {createPortal} from 'react-dom';
import {Link} from 'react-router-dom';
import {api} from '../api';
import type {OperationItem} from '../api';
import {useDialog} from './interactions';
import {fileUnavailableText} from './taskPresentation';
import {logMessage,logOutcome} from './logPresentation';
import type {LogEntry} from './logPresentation';

const states:Record<string,string>={pending:'等待处理',queued:'等待处理',running:'处理中',archiving:'归档中',done:'已完成',failed:'失败',skipped:'已跳过',duplicate:'重复文件',canceled:'已取消',archived:'已归档',published:'已发布',processing:'处理中',deleted:'记录不可用'};
export default function LogFilesDialog({entry,onClose}:{entry:LogEntry;onClose:()=>void}){
 const ref=useRef<HTMLDivElement>(null);useDialog(ref,onClose);
 const previousQuery=useRef('');
 const [items,setItems]=useState<OperationItem[]>([]),[page,setPage]=useState(1),[next,setNext]=useState(0),[total,setTotal]=useState(0),[busy,setBusy]=useState(true),[error,setError]=useState(''),[revision,setRevision]=useState(0),[owner,setOwner]=useState(entry.owner_id);
 const id=entry.group?.task_id||entry.id;
 useEffect(()=>{
  const controller=new AbortController(),query=`${entry.record}:${id}:${page}`;setBusy(true);setError('');
  if(previousQuery.current!==query){previousQuery.current=query;setItems([])}
  if(entry.record==='receipt'&&!entry.group){setItems([{key:String(entry.id),file_id:entry.file_id,file_name:entry.file_name||'未命名文件',file_exists:entry.file_exists===true,file_successor:entry.file_successor,state:entry.state,error:entry.last_error||''}]);setTotal(1);setBusy(false);return()=>controller.abort()}
  api.get<{items:OperationItem[];total:number;next_page:number;owner_id:string}>(`/api/admin/logs/tasks/${id}/files?page=${page}`,controller.signal).then(data=>{if(!controller.signal.aborted){setItems(data.items);setTotal(data.total);setNext(data.next_page);setOwner(data.owner_id)}}).catch(cause=>{if(!controller.signal.aborted)setError(cause.message||'文件列表读取失败，请重试。')}).finally(()=>{if(!controller.signal.aborted)setBusy(false)});
  return()=>controller.abort();
 },[entry,id,page,revision]);
 return createPortal(<><div className="overlay operation-overlay" onClick={onClose}/><div ref={ref} className="modal operation-dialog log-files-dialog" role="dialog" aria-modal="true" aria-labelledby="log-files-title"><div className="dhead"><h2 id="log-files-title">文件列表 <span>{total}</span></h2><button type="button" className="btn btn-ghost" onClick={onClose}>关闭</button></div><div className="log-files-body" aria-busy={busy}><p className="log-files-operation">{logMessage(entry)}</p>{logOutcome(entry)&&<p className="detail-hint">{logOutcome(entry)}</p>}{entry.context?.source&&<p className="detail-hint">来源：{entry.context.source}</p>}{error&&<FloatingFeedback className="detail-error" role="alert">{error}<button type="button" className="btn btn-sm" onClick={()=>setRevision(v=>v+1)}>重试读取</button></FloatingFeedback>}{busy&&!items.length?<p role="status">正在读取文件…</p>:!error&&!items.length?<p>没有文件记录。</p>:<ul className="operation-items log-files-items">{items.map(item=>{const query=new URLSearchParams({file:item.file_successor?.id||item.file_id});if(item.file_successor?.owner_id||item.owner_id||owner)query.set('owner_id',item.file_successor?.owner_id||item.owner_id||owner!);return <li key={item.key}><div><strong>{item.file_name||'未命名文件'}</strong>{item.file_successor&&<small>{item.file_successor.kind==="transferred"?"已转移归属，原件保留":"已完成加密转换"}</small>}{item.file_id&&(item.file_exists===true||item.file_successor)?<Link to={`/admin?${query}`} onClick={onClose}>{item.file_successor?"查看新位置":"查看文件"}</Link>:item.file_id&&item.file_exists===false?<small>{fileUnavailableText}</small>:null}</div><span className={item.state==='failed'?'detail-error':''}>{states[item.state]||item.state}</span>{item.error&&<p>{item.error}</p>}</li>})}</ul>}{(page>1||next>0)&&<nav className="log-paging" aria-label="文件列表分页"><span>第 {page} 页</span><button type="button" className="btn btn-sm btn-ghost" disabled={busy||page===1} onClick={()=>setPage(v=>v-1)}>上一页</button><button type="button" className="btn btn-sm" disabled={busy||!next} onClick={()=>setPage(next)}>下一页</button></nav>}</div></div></>,document.body);
}
