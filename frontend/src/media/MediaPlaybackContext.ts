import {createContext} from "react";

/** One open detail session owns sound across keyed files, including image stops. */
export const MediaPlaybackContext=createContext<{muted:boolean;setMuted:(muted:boolean)=>void}|null>(null);
