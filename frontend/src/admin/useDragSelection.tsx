import {useCallback,useEffect,useRef,useState} from "react";
import type {PointerEvent as ReactPointerEvent,RefObject} from "react";
import {galleryViewportTop} from "./galleryViewport";
import "./drag-selection.css";

type Options={root:RefObject<HTMLDivElement>;enabled:boolean;scope:string;selected:Set<string>;onHit:(ids:string[])=>void;onRestore:(ids:Set<string>)=>void};
type Drag={id:number;touch:boolean;active:boolean;startX:number;startY:number;lastX:number;lastY:number;x:number;y:number;base:Set<string>;visited:Set<string>};
const cardAt=(target:EventTarget|null)=>target instanceof Element?target.closest<HTMLElement>('[data-file-id]'):null;

// Clip the actual pointer segment against a card. This also catches cards crossed
// between sparse pointer events, without selecting the rectangle around a stroke.
function crossesCard(x:number,y:number,nextX:number,nextY:number,rect:DOMRect){
  let enter=0,exit=1;
  for(const [start,delta,min,max] of [[x,nextX-x,rect.left+window.scrollX,rect.right+window.scrollX],[y,nextY-y,rect.top+window.scrollY,rect.bottom+window.scrollY]]){
    if(delta===0){if(start<min||start>max)return false;continue}
    const a=(min-start)/delta,b=(max-start)/delta;
    enter=Math.max(enter,Math.min(a,b));exit=Math.min(exit,Math.max(a,b));
    if(enter>exit)return false;
  }
  return true;
}

