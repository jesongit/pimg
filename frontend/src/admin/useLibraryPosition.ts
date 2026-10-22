import {useLayoutEffect,useRef,useState} from "react";
import type {RefObject} from "react";
import {galleryViewportTop} from "./galleryViewport";

type Position={y:number;id?:string;top?:number;width?:number;first?:string};
const positions=new Map<string,Position>();
const storageKey=(key:string)=>`pimg:gallery-position:${key}`;
function saved(key:string):Position{
 const known=positions.get(key);if(known)return known;
 try{return {y:Math.max(0,Number(sessionStorage.getItem(storageKey(key)))||0)}}catch{return {y:0}}
}
function remember(key:string,value:Position){
 positions.delete(key);positions.set(key,value);
 while(positions.size>96){const oldest=positions.keys().next().value!;positions.delete(oldest);try{sessionStorage.removeItem(storageKey(oldest))}catch{}}
 try{sessionStorage.setItem(storageKey(key),String(value.y))}catch{}
}
for(const event of ["pimg:auth-changed","pimg:unauthorized"])window.addEventListener(event,()=>positions.clear());

// Keep the visible card fixed as the measured grid replaces its initial width.
// Only coordinates are persisted; card identities stay in bounded memory.
export default function useLibraryPosition(key:string,ready:boolean,root:RefObject<HTMLDivElement>){
 const state=useRef({key,target:saved(key),consumed:false});
 if(state.current.key!==key)state.current={key,target:saved(key),consumed:false};
 const [finished,setFinished]=useState("");
 useLayoutEffect(()=>{
  if(!ready)return;
  const view=state.current;
  let restoring=!view.consumed,frame=0,stopped=false,passes=0;
  const capture=()=>{
   if(stopped||restoring||!root.current?.isConnected)return;
   const cards=Array.from(root.current?.querySelectorAll<HTMLElement>("[data-file-id]")||[]);
   const card=cards.find(item=>item.getBoundingClientRect().bottom>galleryViewportTop());
   remember(key,{y:window.scrollY,width:document.documentElement.clientWidth,first:cards[0]?.dataset.fileId,...(card?{id:card.dataset.fileId,top:card.getBoundingClientRect().top}:{})});
  };
  const finish=()=>{restoring=false;view.consumed=true;setFinished(key);capture()};
  const restore=()=>{
   if(stopped||!restoring)return;
   const card=view.target.id?Array.from(root.current?.querySelectorAll<HTMLElement>("[data-file-id]")||[]).find(item=>item.dataset.fileId===view.target.id):null;
   const changedLayout=!!view.target.width&&view.target.width!==document.documentElement.clientWidth||!!view.target.first&&view.target.first!==root.current?.querySelector<HTMLElement>("[data-file-id]")?.dataset.fileId;
   // Same viewport and page: restore the exact coordinate, avoiding temporary
   // first-frame row measurements being mistaken for a real content shift.
   const top=card&&changedLayout?window.scrollY+card.getBoundingClientRect().top-(view.target.top||0):view.target.y;
   window.scrollTo({top:Math.max(0,top),behavior:"instant" as ScrollBehavior});
   if(++passes<3)frame=requestAnimationFrame(restore);else finish();
  };
  const interrupt=()=>{if(restoring){cancelAnimationFrame(frame);finish()}};
  if(restoring)restore();else setFinished(key);
  window.addEventListener("scroll",capture,{passive:true});
  for(const name of ["wheel","touchstart","pointerdown","keydown"])window.addEventListener(name,interrupt,{passive:true});
  return()=>{
   // React may already have changed the document geometry during unmount.
   // Keep the last scroll-event snapshot rather than reading the next page.
   stopped=true;cancelAnimationFrame(frame);
   window.removeEventListener("scroll",capture);
   for(const name of ["wheel","touchstart","pointerdown","keydown"])window.removeEventListener(name,interrupt);
  };
 },[key,ready,root]);
 return {ready:finished===key,cancelRestore:()=>{state.current.consumed=true;setFinished(key)}};
}
