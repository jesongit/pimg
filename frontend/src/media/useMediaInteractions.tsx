import {useCallback,useEffect,useLayoutEffect,useRef,useState} from "react";
import type {PointerEvent as ReactPointerEvent,RefObject} from "react";
import {IconArrowsMaximize,IconArrowsMinimize,IconChevronLeft,IconChevronRight,IconPlayerPlay} from "@tabler/icons-react";
import {createPlaybackAcceleration} from "./playbackAcceleration";
import useMediaFullscreen from "./useMediaFullscreen";
import "./media-interactions.css";
import FloatingFeedback from "../FloatingFeedback";

type Options={
  mediaKey:string;video:boolean;enabled:boolean;active?:boolean;source?:string;
  fullscreenImages?:boolean;
  toolbar?:boolean;
  videoElement:RefObject<HTMLVideoElement>;stageElement:RefObject<HTMLDivElement>;onRequestPlay:()=>void;
  onPrev?:()=>void;onNext?:()=>void;
};
type Gesture={id:number;x:number;y:number;width:number;mode:"pending"|"seek"|"ignored";start:number;target:number;applied:number;wasPlaying:boolean};
const clamp=(value:number,min:number,max:number)=>Math.max(min,Math.min(max,value));
const interactive=(target:EventTarget|null)=>target instanceof HTMLElement&&!!target.closest("button,a,input,select,textarea,summary,[contenteditable]:not([contenteditable=false]),[role=slider]");
const time=(value:number)=>{const seconds=Math.max(0,Math.round(value));return `${Math.floor(seconds/60)}:${String(seconds%60).padStart(2,"0")}`};
export const MEDIA_STEPS={normal:5,fast:30,slow:1,forwardRate:2,fastRate:4,slowRate:1.25} as const;

