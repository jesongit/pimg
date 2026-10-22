import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useRef, useState } from "react";
import { Link, Outlet, useLocation } from "react-router-dom";
import {createPortal} from "react-dom";
import {IconCheck,IconExclamationCircle,IconLoader2,IconX} from "@tabler/icons-react";
import type { SiteInfo } from "./api";
import { api, displaySiteName } from "./api";
import { UnsavedChangesGuard,useDialog } from "./admin/interactions";
import "./styles/notifications.css";

export type ToastOptions = { key?: string; description?:string; progress?:number|null; progressLabel?:string; details?: { label: string; description: string }[]; href?: string; persistent?: boolean; onAction?: () => void; actionLabel?: string };
type Toast = ToastOptions & { id: number; msg: string; kind: "ok" | "err" };
export const ToastCtx = createContext<(msg: string, kind?: "ok" | "err", options?: ToastOptions) => void>(() => {});
export const SiteCtx = createContext<SiteInfo | null>(null);

export function useToast() {
  return useContext(ToastCtx);
}

export default function App() {
  const location = useLocation();
  const privateScope = /^\/(?:admin|library)(?:\/|$)/.test(location.pathname);
  const [site, setSite] = useState<SiteInfo | null>(null);
  const [toasts, setToasts] = useState<Toast[]>([]);
  const [expanded,setExpanded]=useState(false),[detailID,setDetailID]=useState<number|null>(null),[obstruction,setObstruction]=useState(0);
  const [toastEpoch, setToastEpoch] = useState(0), currentEpoch = useRef(0);
  const timers = useRef(new Map<number, ReturnType<typeof setTimeout>>());
  const toastKeys = useRef(new Map<string, number>());
  const messageKeys=useRef(new Map<string,number>()),dismissedKeys=useRef(new Set<string>()),paused=useRef(new Set<number>());
  const loadedDismissals=useRef(false);
  if(!loadedDismissals.current){loadedDismissals.current=true;try{for(const key of JSON.parse(sessionStorage.getItem("pimg:notifications:dismissed")||"[]"))if(typeof key==="string")dismissedKeys.current.add(key)}catch{}}
  const clearToasts = useCallback(() => {
    currentEpoch.current++;
    setToastEpoch(currentEpoch.current);
    for (const timer of timers.current.values()) clearTimeout(timer);
    timers.current.clear();
    toastKeys.current.clear();
    messageKeys.current.clear();
    dismissedKeys.current.clear();paused.current.clear();
    try{sessionStorage.removeItem("pimg:notifications:dismissed")}catch{}
    setDetailID(null);setExpanded(false);
    setToasts([]);
  }, []);
  const dismissToast = useCallback((id: number) => {
    clearTimeout(timers.current.get(id));
    timers.current.delete(id);
    paused.current.delete(id);
    for (const [key, value] of toastKeys.current) if (value === id) {toastKeys.current.delete(key);dismissedKeys.current.add(key)}
    for(const [key,value] of messageKeys.current)if(value===id)messageKeys.current.delete(key);
    try{sessionStorage.setItem("pimg:notifications:dismissed",JSON.stringify([...dismissedKeys.current].slice(-1000)))}catch{}
    setDetailID(current=>current===id?null:current);
    setToasts((current) => current.filter((item) => item.id !== id));
  }, []);

  const toast = useCallback((msg: string, kind: "ok" | "err" = "ok", options: ToastOptions = {}) => {
    if (toastEpoch !== currentEpoch.current || options.key&&dismissedKeys.current.has(options.key)) return;
    const messageKey=`${kind}:${msg}`;
    const id = (options.key?toastKeys.current.get(options.key):messageKeys.current.get(messageKey)) || Date.now() + Math.random();
    if (options.key) toastKeys.current.set(options.key, id);else messageKeys.current.set(messageKey,id);
    clearTimeout(timers.current.get(id));
    timers.current.delete(id);
    const persistent = options.persistent ?? (kind==="err" || !!options.details?.length);
    setToasts((current) => { const next = { ...options, persistent, id, msg, kind }; return current.some(item => item.id === id) ? current.map(item => item.id === id ? next : item) : [...current, next]; });
    if (!persistent&&!paused.current.has(id)) timers.current.set(id, setTimeout(() => dismissToast(id), 3200));
  }, [dismissToast, toastEpoch]);
  const pauseToast=(id:number)=>{paused.current.add(id);clearTimeout(timers.current.get(id));timers.current.delete(id)};
  const resumeToast=(item:Toast)=>{if(detailID===item.id)return;paused.current.delete(item.id);if(!item.persistent&&!timers.current.has(item.id))timers.current.set(item.id,setTimeout(()=>dismissToast(item.id),3200))};
  const closeDetails=()=>{const item=toasts.find(value=>value.id===detailID);setDetailID(null);if(item){paused.current.delete(item.id);if(!item.persistent)timers.current.set(item.id,setTimeout(()=>dismissToast(item.id),3200))}};
  // On phones, keep the compact stack above the gallery's batch actions.
  useLayoutEffect(()=>{
    if(!toasts.length){setObstruction(0);return}
    let bar:HTMLElement|null=null;
    const measure=()=>setObstruction(bar?Math.ceil(bar.getBoundingClientRect().height)+16:0);
    const resize=window.ResizeObserver?new ResizeObserver(measure):null;
    const track=()=>{const next=document.querySelector<HTMLElement>(".batch-bar");if(next===bar)return;if(bar)resize?.unobserve(bar);bar=next;if(bar)resize?.observe(bar);measure()};
    track();
    const changes=new MutationObserver(track);changes.observe(document.body,{childList:true,subtree:true});
    return()=>{changes.disconnect();resize?.disconnect()};
  },[toasts.length>0,location.pathname]);
  useEffect(()=>{if(toasts.length<=3)setExpanded(false)},[toasts.length]);
  useEffect(() => () => { for (const timer of timers.current.values()) clearTimeout(timer); timers.current.clear(); }, []);
  useLayoutEffect(() => { if (!privateScope) clearToasts(); }, [privateScope, clearToasts]);
  useEffect(() => {
    window.addEventListener("pimg:unauthorized", clearToasts);
    window.addEventListener("pimg:account-updated", clearToasts);
    window.addEventListener("pimg:auth-changed", clearToasts);
    return () => {
      window.removeEventListener("pimg:unauthorized", clearToasts);
      window.removeEventListener("pimg:account-updated", clearToasts);
      window.removeEventListener("pimg:auth-changed", clearToasts);
    };
  }, [clearToasts]);

  useEffect(() => {
    let active = true;
    const refreshSite = () => {
      api.get<SiteInfo>("/api/site").then((next) => { if (active) setSite(next); }).catch(() => {});
    };
    refreshSite();
    window.addEventListener("pimg:site-updated", refreshSite);
    return () => { active = false; window.removeEventListener("pimg:site-updated", refreshSite); };
  }, []);

  useEffect(() => {
    document.title = displaySiteName(site?.site_name);
  }, [site?.site_name]);

  return (
    <SiteCtx.Provider value={site}>
      <ToastCtx.Provider value={toast}>
        <UnsavedChangesGuard />
        <Outlet />
        {!!toasts.length&&<section className={`toasts notification-stack${expanded?" is-expanded":""}`} aria-label="通知" style={{["--notification-obstruction" as string]:`${obstruction}px`}}>
          <div className="notification-items" aria-live="polite" aria-atomic="false">{[...toasts].reverse().slice(0,expanded?undefined:3).map(item=>{
            const hasProgress=item.progress!==undefined,needsDisclosure=!!item.details?.length||(!item.onAction&&!item.href&&(item.msg.length>90||(item.description?.length||0)>120));
            const Icon=hasProgress?IconLoader2:item.kind==="err"?IconExclamationCircle:IconCheck;
            return <div key={item.id} data-notification-key={item.key} className={`toast notification ${item.kind==="err"?"toast-err":"toast-ok"}${hasProgress?" is-working":""}`} onMouseEnter={()=>pauseToast(item.id)} onMouseLeave={()=>resumeToast(item)} onFocusCapture={()=>pauseToast(item.id)} onBlurCapture={event=>{if(!event.currentTarget.contains(event.relatedTarget as Node|null))resumeToast(item)}}>
              <Icon className="notification-icon" size={18} stroke={1.8} aria-hidden="true"/>
              <div className="notification-content"><div className="toast-heading"><strong>{item.msg}</strong></div>{item.description&&<p className="notification-description">{item.description}</p>}
                {hasProgress&&<div className="notification-progress"><progress aria-label={item.progressLabel||"处理进度"} max={1} value={typeof item.progress==="number"?Math.min(1,Math.max(0,item.progress)):undefined}/>{item.progressLabel&&<span>{item.progressLabel}</span>}</div>}
                {needsDisclosure?<button type="button" className="text-button toast-task-link" onClick={()=>{pauseToast(item.id);setDetailID(item.id)}}>查看详情{item.details?.length?`（${item.details.length}）`:""}</button>:item.onAction?<button type="button" className="text-button toast-task-link" onClick={item.onAction}>{item.actionLabel||"查看详情"}</button>:item.href?<Link className="text-button toast-task-link" to={item.href}>{item.actionLabel||"查看任务"}</Link>:null}
              </div><button type="button" className="toast-close notification-close" aria-label="关闭通知" onClick={()=>dismissToast(item.id)}><IconX size={16} stroke={1.8} aria-hidden="true"/></button>
            </div>;
          })}</div>
          {toasts.length>3&&<button type="button" className="notification-more text-button" aria-expanded={expanded} onClick={()=>setExpanded(value=>!value)}>{expanded?"收起通知":`还有 ${toasts.length-3} 条通知`}</button>}
        </section>}
        {detailID!==null&&toasts.find(item=>item.id===detailID)&&<NotificationDetails item={toasts.find(item=>item.id===detailID)!} onClose={closeDetails}/>}
      </ToastCtx.Provider>
    </SiteCtx.Provider>
  );
}

