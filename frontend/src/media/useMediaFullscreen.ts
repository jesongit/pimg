import {useCallback,useEffect,useRef,useState} from "react";
import type {RefObject} from "react";

/** Fullscreen the complete player so its gesture layer stays with the video. */
export default function useMediaFullscreen(stage:RefObject<HTMLDivElement>,enabled:boolean){
  const [native,setNative]=useState(false),[expanded,setExpanded]=useState(false);
  const alive=useRef(true),busy=useRef(false),available=useRef(enabled);available.current=enabled;
  const exit=useCallback(async()=>{
    setExpanded(false);
    if(stage.current&&document.fullscreenElement===stage.current){try{await document.exitFullscreen()}catch{/* Browser may already be exiting. */}}
  },[stage]);
  const toggle=useCallback(async()=>{
    const element=stage.current;if(!element||!enabled||busy.current)return;
    if(expanded||document.fullscreenElement===element){await exit();return}
    busy.current=true;
    try{
      if(element.requestFullscreen&&document.fullscreenEnabled!==false){await element.requestFullscreen();if(!available.current&&document.fullscreenElement===element)await document.exitFullscreen()}
      else if(alive.current&&available.current)setExpanded(true);
    }catch{if(alive.current&&available.current&&stage.current===element)setExpanded(true)}
    finally{busy.current=false}
  },[enabled,expanded,exit,stage]);
  useEffect(()=>{
    alive.current=true;
    const element=stage.current;
    const changed=()=>{setNative(!!element&&document.fullscreenElement===element)};
    document.addEventListener("fullscreenchange",changed);changed();
    return()=>{alive.current=false;document.removeEventListener("fullscreenchange",changed);if(element&&document.fullscreenElement===element)void document.exitFullscreen().catch(()=>{})};
  },[stage]);
  useEffect(()=>{if(!enabled)void exit()},[enabled,exit]);
  useEffect(()=>{
    if(!expanded)return;
    const previous=document.body.style.overflow;document.body.style.overflow="hidden";
    return()=>{document.body.style.overflow=previous};
  },[expanded]);
  useEffect(()=>{
    if(!native&&!expanded)return;
    const escape=(event:KeyboardEvent)=>{if(event.key!=="Escape")return;event.preventDefault();event.stopImmediatePropagation();void exit()};
    document.addEventListener("keydown",escape,true);
    return()=>document.removeEventListener("keydown",escape,true);
  },[native,expanded,exit]);
  return {active:native||expanded,expanded,toggle};
}