/** Horizontal seeking never pauses playback. The surrounding dock owns its input. */
export default function useMediaInteractions(options:Options){
  const latest=useRef(options);latest.current=options;
  const pad=useRef<HTMLDivElement>(null),gesture=useRef<Gesture|null>(null),pointers=useRef(new Set<number>());
  const hide=useRef<ReturnType<typeof setTimeout>>();
  const alive=useRef(true),playPending=useRef(false),cancelClick=useRef(false),tap=useRef<{at:number;x:number;y:number}|null>(null);
  const [feedback,setFeedback]=useState(""),[playing,setPlaying]=useState(false);
  const acceleration=useRef(createPlaybackAcceleration()),keyboardSpeed=useRef(false);
  const announce=useCallback((message:string,persistent=false)=>{clearTimeout(hide.current);if(!alive.current)return;setFeedback(message);if(!persistent)hide.current=setTimeout(()=>{if(alive.current)setFeedback("")},1600)},[]);
  const stop=useCallback((resume=true)=>{
    const accelerated=acceleration.current.stop();keyboardSpeed.current=false;
    const previous=gesture.current;gesture.current=null;
    if(previous&&pad.current?.hasPointerCapture(previous.id))pad.current.releasePointerCapture(previous.id);
    if(previous?.mode==="seek"&&previous.wasPlaying&&resume){const element=latest.current.videoElement.current;if(element?.paused&&!element.ended)void element.play().catch(()=>announce("已定位，双击或按空格继续播放"))}
    if(accelerated)announce(accelerated.wasPlaying?`恢复 ${accelerated.rate.toFixed(1)}× 播放`:"已暂停");
    else if(previous?.mode==="seek")announce(`已定位 ${time(previous.target)}`);
  },[announce]);
  const fullscreen=useMediaFullscreen(options.stageElement,options.active!==false&&options.enabled);
  const toggleFullscreen=useCallback(()=>{stop();void fullscreen.toggle().then(()=>pad.current?.focus({preventScroll:true}))},[fullscreen.toggle,stop]);
  useEffect(()=>{stop(false)},[fullscreen.active,stop]);
  const toggle=useCallback(()=>{
    const current=latest.current;if(current.active===false||!current.video||!current.enabled||playPending.current)return;
    const element=current.videoElement.current;
    if(!current.source||!element){current.onRequestPlay();return}
    if(!element.paused){element.pause();announce("已暂停");return}
    playPending.current=true;const key=current.mediaKey;
    void element.play().then(()=>{if(alive.current&&latest.current.mediaKey===key)announce("播放")}).catch(()=>{if(alive.current&&latest.current.mediaKey===key)announce("视频已就绪，双击或按空格播放")}).finally(()=>{if(latest.current.mediaKey===key)playPending.current=false});
  },[announce]);
  const seek=useCallback((seconds:number,target?:number)=>{
    const element=latest.current.videoElement.current;
    if(!element||!latest.current.source||!Number.isFinite(element.duration)||element.duration<=0){announce("先播放视频，再调整进度");return null}
    // Accumulate a target while decoding is busy; never flood the decoder with seeks.
    const next=clamp(target??element.currentTime+seconds,0,Math.max(0,element.duration-.05));
    try{element.currentTime=next;return next}catch{return null}
  },[announce]);
  const accelerate=useCallback((multiplier:number)=>{
    const current=latest.current,element=current.videoElement.current;
    if(!element||!current.source||!Number.isFinite(element.duration)||element.duration<=0){announce("先播放视频，再按住加速");return false}
    const rate=acceleration.current.start(element,multiplier,()=>announce("无法加速播放，请先播放视频后重试"));
    if(rate!==null)announce(`${rate.toFixed(2).replace(/0$/,"")}× 加速播放`,true);
    return rate!==null;
  },[announce]);
  const volume=useCallback((value:number)=>{
    const element=latest.current.videoElement.current;
    if(!element){announce("先播放视频，再调整音量");return}
    const next=clamp(value,0,1);
    try{element.volume=next;element.muted=next===0;
      if(Math.abs(element.volume-next)>.01)announce("请用设备音量键调节音量");else announce(`音量 ${Math.round(element.volume*100)}%`,true);
    }catch{announce("请用设备音量键调节音量")}
  },[announce]);
  const pointerDown=(event:ReactPointerEvent<HTMLDivElement>)=>{
    if(event.pointerType==="mouse"&&event.button!==0)return;
    pointers.current.add(event.pointerId);
    if(pointers.current.size>1||!event.isPrimary){tap.current=null;cancelClick.current=true;stop(false);return}
    if(!latest.current.video||!latest.current.enabled)return;
    stop(false);
    const rect=event.currentTarget.getBoundingClientRect();
    cancelClick.current=false;gesture.current={id:event.pointerId,x:event.clientX,y:event.clientY,width:rect.width,mode:"pending",start:0,target:0,applied:0,wasPlaying:false};
    event.currentTarget.setPointerCapture(event.pointerId);
  };
  const pointerMove=(event:ReactPointerEvent<HTMLDivElement>)=>{
    const g=gesture.current;if(!g||g.id!==event.pointerId)return;
    const dx=event.clientX-g.x,dy=event.clientY-g.y;
    if(g.mode==="pending"&&Math.abs(dy)>10&&Math.abs(dy)>Math.abs(dx)){g.mode="ignored";cancelClick.current=true;tap.current=null;return}
    const element=latest.current.videoElement.current;
    if(g.mode==="pending"&&Math.abs(dx)>10&&Math.abs(dx)>Math.abs(dy)*1.15){
      cancelClick.current=true;tap.current=null;
      if(!element||!latest.current.source||!Number.isFinite(element.duration)||element.duration<=0){stop(false);announce("先播放视频，再调整进度");return}
      g.mode="seek";g.start=element.currentTime;g.applied=g.start;g.wasPlaying=!element.paused;
    }
    if(g.mode!=="seek"||!element)return;
    g.target=clamp(g.start+dx/Math.max(1,g.width)*Math.min(120,element.duration),0,Math.max(0,element.duration-.05));
    if(!element.seeking){g.applied=g.target;seek(0,g.target)}
    if(g.wasPlaying&&element.paused&&!element.ended)void element.play().catch(()=>announce("已定位，双击或按空格继续播放"));
    announce(`${dx>=0?"快进":"快退"} · ${time(g.target)}`,true);
  };
  const pointerEnd=(event:ReactPointerEvent<HTMLDivElement>,canceled=false)=>{
    pointers.current.delete(event.pointerId);const g=gesture.current;if(!g||g.id!==event.pointerId)return;
    const moved=g.mode!=="pending"||Math.hypot(event.clientX-g.x,event.clientY-g.y)>10;
    if(!canceled&&g.mode==="seek"&&Math.abs(g.target-g.applied)>.02)seek(0,g.target);
    stop(!canceled);if(canceled||moved){cancelClick.current=true;tap.current=null;return}
    // Use pointer taps for mouse and touch alike; no native double-click/zoom conflict.
    const now=performance.now(),previous=tap.current;
    if(previous&&now-previous.at<330&&Math.hypot(event.clientX-previous.x,event.clientY-previous.y)<35){tap.current=null;toggle()}
    else tap.current={at:now,x:event.clientX,y:event.clientY};
  };
  useLayoutEffect(()=>{alive.current=true;return()=>{alive.current=false;acceleration.current.stop();clearTimeout(hide.current);gesture.current=null;pointers.current.clear()}},[]);
  useEffect(()=>{
    stop(false);pointers.current.clear();tap.current=null;playPending.current=false;setFeedback("");
    const focus=requestAnimationFrame(()=>{
      // A preview becoming ready must not steal focus from an action the user
      // has already started (including the first press of a confirmation).
      if(latest.current.active!==false&&!document.activeElement?.closest(".preview-actions,.detail-function-panel,.public-viewer-dock"))pad.current?.focus({preventScroll:true});
    });
    return()=>{cancelAnimationFrame(focus);stop(false)};
  },[options.mediaKey,options.source,stop]);
  useEffect(()=>{if(options.active===false||!options.enabled){stop(false);tap.current=null;pointers.current.clear()}},[options.active,options.enabled,stop]);
  useEffect(()=>{
    const media=options.videoElement.current;
    if(!media){setPlaying(false);return}
    const update=()=>setPlaying(!media.paused);
    const seeked=()=>{const g=gesture.current;if(g?.mode!=="seek")return;if(Math.abs(g.target-g.applied)>.02){g.applied=g.target;seek(0,g.target)}if(g.wasPlaying&&media.paused&&!media.ended)void media.play().catch(()=>announce("已定位，双击或按空格继续播放"))};
    update();media.addEventListener("play",update);media.addEventListener("pause",update);media.addEventListener("ended",update);
    media.addEventListener("seeked",seeked);
    return()=>{media.removeEventListener("play",update);media.removeEventListener("pause",update);media.removeEventListener("ended",update);media.removeEventListener("seeked",seeked)};
  },[options.source,options.mediaKey,options.videoElement,announce,seek]);
  useEffect(()=>{
    const cancel=()=>{stop(false);tap.current=null;pointers.current.clear()};
    const visibility=()=>{if(document.hidden)cancel()};
    const end=(event:PointerEvent)=>pointers.current.delete(event.pointerId);
    const extraPointer=(event:PointerEvent)=>{if(gesture.current&&event.pointerId!==gesture.current.id){pointers.current.add(event.pointerId);cancelClick.current=true;tap.current=null;stop(false)}};
    window.addEventListener("blur",cancel);window.addEventListener("resize",cancel);document.addEventListener("visibilitychange",visibility);window.addEventListener("pointerup",end,true);window.addEventListener("pointercancel",end,true);window.addEventListener("pointerdown",extraPointer,true);
    const key=(event:KeyboardEvent)=>{
      const current=latest.current;if(current.active===false||event.defaultPrevented||event.altKey||event.metaKey||interactive(event.target))return;
      // A separately opened dialog owns focus and its own shortcuts.
      const owner=pad.current?.closest('[role="dialog"]'),dialogs=Array.from(document.querySelectorAll('[role="dialog"][aria-modal="true"]'));
      if(owner&&dialogs[dialogs.length-1]!==owner)return;
      if(event.key==="PageUp"||event.key==="PageDown"){event.preventDefault();if(!event.repeat)(event.key==="PageUp"?current.onPrev:current.onNext)?.();return}
      if(!current.enabled)return;
      if(event.key.toLowerCase()==="f"&&!event.ctrlKey&&(current.video||current.fullscreenImages)){event.preventDefault();if(!event.repeat)toggleFullscreen();return}
      if(!current.video)return;
      if(event.code==="Space"||event.key===" "){event.preventDefault();if(!event.repeat){if(keyboardSpeed.current)stop();toggle()}}
      else if(event.key==="ArrowRight"){event.preventDefault();if(gesture.current)stop(false);keyboardSpeed.current=accelerate(event.ctrlKey?MEDIA_STEPS.fastRate:event.shiftKey?MEDIA_STEPS.slowRate:MEDIA_STEPS.forwardRate)}
      else if(event.key==="ArrowLeft"){event.preventDefault();if(keyboardSpeed.current)stop();const value=seek(-(event.ctrlKey?MEDIA_STEPS.fast:event.shiftKey?MEDIA_STEPS.slow:MEDIA_STEPS.normal));if(value!==null)announce(`定位 ${time(value)}`)}
      else if(event.key==="ArrowUp"||event.key==="ArrowDown"){event.preventDefault();const element=current.videoElement.current;volume((element?.muted?0:element?.volume??1)+(event.key==="ArrowUp"?.05:-.05));clearTimeout(hide.current);hide.current=setTimeout(()=>{if(alive.current)setFeedback("")},1600)}
    };
    const keyUp=(event:KeyboardEvent)=>{if(event.key==="ArrowRight"&&keyboardSpeed.current)stop()};
    document.addEventListener("keydown",key);document.addEventListener("keyup",keyUp);
    return()=>{document.removeEventListener("keydown",key);document.removeEventListener("keyup",keyUp);window.removeEventListener("blur",cancel);window.removeEventListener("resize",cancel);document.removeEventListener("visibilitychange",visibility);window.removeEventListener("pointerup",end,true);window.removeEventListener("pointercancel",end,true);window.removeEventListener("pointerdown",extraPointer,true)};
  },[accelerate,announce,seek,stop,toggle,toggleFullscreen,volume]);
  const active=options.active!==false;
  const overlay=<div className={`media-interactions${active?"":" is-inactive"}`}>
    <button type="button" className="media-edge-nav media-edge-prev" aria-label="上一张" disabled={!options.onPrev||!active} onClick={options.onPrev}><IconChevronLeft size={28} aria-hidden="true"/></button>
    <div ref={pad} className="media-gesture-pad" tabIndex={active?0:-1} role="group" aria-label={options.video?"视频手势区域":"图片浏览区域"} aria-keyshortcuts="PageUp PageDown Space ArrowLeft ArrowRight ArrowUp ArrowDown F"
      style={options.video?undefined:{pointerEvents:"none"}} onPointerDown={pointerDown} onPointerMove={pointerMove} onPointerUp={event=>pointerEnd(event)} onPointerCancel={event=>pointerEnd(event,true)} onLostPointerCapture={event=>{if(gesture.current?.id===event.pointerId){cancelClick.current=true;stop(false)}}} onContextMenu={event=>{if(options.video)event.preventDefault()}} onClick={event=>{if(cancelClick.current)event.preventDefault()}} />
    <button type="button" className="media-edge-nav media-edge-next" aria-label="下一张" disabled={!options.onNext||!active} onClick={options.onNext}><IconChevronRight size={28} aria-hidden="true"/></button>
    {(!options.toolbar||fullscreen.active)&&active&&(options.video||options.fullscreenImages)&&(options.source||fullscreen.active)&&options.enabled&&<button type="button" className="media-fullscreen-toggle" aria-label={fullscreen.active?"退出全屏":options.video?"全屏播放":"全屏查看"} aria-keyshortcuts="F" onClick={toggleFullscreen}>{fullscreen.active?<IconArrowsMinimize size={20} aria-hidden="true"/>:<IconArrowsMaximize size={20} aria-hidden="true"/>}</button>}
    {!options.toolbar&&active&&options.enabled&&options.video&&options.source&&!playing&&<button type="button" className="media-center-play" aria-label="播放视频" onClick={toggle}><IconPlayerPlay size={28} aria-hidden="true"/></button>}
    {!!feedback&&active&&<FloatingFeedback role="status" persistent>{feedback}</FloatingFeedback>}
  </div>;
  return {brightness:1,playing,overlay,announce,toggle,seek,stop,toggleFullscreen,fullscreenActive:fullscreen.active,expanded:fullscreen.expanded};
}