function NotificationDetails({item,onClose}:{item:Toast;onClose:()=>void}){
 const ref=useRef<HTMLDivElement>(null);useDialog(ref,onClose);
 const act=()=>{onClose();requestAnimationFrame(()=>item.onAction?.())};
 return createPortal(<><div className="overlay notification-detail-overlay" onClick={onClose}/><div ref={ref} className="modal notification-detail-dialog" role="dialog" aria-modal="true" aria-labelledby="notification-detail-title"><div className="notification-detail-heading"><h2 id="notification-detail-title">通知详情</h2><button type="button" className="btn btn-sm btn-ghost" onClick={onClose}>关闭</button></div><div className="notification-detail-body"><p className="notification-detail-summary">{item.msg}</p>{item.description&&<p>{item.description}</p>}{!!item.details?.length&&<ul aria-label="操作未处理明细">{item.details.map((detail,index)=><li key={index}><strong>{detail.label}</strong><p>{detail.description}</p></li>)}</ul>}</div>{(item.onAction||item.href)&&<div className="notification-detail-actions">{item.onAction?<button type="button" className="btn btn-sm" onClick={act}>{item.actionLabel||"查看操作详情"}</button>:<Link className="btn btn-sm" to={item.href!} onClick={onClose}>{item.actionLabel||"查看任务"}</Link>}</div>}</div></>,document.body);
}
