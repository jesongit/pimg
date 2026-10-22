import {useCallback,useEffect,useRef,useState} from "react";
import type {galleryApi} from "../api";
import {filesUpdated} from "./interactions";

// An accepted operation can still be waiting for intake, before any visible file
// carries a pending task. Follow the operation itself through that gap.
export default function useOperationRefresh(api:ReturnType<typeof galleryApi>){
 const [ids,setIDs]=useState<number[]>([]),current=useRef(api);current.current=api;
 const watch=useCallback((id:number)=>setIDs(previous=>previous.includes(id)?previous:[...previous,id]),[]);
 useEffect(()=>{setIDs([])},[api]);
 useEffect(()=>{
  if(!ids.length)return;const controller=new AbortController();let timer:ReturnType<typeof setTimeout>,loading=false,stopped=false;
  const load=async()=>{
   if(stopped||loading||document.hidden)return;loading=true;
   const finished:number[]=[];let read=false;
   await Promise.all(ids.map(async id=>{try{const {operation}=await api.get<{operation:{state:string;group?:{input_state?:string}}}>(`/operations/${id}`,controller.signal);read=true;if(!["pending","running"].includes(operation.state)&&!["open","receiving"].includes(operation.group?.input_state||""))finished.push(id)}catch(error:any){if(error.status===404||error.status===403)finished.push(id)}}));
   loading=false;if(stopped||current.current!==api)return;
   if(read)filesUpdated();
   if(finished.length)setIDs(previous=>previous.filter(id=>!finished.includes(id)));
   else timer=setTimeout(()=>void load(),1500);
  };
  const visible=()=>{if(!document.hidden){clearTimeout(timer);void load()}};
  void load();document.addEventListener("visibilitychange",visible);
  return()=>{stopped=true;controller.abort();clearTimeout(timer);document.removeEventListener("visibilitychange",visible)};
 },[ids,api]);
 return {watch,watching:ids.length>0};
}
