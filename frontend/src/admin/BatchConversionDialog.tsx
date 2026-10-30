import FloatingFeedback from "../FloatingFeedback";
import {useEffect,useId,useRef,useState} from "react";
import {createPortal} from "react-dom";
import {useGallery} from "./GalleryScope";
import {useDialog} from "./interactions";
import "./batch-conversion.css";

type Check={id:string;name?:string;state:"ready"|"skipped"|"blocked";reason?:string};
export default function BatchConversionDialog({ids,busy,onClose,onSubmit}:{ids:string[];busy:boolean;onClose:()=>void;onSubmit:(encrypted:boolean,ids:string[])=>Promise<boolean>}){
 const {api}=useGallery(),ref=useRef<HTMLDivElement>(null),lock=useRef(false),title=useId(),hint=useId();
 const [encrypted,setEncrypted]=useState(true),[items,setItems]=useState<Check[]>([]),[checking,setChecking]=useState(true),[error,setError]=useState(""),[revision,setRevision]=useState(0),[loaded,setLoaded]=useState(0);
 const close=()=>{if(!busy&&!lock.current)onClose()};useDialog(ref,close);
 useEffect(()=>{
  let stopped=false;setChecking(true);setItems([]);setError("");setLoaded(0);
  void (async()=>{try{const checked:Check[]=[];for(let offset=0;offset<ids.length;offset+=200){if(stopped)return;const result=await api.post<{items:Check[]}>("/files/conversion-check",{ids:ids.slice(offset,offset+200),encrypted});if(stopped)return;checked.push(...result.items);setLoaded(Math.min(ids.length,offset+200))}setItems(checked)}catch(cause:any){if(!stopped)setError(cause.message||"检查失败，请重试。")}finally{if(!stopped)setChecking(false)}})();
  return()=>{stopped=true};
 },[api,ids,encrypted,revision]);
 const needed=items.filter(item=>item.state==="ready"),skipped=items.filter(item=>item.state==="skipped"),blocked=items.filter(item=>item.state==="blocked"),issues=items.filter(item=>item.state!=="ready");
 const submit=async(event:React.FormEvent)=>{event.preventDefault();if(busy||lock.current||checking||error||!needed.length)return;lock.current=true;try{if(await onSubmit(encrypted,needed.map(item=>item.id)))onClose();else setError("提交未完成，请检查提示后重新检查。")}catch(cause:any){setError(cause.message||"转换提交失败，请重试。")}finally{lock.current=false}};
 return createPortal(<><div className="overlay" onClick={close}/><div ref={ref} className="modal batch-conversion-dialog" role="dialog" aria-modal="true" aria-labelledby={title} aria-busy={busy||checking}><div className="dhead"><h2 id={title}>批量加密转换</h2><button type="button" className="btn btn-ghost" disabled={busy} onClick={close}>关闭</button></div><form onSubmit={submit}><fieldset className="batch-conversion-fields" disabled={busy}><p>已选 {ids.length} 项，只提交检查后需要转换的文件。</p><label htmlFor={`${title}-target`}>转换为</label><select id={`${title}-target`} className="select" value={String(encrypted)} onChange={event=>{setChecking(true);setItems([]);setEncrypted(event.target.value==="true")}} aria-describedby={hint}><option value="true">加密（按存储设置）</option><option value="false">未加密</option></select><p id={hint} className="detail-hint">加密按各存储设置；没有允许加密的存储也可转换，副本保持明文。转换后新文件默认私有，旧链接停用；原件保留至校验成功。</p><p className="batch-conversion-summary" role="status" aria-live="polite">{checking?`正在检查 ${loaded} / ${ids.length} 项…`:error?"尚未完成检查":`需转换 ${needed.length} 项 · 无需转换 ${skipped.length} 项 · 暂不能处理 ${blocked.length} 项`}</p>{!checking&&!error&&!needed.length&&<p className="detail-hint">没有需要提交的文件，不会创建转换任务。</p>}{!checking&&issues.length>0&&<details className="batch-conversion-issues"><summary>查看跳过原因（{issues.length} 项）</summary><ul>{issues.map(item=><li key={item.id}><strong>{item.name||item.id}</strong><span>{item.reason||"暂不能处理"}</span></li>)}</ul></details>}{error&&<FloatingFeedback className="detail-error" role="alert">{error}</FloatingFeedback>}</fieldset><div className="batch-conversion-actions"><button type="button" className="btn btn-ghost" disabled={busy||checking} onClick={()=>setRevision(value=>value+1)}>重新检查</button><button type="button" className="btn btn-ghost" disabled={busy} onClick={close}>取消</button><button type="submit" className="btn btn-crayon" disabled={busy||checking||!!error||!needed.length}>{busy?"正在提交…":`确认转换 ${needed.length} 项`}</button></div></form></div></>,document.body);
}
