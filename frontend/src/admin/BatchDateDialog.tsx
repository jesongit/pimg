import {useId,useRef,useState} from "react";
import {createPortal} from "react-dom";
import {useDialog} from "./interactions";

export function localDateInput(stamp:number){
  const date=new Date(stamp*1000),pad=(n:number)=>String(n).padStart(2,"0");
  return `${String(date.getFullYear()).padStart(4,"0")}-${pad(date.getMonth()+1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}
export function localDateSeconds(value:string):number|null{
  // datetime-local controls may serialize integral seconds with .000.
  value=value.replace(/\.0{1,3}$/,"");
  if(!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$/.test(value))return null;
  const normalized=value.length===16?value+":00":value,stamp=Date.parse(normalized)/1000;
  return Number.isInteger(stamp)&&stamp>=-2208988800&&stamp<=253402300799&&localDateInput(stamp)===normalized?stamp:null;
}
export default function BatchDateDialog({count,initial,busy,onClose,onSubmit}:{count:number;initial:number;busy:boolean;onClose:()=>void;onSubmit:(stamp:number)=>Promise<boolean>}){
  const ref=useRef<HTMLDivElement>(null),lock=useRef(false),title=useId(),field=useId();
  const [value,setValue]=useState(()=>localDateInput(initial)),[error,setError]=useState("");
  const close=()=>{if(!busy&&!lock.current)onClose()};useDialog(ref,close);
  const submit=async(event:React.FormEvent)=>{event.preventDefault();if(busy||lock.current)return;const stamp=localDateSeconds(value);if(stamp===null){setError("请填写有效的年月日和时分秒。");return}lock.current=true;setError("");try{if(await onSubmit(stamp))onClose();else setError("提交未完成，请检查提示后重试。")}catch(cause:any){setError(cause.message||"日期修改失败，请重试。")}finally{lock.current=false}};
  return createPortal(<><div className="overlay" onClick={close}/><div ref={ref} className="modal batch-date-dialog" role="dialog" aria-modal="true" aria-labelledby={title} aria-busy={busy}><div className="dhead"><h2 id={title}>批量修改日期</h2><button type="button" className="btn btn-ghost" disabled={busy} onClick={close}>关闭</button></div><form onSubmit={submit}><fieldset className="batch-date-fields" disabled={busy}><p>将已选 {count} 项的图库日期统一设置为：</p><label htmlFor={field}>日期与时间</label><input id={field} className="input" type="datetime-local" step="1" required value={value} onChange={event=>setValue(event.target.value)} aria-describedby={`${field}-hint`}/><p id={`${field}-hint`} className="detail-hint">按当前设备时区（{Intl.DateTimeFormat().resolvedOptions().timeZone}）保存，精确到秒。用于图库排序和时间轴；原件及上传时间保留。</p>{error&&<p className="detail-error" role="alert">{error}</p>}</fieldset><div className="batch-date-actions"><button type="button" className="btn btn-ghost" disabled={busy} onClick={close}>取消</button><button type="submit" className="btn btn-crayon" disabled={busy}>{busy?"正在修改…":"保存日期"}</button></div></form></div></>,document.body);
}
