import {useCallback,useRef,useState} from "react";
import type {ReactNode} from "react";
import {createPortal} from "react-dom";
import {MediaFullscreenContext,useFullscreenController} from "./useMediaFullscreen";
import {MediaPlaybackContext} from "./MediaPlaybackContext";
import "./media-interactions.css";

/** This portal host survives keyed media/drawer replacement within one viewer. */
export default function MediaFullscreenSession({children}:{children:ReactNode}){
  const root=useRef<HTMLDivElement|null>(null),[portal,setPortal]=useState<HTMLDivElement|null>(null);
  const attach=useCallback((element:HTMLDivElement|null)=>{root.current=element;setPortal(element)},[]);
  const fullscreen=useFullscreenController(root,true,false);
  const [muted,setMuted]=useState(true);
  return <MediaPlaybackContext.Provider value={{muted,setMuted}}><MediaFullscreenContext.Provider value={{...fullscreen,expanded:fullscreen.active,portal:portal||undefined}}>
    {createPortal(<div ref={attach} className={`media-fullscreen-session${fullscreen.active?" is-expanded":""}`}/>,document.body)}
    {portal&&createPortal(children,portal)}
  </MediaFullscreenContext.Provider></MediaPlaybackContext.Provider>;
}