/** Each card crossed by a stroke toggles once, with immediate feedback. */
export default function useDragSelection(options:Options){
  const latest=useRef(options);latest.current=options;
  const drag=useRef<Drag|null>(null),timer=useRef<ReturnType<typeof setTimeout>>(),frame=useRef<number>(),suppress=useRef(false),alive=useRef(true);
  const [active,setActive]=useState(false);
  const mode=options.enabled&&(options.selected.size>0||active);
  const collect=useCallback(()=>{
    const g=drag.current,root=latest.current.root.current;if(!g?.active||!root)return;
    const x=g.x+window.scrollX,y=g.y+window.scrollY;
    const hits:string[]=[];
    root.querySelectorAll<HTMLElement>('[data-file-id]:not([data-selection-disabled])').forEach(card=>{
      const id=card.dataset.fileId;if(!id||g.visited.has(id))return;
      const rect=card.getBoundingClientRect();
      if(rect.width>0&&rect.height>0&&crossesCard(g.lastX,g.lastY,x,y,rect)){g.visited.add(id);hits.push(id)}
    });
    g.lastX=x;g.lastY=y;
    if(hits.length)latest.current.onHit(hits);
  },[]);
  const stop=useCallback((restore=false)=>{
    clearTimeout(timer.current);if(frame.current!==undefined)cancelAnimationFrame(frame.current);frame.current=undefined;
    const previous=drag.current;drag.current=null;
    if(previous?.active){suppress.current=true;if(restore)latest.current.onRestore(previous.base)}
    if(alive.current)setActive(false);
  },[]);
  const animate=useCallback(function tick(){
    frame.current=undefined;
    const g=drag.current;if(!g?.active)return;
    collect();
    const edge=48,top=galleryViewportTop(),barTop=document.querySelector('.batch-bar')?.getBoundingClientRect().top;
    const bottom=barTop&&barTop>top?Math.min(window.innerHeight,barTop):window.innerHeight-24;
    const speed=g.y<top+edge?-Math.min(18,(top+edge-g.y)*.3):g.y>bottom-edge?Math.min(18,(g.y-bottom+edge)*.3):0;
    if(speed)window.scrollBy({top:speed,behavior:"instant" as ScrollBehavior});
    frame.current=requestAnimationFrame(tick);
  },[collect]);
  const activate=useCallback(()=>{
    const g=drag.current;if(!g||g.active||!latest.current.enabled)return;
    g.active=true;suppress.current=true;setActive(true);
    collect();frame.current=requestAnimationFrame(animate);
  },[animate,collect]);
  const pointerDown=(event:ReactPointerEvent<HTMLDivElement>)=>{
    suppress.current=false;
    if(!latest.current.enabled||event.button!==0||!event.isPrimary)return;
    const target=event.target as Element;
    if(target.closest('input,select,textarea,a,.photo-group-heading,button:not(.card-open):not([data-selection-open])'))return;
    const card=cardAt(target);if(card?.hasAttribute('data-selection-disabled'))return;
    stop();suppress.current=false;
    drag.current={id:event.pointerId,touch:event.pointerType==="touch",active:false,startX:event.clientX+window.scrollX,startY:event.clientY+window.scrollY,lastX:event.clientX+window.scrollX,lastY:event.clientY+window.scrollY,x:event.clientX,y:event.clientY,base:new Set(latest.current.selected),visited:new Set()};
    if(event.pointerType==="touch"&&latest.current.selected.size>0){event.preventDefault();activate()}
    else if(event.pointerType==="touch")timer.current=setTimeout(activate,380);
  };
  useEffect(()=>{
    alive.current=true;
    const move=(event:PointerEvent)=>{
      const g=drag.current;if(!g||g.id!==event.pointerId)return;
      const points=event.getCoalescedEvents?.()||[];
      for(const point of [...points,event]){
        g.x=point.clientX;g.y=point.clientY;
        if(!g.active&&Math.hypot(g.x+window.scrollX-g.startX,g.y+window.scrollY-g.startY)>6){if(g.touch){stop();return}activate()}
        if(g.active){event.preventDefault();collect()}
      }
    };
    const end=(event:PointerEvent)=>{const g=drag.current;if(!g||g.id!==event.pointerId)return;if(g.active){g.x=event.clientX;g.y=event.clientY;collect()}stop()};
    const cancel=(event:PointerEvent)=>{if(drag.current?.id===event.pointerId)stop()};
    const extra=(event:PointerEvent)=>{if(drag.current&&event.pointerId!==drag.current.id)stop()};
    const touchMove=(event:TouchEvent)=>{const g=drag.current;if(!g?.touch||!g.active)return;if(event.touches.length!==1){stop();return}event.preventDefault();g.x=event.touches[0].clientX;g.y=event.touches[0].clientY;collect()};
    const touchEnd=()=>{if(drag.current?.touch){collect();stop()}};
    const blur=()=>stop();
    const visibility=()=>{if(document.hidden)stop()};
    const escape=(event:KeyboardEvent)=>{if(event.key!=="Escape"||!drag.current?.active)return;event.preventDefault();event.stopPropagation();stop(true)};
    window.addEventListener("pointermove",move,{passive:false});window.addEventListener("pointerup",end,true);window.addEventListener("pointercancel",cancel,true);window.addEventListener("pointerdown",extra,true);
    window.addEventListener("touchmove",touchMove,{passive:false,capture:true});window.addEventListener("touchend",touchEnd,true);window.addEventListener("touchcancel",touchEnd,true);
    window.addEventListener("blur",blur);window.addEventListener("resize",blur);document.addEventListener("visibilitychange",visibility);document.addEventListener("keydown",escape,true);
    return()=>{alive.current=false;clearTimeout(timer.current);if(frame.current!==undefined)cancelAnimationFrame(frame.current);drag.current=null;window.removeEventListener("pointermove",move);window.removeEventListener("pointerup",end,true);window.removeEventListener("pointercancel",cancel,true);window.removeEventListener("pointerdown",extra,true);window.removeEventListener("touchmove",touchMove,true);window.removeEventListener("touchend",touchEnd,true);window.removeEventListener("touchcancel",touchEnd,true);window.removeEventListener("blur",blur);window.removeEventListener("resize",blur);document.removeEventListener("visibilitychange",visibility);document.removeEventListener("keydown",escape,true)};
  },[activate,collect,stop]);
  useEffect(()=>{stop()},[options.scope,options.enabled,stop]);
  return {active,mode,bind:{onPointerDownCapture:pointerDown,onClickCapture:(event:React.MouseEvent)=>{if(suppress.current&&event.detail!==0){event.preventDefault();event.stopPropagation();suppress.current=false}},onDragStartCapture:(event:React.DragEvent)=>{if(cardAt(event.target))event.preventDefault()},onContextMenuCapture:(event:React.MouseEvent)=>{if(drag.current?.active)event.preventDefault()}}};
}
