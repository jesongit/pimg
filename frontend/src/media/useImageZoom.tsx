import {useCallback,useEffect,useLayoutEffect,useRef,useState} from "react";
import type {CSSProperties,PointerEvent as ReactPointerEvent,RefObject} from "react";
import "./image-zoom.css";

type View={scale:number;x:number;y:number};
type Options={mediaKey:string;enabled:boolean;active:boolean;stage:RefObject<HTMLDivElement>};
type Drag={id:number;x:number;y:number;origin:View};
const minimum=.25,maximum=8,initial:View={scale:1,x:0,y:0};
const clamp=(value:number,min:number,max:number)=>Math.min(max,Math.max(min,value));
const editing=(target:EventTarget|null)=>target instanceof Element&&!!target.closest('input,select,textarea,[contenteditable]:not([contenteditable="false"]),[role="textbox"],[role="combobox"]');

/** Image transforms remain local to the viewer; the media source is unchanged. */
export default function useImageZoom(options:Options){
 const latest=useRef(options);latest.current=options;
 const [view,setView]=useState<View>(initial),current=useRef<View>(initial),drag=useRef<Drag|null>(null),suppressClick=useRef(false);
 const [dragging,setDragging]=useState(false);
 const available=useCallback(()=>{
  const value=latest.current;if(!value.enabled||!value.active||!value.stage.current)return false;
  const owner=value.stage.current.closest('[role="dialog"]'),dialogs=document.querySelectorAll('[role="dialog"][aria-modal="true"]');
  return !owner||dialogs[dialogs.length-1]===owner;
 },[]);
 const commit=useCallback((next:View)=>{
  const stage=latest.current.stage.current;
  const images=stage?.querySelectorAll<HTMLImageElement>('img.media-visual:not(.original-pending)');
  const image=images?.[images.length-1];
  let maxX=0,maxY=0;
  if(stage&&image?.naturalWidth&&image.naturalHeight){
   const fit=Math.min(image.clientWidth/image.naturalWidth,image.clientHeight/image.naturalHeight);
   maxX=Math.max(0,(image.naturalWidth*fit*next.scale-stage.clientWidth)/2);
   maxY=Math.max(0,(image.naturalHeight*fit*next.scale-stage.clientHeight)/2);
  }
  const value={scale:clamp(next.scale,minimum,maximum),x:clamp(next.x,-maxX,maxX),y:clamp(next.y,-maxY,maxY)};
  const previous=current.current;
  if(value.scale===previous.scale&&Math.abs(value.x-previous.x)<.01&&Math.abs(value.y-previous.y)<.01)return;
  current.current=value;setView(value);
 },[]);
 const constrain=useCallback(()=>commit(current.current),[commit]);
 const scaleTo=useCallback((value:number,point?:{x:number;y:number})=>{
  if(!available())return;
  const before=current.current,scale=clamp(value,minimum,maximum),rect=latest.current.stage.current!.getBoundingClientRect();
  if(scale===before.scale)return;
  const x=point?point.x-rect.left-rect.width/2:0,y=point?point.y-rect.top-rect.height/2:0,ratio=scale/before.scale;
  commit({scale,x:x-(x-before.x)*ratio,y:y-(y-before.y)*ratio});
 },[available,commit]);
 const stop=useCallback(()=>{
  const previous=drag.current;drag.current=null;
  const stage=latest.current.stage.current;
  if(previous&&stage?.hasPointerCapture?.(previous.id))stage.releasePointerCapture(previous.id);
  setDragging(false);
 },[]);
 const reset=useCallback(()=>{stop();commit(initial)},[commit,stop]);
 useLayoutEffect(()=>{reset();suppressClick.current=false},[options.mediaKey,reset]);
 useEffect(()=>{if(!options.enabled)reset();if(!options.active)stop()},[options.enabled,options.active,reset,stop]);
 useEffect(()=>{
  if(!options.enabled)return;
  const stage=options.stage.current;if(!stage)return;
  const wheel=(event:WheelEvent)=>{
   if(!available()||editing(event.target)||!event.deltaY)return;
   event.preventDefault();
   const pixels=event.deltaY*(event.deltaMode===1?16:event.deltaMode===2?stage.clientHeight:1);
   scaleTo(current.current.scale*Math.exp(-clamp(pixels,-120,120)*.002),{x:event.clientX,y:event.clientY});
  };
  const key=(event:KeyboardEvent)=>{
   if(!available()||event.defaultPrevented||!event.ctrlKey||event.altKey||event.metaKey||editing(event.target))return;
   if(!["+","=","-","_","0"].includes(event.key))return;
   event.preventDefault();
   if(event.key==="0")reset();else scaleTo(current.current.scale+(["+","="].includes(event.key)?.25:-.25));
  };
  const resized=()=>{stop();constrain()};
  const hidden=()=>{if(document.hidden)stop()};
  stage.addEventListener("wheel",wheel,{passive:false});document.addEventListener("keydown",key);
  window.addEventListener("resize",resized);window.addEventListener("blur",stop);document.addEventListener("visibilitychange",hidden);
  const observer=typeof ResizeObserver!=="undefined"?new ResizeObserver(resized):null;observer?.observe(stage);
  return()=>{stage.removeEventListener("wheel",wheel);document.removeEventListener("keydown",key);window.removeEventListener("resize",resized);window.removeEventListener("blur",stop);document.removeEventListener("visibilitychange",hidden);observer?.disconnect();const previous=drag.current;drag.current=null;if(previous&&stage.hasPointerCapture?.(previous.id))stage.releasePointerCapture(previous.id)};
 },[options.enabled,options.stage,available,scaleTo,reset,stop,constrain]);
 const down=(event:ReactPointerEvent<HTMLDivElement>)=>{
  suppressClick.current=false;
  if(!event.isPrimary){stop();return}
  if(!available()||current.current.scale<=1||event.button!==0||event.target instanceof Element&&event.target.closest('button,a,input,select,textarea'))return;
  drag.current={id:event.pointerId,x:event.clientX,y:event.clientY,origin:current.current};
  event.currentTarget.setPointerCapture(event.pointerId);setDragging(true);event.preventDefault();event.stopPropagation();
 };
 const move=(event:ReactPointerEvent<HTMLDivElement>)=>{
  const previous=drag.current;if(!previous||previous.id!==event.pointerId)return;
  commit({...previous.origin,x:previous.origin.x+event.clientX-previous.x,y:previous.origin.y+event.clientY-previous.y});
  suppressClick.current=true;event.preventDefault();event.stopPropagation();
 };
 const end=(event:ReactPointerEvent<HTMLDivElement>)=>{
  if(drag.current?.id!==event.pointerId)return;
  stop();event.preventDefault();event.stopPropagation();
 };
 return {scale:view.scale,zoomed:view.scale>1,dragging,constrain,reset,zoomIn:()=>scaleTo(current.current.scale+.25),zoomOut:()=>scaleTo(current.current.scale-.25),style:{"--image-scale":view.scale,"--image-x":`${view.x}px`,"--image-y":`${view.y}px`} as CSSProperties,
  bind:{onPointerDownCapture:down,onPointerMoveCapture:move,onPointerUpCapture:end,onPointerCancelCapture:end,onLostPointerCapture:()=>{if(drag.current)stop()},onClickCapture:(event:React.MouseEvent)=>{if(suppressClick.current&&event.detail!==0){suppressClick.current=false;event.preventDefault();event.stopPropagation()}},onDragStartCapture:(event:React.DragEvent)=>{if(options.enabled)event.preventDefault()}}};
}
