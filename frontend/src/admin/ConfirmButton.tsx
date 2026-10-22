import {useCallback,useEffect,useId,useLayoutEffect,useRef,useState} from "react";
import type {ButtonHTMLAttributes,MouseEvent,ReactNode} from "react";
import {createPortal} from "react-dom";
import {useToast} from "../App";
import "./confirm-button.css";

type Props=Omit<ButtonHTMLAttributes<HTMLButtonElement>,"onClick">&{
  onConfirm:()=>unknown|Promise<unknown>;
  confirmLabel?:string;
  busyLabel?:string;
  description?:ReactNode;
  resetKey?:unknown;
  /** A user-selected option is already the first step of a confirmation. */
  armKey?:unknown;
  onReset?:()=>void;
};
let current:{id:string;reset:()=>void}|null=null;
const confirmationLifetime=30000;

/** Reserve every label's width so the second click hits the original button. */
export default function ConfirmButton({children,onConfirm,confirmLabel="确认",busyLabel="处理中…",description,resetKey,armKey,onReset,disabled,className="btn",onBlur,onKeyDown,onPointerDown,onPointerCancel,"aria-label":label,"aria-describedby":describedBy,...props}:Props){
  const id=useId(),toast=useToast(),button=useRef<HTMLButtonElement>(null),hint=useRef<HTMLDivElement>(null);
  const armedRef=useRef(false),locked=useRef(false),alive=useRef(true),activation=useRef(false),timer=useRef<ReturnType<typeof setTimeout>>();
  const intent=useRef<{resetKey:unknown;armKey:unknown;expiresAt:number}>();
  const [armed,setArmed]=useState(false),[busy,setBusy]=useState(false),[position,setPosition]=useState<{top:number;left:number}|null>(null);
  const context=useRef({resetKey,armKey});context.current={resetKey,armKey};
  const resetCallback=useRef(onReset);resetCallback.current=onReset;
  const reset=useCallback(()=>{
    const wasArmed=armedRef.current;
    armedRef.current=false;activation.current=false;intent.current=undefined;clearTimeout(timer.current);
    if(alive.current){setArmed(false);setPosition(null)}
    if(current?.id===id)current=null;
    if(wasArmed&&alive.current)resetCallback.current?.();
  },[id]);
  const arm=useCallback(()=>{
    if(current?.id!==id)current?.reset();armedRef.current=true;setArmed(true);current={id,reset};
    activation.current=false;
    intent.current={...context.current,expiresAt:Date.now()+confirmationLifetime};
    clearTimeout(timer.current);timer.current=setTimeout(reset,confirmationLifetime);
  },[id,reset]);
  useLayoutEffect(()=>{alive.current=true;return()=>{alive.current=false;clearTimeout(timer.current);if(current?.id===id)current=null}},[id]);
  useLayoutEffect(()=>{reset()},[resetKey,reset]);
  // Selecting a target is the first step. Temporary loading must neither cancel
  // this choice nor arm it again when the button becomes enabled.
  useLayoutEffect(()=>{if(armKey!==undefined){arm();if(!button.current?.disabled)button.current?.focus({preventScroll:true})}else reset()},[armKey,arm,reset]);
  const placeHint=useCallback(()=>{
    if(!button.current||!hint.current)return;
    const rect=button.current.getBoundingClientRect(),box=hint.current.getBoundingClientRect();
    const left=Math.max(12,Math.min(window.innerWidth-box.width-12,rect.left+rect.width/2-box.width/2));
    const top=rect.top-box.height-8>=8?rect.top-box.height-8:Math.min(window.innerHeight-box.height-8,rect.bottom+8);
    setPosition({left,top:Math.max(8,top)});
  },[]);
  useEffect(()=>{
    if(!armed)return;
    const outside=(event:PointerEvent)=>{if(!button.current?.contains(event.target as Node))reset()};
    const escape=(event:KeyboardEvent)=>{if(event.key==="Escape"){event.preventDefault();event.stopPropagation();reset()}};
    const hidden=()=>{if(document.hidden)reset()};
    document.addEventListener("pointerdown",outside,true);document.addEventListener("keydown",escape,true);
    window.addEventListener("scroll",placeHint,true);window.addEventListener("resize",placeHint);window.addEventListener("blur",reset);document.addEventListener("visibilitychange",hidden);
    return()=>{document.removeEventListener("pointerdown",outside,true);document.removeEventListener("keydown",escape,true);window.removeEventListener("scroll",placeHint,true);window.removeEventListener("resize",placeHint);window.removeEventListener("blur",reset);document.removeEventListener("visibilitychange",hidden)};
  },[armed,reset,placeHint]);
  useLayoutEffect(()=>{if(armed&&description)placeHint()},[armed,description,placeHint]);
  const click=async(event:MouseEvent<HTMLButtonElement>)=>{
    const freshActivation=activation.current||event.detail===0&&event.isTrusted;activation.current=false;
    if(disabled||locked.current||button.current?.matches(":disabled"))return;
    const pending=intent.current;
    if(armedRef.current&&pending&&(!Object.is(pending.resetKey,resetKey)||!Object.is(pending.armKey,armKey))){reset();return}
    if(!armedRef.current||!pending||Date.now()>=pending.expiresAt){
      arm();return;
    }
    // Accept the next distinct press immediately, including a fast double click.
    // Requiring a new press after arming prevents a native select's release or
    // a repeated click event from consuming the confirmation by itself.
    if(!freshActivation||button.current?.dataset.confirmState!=="armed")return;
    locked.current=true;clearTimeout(timer.current);setBusy(true);
    try{await onConfirm();if(alive.current)reset()}
    catch(error:any){if(alive.current){reset();toast(error?.message||"操作未完成，请重试。","err")}}
    finally{locked.current=false;if(alive.current)setBusy(false)}
  };
  const text=busy?busyLabel:armed?confirmLabel:children;
  return <>
    <button {...props} ref={button} type={props.type||"button"} className={`${className} confirm-button${armed?" is-confirming":""}`} disabled={disabled||busy} data-confirm-state={busy?"busy":armed?"armed":"idle"} aria-busy={busy||undefined} aria-label={busy?busyLabel:armed?confirmLabel:label} aria-describedby={[describedBy,armed&&description?`${id}-hint`:null].filter(Boolean).join(" ")||undefined}
      onClick={event=>void click(event)} onPointerDown={event=>{onPointerDown?.(event);activation.current=!event.defaultPrevented&&event.button===0&&armedRef.current}}
      onPointerCancel={event=>{activation.current=false;onPointerCancel?.(event)}}
      onBlur={event=>{onBlur?.(event);if(!locked.current&&event.relatedTarget&&!button.current?.matches(":disabled"))reset()}}
      onKeyDown={event=>{onKeyDown?.(event);if(event.key==="Enter"||event.key===" "){if(event.repeat){event.preventDefault();return}activation.current=!event.defaultPrevented&&armedRef.current}}}>
      <span className="confirm-button-text"><span aria-hidden="true" className="confirm-button-measure">{children}</span><span aria-hidden="true" className="confirm-button-measure">{confirmLabel}</span><span aria-hidden="true" className="confirm-button-measure">{busyLabel}</span><span>{text}</span></span>
    </button>
    {armed&&description&&createPortal(<div ref={hint} id={`${id}-hint`} role="tooltip" className="confirm-button-hint" style={position?{left:position.left,top:position.top}:{visibility:"hidden"}}>{description}</div>,document.body)}
  </>;
}
