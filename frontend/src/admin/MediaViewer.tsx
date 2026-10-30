import { useContext, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { CSSProperties, ReactNode } from "react";
import {IconArrowsMaximize,IconArrowsMinimize,IconCopy,IconGauge,IconPhoto,IconPlayerPause,IconPlayerPlay,IconRewindBackward5,IconRewindForward5,IconViewportWide,IconVolume,IconVolumeOff,IconZoomIn,IconZoomOut} from "@tabler/icons-react";
import type { FileItem, OriginalView } from "../api";
import useMediaInteractions from "../media/useMediaInteractions";
import useImageZoom from "../media/useImageZoom";
import MediaLoading from "../media/MediaLoading";
import { copyOriginalImage } from "../media/copyOriginalImage";
import { useToast } from "../App";
import { MediaPlaybackContext } from "../media/MediaPlaybackContext";
import "../media/media-controls.css";

import { useGallery } from "./GalleryScope";

type Stage = "idle" | "preparing" | "loading" | "decoding" | "ready" | "error";
const playbackRates=[.5,1,1.25,1.5,2,3];
const mediaTime=(value:number)=>{const seconds=Math.floor(Number.isFinite(value)?Math.max(0,value):0);return `${Math.floor(seconds/60)}:${String(seconds%60).padStart(2,"0")}`};

export type MediaSourceAdapter = {
  autoLoad?: boolean;
  compatibleFallback?: string;
  prepare: (compatible: string, retry: boolean, signal: AbortSignal) => Promise<OriginalView>;
  download: string;
};

export async function originalImage(url: string, size: number, signal: AbortSignal, progress: (value: number) => void): Promise<string> {
  const response = await fetch(url, { credentials: "same-origin", signal });
  if (!response.ok) {
    if (response.status === 401) window.dispatchEvent(new Event("pimg:unauthorized"));
    throw new Error("原图读取失败，请重新加载。");
  }
  const reader = response.body?.getReader();
  if (!reader) throw new Error("浏览器无法读取原图，请尝试下载原件。");
  const total = Number(response.headers.get("Content-Length")) || size;
  const chunks: BlobPart[] = [];
  let loaded = 0;
  for (;;) {
    const next = await reader.read();
    if (next.done) break;
    chunks.push(next.value);
    loaded += next.value.byteLength;
    if (total > 0) progress(Math.min(99, Math.floor(loaded * 100 / total)));
  }
  if (size > 0 && loaded !== size) throw new Error("原图没有完整加载，请重试。");
  return URL.createObjectURL(new Blob(chunks, { type: response.headers.get("Content-Type") || "image/jpeg" }));
}

export default function MediaViewer({ file, blocked, status, actions, notice, onPrev, onNext, interactionActive=true, sourceAdapter }: {
  file: FileItem; blocked?: string; status?: ReactNode; actions?: ReactNode; notice?: ReactNode;
  onPrev?:()=>void;onNext?:()=>void;interactionActive?:boolean;
  sourceAdapter?: MediaSourceAdapter;
}) {
  const { api } = useGallery();
  const toast = useToast();
  const sound = useContext(MediaPlaybackContext);
  const [localMuted,setLocalMuted]=useState(true);
  const muted=sound?.muted??localMuted,setMuted=sound?.setMuted??setLocalMuted;
  const [position,setPosition]=useState(0),[duration,setDuration]=useState(0),[rate,setRate]=useState(1);
  const [copying, setCopying] = useState(false);
  const copyController = useRef<AbortController>();
  useEffect(() => { setCopying(false); return () => { copyController.current?.abort(); copyController.current = undefined; }; }, [file.id]);
  const video = file.kind === "video", enabled = file.archive_state === "done" && !blocked;
  const autoLoad = !!sourceAdapter?.autoLoad;
  const [requested, setRequested] = useState(autoLoad || !video && !!file.local_ready), [attempt, setAttempt] = useState(0);
  const [stage, setStage] = useState<Stage>("idle"), [percent, setPercent] = useState(0);
  const [preparationPhase, setPreparationPhase] = useState("");
  const [source, setSource] = useState(""), [visible, setVisible] = useState(false), [error, setError] = useState("");
  const [previewFailed, setPreviewFailed] = useState(false), [previewAttempt, setPreviewAttempt] = useState(0);
  const [partIndex, setPartIndex] = useState(0), [compatible, setCompatible] = useState("");
  const objectURL = useRef(""), dock = useRef<HTMLDivElement>(null), videoElement = useRef<HTMLVideoElement>(null),stageElement=useRef<HTMLDivElement>(null);
  const activeSignal = useRef<AbortSignal>(), heicTried = useRef(false), playNext = useRef(autoLoad && video), expectedTime = useRef(0);
  const requestedRef=useRef(requested);requestedRef.current=requested;
  const parts = file.media_parts || [];
  const preview = !sourceAdapter && enabled && (file.has_thumb || file.kind === "image" || file.kind === "video") ? api.url(`/files/${encodeURIComponent(file.id)}/content?kind=thumb&v=${previewAttempt}`) : "";
  const loading = ["preparing", "loading", "decoding"].includes(stage);
  const copyImage = async () => {
    if (copyController.current || video || !enabled) return;
    const controller = new AbortController(); copyController.current = controller; setCopying(true);
    try {
      await copyOriginalImage(sourceAdapter?.download || api.url(`/files/${encodeURIComponent(file.id)}/download`), file.ext, file.size, controller.signal);
      if (!controller.signal.aborted) toast("原图已复制，可在 QQ、微信中粘贴");
    } catch (failure) {
      if (!controller.signal.aborted) toast(failure instanceof Error ? failure.message : "图片复制失败，请重试。", "err");
    } finally {
      const canceled = controller.signal.aborted;
      controller.abort();
      if (copyController.current === controller) copyController.current = undefined;
      if (!canceled) setCopying(false);
    }
  };
  const requestPlay=()=>{
    if(!enabled||requestedRef.current&&stage!=="error")return;
    requestedRef.current=true;playNext.current=video;setRequested(true);setAttempt(value=>value+1);
  };
  const interactions=useMediaInteractions({mediaKey:`${file.id}:${partIndex}:${compatible}`,video,fullscreenImages:true,toolbar:true,enabled,active:interactionActive,source,videoElement,stageElement,onRequestPlay:requestPlay,onPrev,onNext});
  const zoom=useImageZoom({mediaKey:file.id,enabled:!video&&enabled&&!!(source||preview&&!previewFailed),active:interactionActive,stage:stageElement});
  useLayoutEffect(()=>{zoom.constrain()},[source,visible,previewAttempt,zoom.constrain]);
  useEffect(()=>{setPosition(0);setDuration(0);setRate(1)},[file.id,partIndex,source]);
  useEffect(() => () => { if (objectURL.current) URL.revokeObjectURL(objectURL.current); }, []);
  useEffect(()=>{const media=videoElement.current;return()=>media?.pause()},[source,enabled]);
  useEffect(()=>{
    if(enabled)return;
    videoElement.current?.pause();playNext.current=false;expectedTime.current=0;requestedRef.current=false;
    setRequested(false);setVisible(false);setSource("");setStage("idle");
  },[enabled]);
  useEffect(() => {
    setRequested(autoLoad || !video && !!file.local_ready); setPartIndex(0); setCompatible(""); setVisible(false); setSource(""); heicTried.current = false; playNext.current = autoLoad && video;
  }, [file.id]);
  useEffect(() => {
    const element = dock.current; if (!element) return;
    const workspace = element.closest<HTMLElement>(".detail-docked");
    const update = () => workspace?.style.setProperty("--detail-dock-height", `${element.offsetHeight}px`);
    update(); const observer = new ResizeObserver(update); observer.observe(element); return () => observer.disconnect();
  }, []);
  useEffect(() => {
    if (!requested || !enabled) return;
    let stopped = false, timer: ReturnType<typeof setTimeout> | undefined;
    const controller = new AbortController(); activeSignal.current = controller.signal;
    setStage("preparing"); setPercent(0); setPreparationPhase(""); setError(""); setVisible(false); setSource("");
    const poll = async (retry: boolean) => {
      try {
        const result = sourceAdapter ? await sourceAdapter.prepare(compatible, retry, controller.signal) : await api.post<OriginalView>(`/files/${encodeURIComponent(file.id)}/original`, { retry, ...(file.media_version === 2 ? { part: partIndex, compatible } : {}) });
        if (stopped) return;
        setPreparationPhase(result.phase || "");
        if (sourceAdapter && result.compatible && result.compatible !== compatible) { setCompatible(result.compatible); return; }
        if (result.status === "failed") { if(video && compatible === "remux") { setCompatible("transcode"); return; } throw new Error(result.error || "原件加载失败，请重试。"); }
        if (result.status !== "ready" || !result.url) {
          setPercent(Math.max(0, Math.min(99, result.percent))); timer = setTimeout(() => void poll(false), 600); return;
        }
        setStage("loading"); setPercent(0);
        if (!sourceAdapter) window.dispatchEvent(new CustomEvent("pimg:media-stored", { detail: { id: file.id } }));
        if (video) { setSource(result.url); return; }
        const size = result.compatible ? 0 : result.part?.size || file.size;
        const url = await originalImage(result.url, size, controller.signal, value => { if (!stopped) setPercent(value); });
        if (stopped) { URL.revokeObjectURL(url); return; }
        if (objectURL.current) URL.revokeObjectURL(objectURL.current);
        objectURL.current = url; setSource(url); setStage("decoding");
      } catch (failure: unknown) {
        if (!stopped) { setError(failure instanceof Error ? failure.message : "加载失败，请重试。"); setStage("error"); }
      }
    };
    void poll(true);
    return () => { stopped = true; controller.abort(); clearTimeout(timer); };
  }, [requested, attempt, enabled, file.id, file.size, file.media_version, video, api, partIndex, compatible, sourceAdapter]);
  const ready = () => {
    setVisible(true); setPercent(100); setStage("ready");
    if (videoElement.current) {
      if (expectedTime.current) { videoElement.current.currentTime = expectedTime.current; expectedTime.current = 0; }
      if (playNext.current) { playNext.current = false; void videoElement.current.play().catch(() => { if(!activeSignal.current?.aborted)interactions.announce("视频已就绪，双击或按空格播放"); }); }
    }
  };
  const failed = async () => {
    if(video&&videoElement.current){expectedTime.current=videoElement.current.currentTime;playNext.current=playNext.current||!videoElement.current.paused;}
    if (file.media_version === 2 && !video && /^(heic|heif)$/i.test(file.ext) && !heicTried.current && !compatible && activeSignal.current && objectURL.current) {
      heicTried.current = true; setStage("decoding");
      try {
        const { decodeHEIC } = await import("../media/heic");
        const signal = activeSignal.current, url = await decodeHEIC(objectURL.current, signal);
        if (signal.aborted) { URL.revokeObjectURL(url); return; }
        URL.revokeObjectURL(objectURL.current); objectURL.current = url; setSource(url); return;
      } catch { if (activeSignal.current?.aborted) return; }
    }
    if (video && !compatible && sourceAdapter?.compatibleFallback) { setCompatible(sourceAdapter.compatibleFallback); return; }
    if (file.media_version === 2 && compatible !== (video ? "transcode" : "image")) {
      setCompatible(video ? compatible === "remux" ? "transcode" : "remux" : "image"); return;
    }
    setVisible(false); setSource(""); setStage("error"); setError(video ? "视频暂时无法播放，请重试或下载视频文件。" : "原图暂时无法显示，请重试或下载原件。");
  };
  const choosePart = (index: number, autoplay = false) => { expectedTime.current=0;playNext.current = autoplay;requestedRef.current=true;setPartIndex(index); setCompatible(""); setRequested(true); setAttempt(value => value + 1); };
  const label = video ? "视频" : "原图";
  const phaseLabels: Record<string, string> = { queued: "等待兼容处理", copy: "准备兼容处理", remux: "整理视频封装", transcode: "转换兼容视频", image: "转换兼容图片" };
  const phase = stage === "preparing" ? (phaseLabels[preparationPhase] || `准备${label}`) : stage === "decoding" ? "正在解码原图" : `加载${label}`;
  const videoReady=!!source&&visible,canZoom=!video&&enabled&&!!(source||preview&&!previewFailed);
  const toggleMute=()=>{
    const next=!muted,element=videoElement.current;
    if(element){if(!next&&element.volume===0)element.volume=1;element.muted=next}
    setMuted(next);
  };
  const cycleRate=()=>{
    interactions.stop();const element=videoElement.current;if(!element)return;
    const index=playbackRates.findIndex(value=>Math.abs(element.playbackRate-value)<.01);
    const next=playbackRates[(index+1)%playbackRates.length];
    element.playbackRate=next;setRate(next);
  };
  const seekBy=(seconds:number)=>{interactions.stop();interactions.seek(seconds)};
  const controls=video?[
    {name:stage==="error"?"重试播放视频":interactions.playing?"暂停视频":"播放视频",label:loading&&!videoReady?"加载中":stage==="error"?"重试":interactions.playing?"暂停":"播放",icon:interactions.playing?<IconPlayerPause/>:<IconPlayerPlay/>,action:interactions.toggle,disabled:!enabled||loading&&!videoReady},
    {name:"后退 5 秒",label:"后退",icon:<IconRewindBackward5/>,action:()=>seekBy(-5),disabled:!videoReady},
    {name:"前进 5 秒",label:"前进",icon:<IconRewindForward5/>,action:()=>seekBy(5),disabled:!videoReady},
    {name:`播放速度 ${rate} 倍，点击切换`,label:`${rate}×`,icon:<IconGauge/>,action:cycleRate,disabled:!videoReady},
    {name:muted?"取消静音":"静音",label:muted?"静音":"有声",icon:muted?<IconVolumeOff/>:<IconVolume/>,action:toggleMute,disabled:!enabled},
  ]:[
    {name:stage==="error"?"重试查看原图":"查看原图",label:loading?"加载中":stage==="error"?"重试":"原图",icon:<IconPhoto/>,action:requestPlay,disabled:!enabled||loading||visible&&!compatible},
    {name:"缩小图片",label:"缩小",icon:<IconZoomOut/>,action:zoom.zoomOut,disabled:!canZoom||zoom.scale<=.25},
    {name:"放大图片",label:"放大",icon:<IconZoomIn/>,action:zoom.zoomIn,disabled:!canZoom||zoom.scale>=8},
    {name:"图片适应窗口",label:"适应",icon:<IconViewportWide/>,action:zoom.reset,disabled:!canZoom},
    {name:"复制原图",label:copying?"复制中":"复制",icon:<IconCopy/>,action:()=>void copyImage(),disabled:!enabled||copying},
  ];
  controls.push({name:interactions.fullscreenActive?"退出全屏":"全屏查看",label:interactions.fullscreenActive?"退出":"全屏",icon:interactions.fullscreenActive?<IconArrowsMinimize/>:<IconArrowsMaximize/>,action:interactions.toggleFullscreen,disabled:!enabled});
  return <>
    <div ref={stageElement} {...zoom.bind} className={`dpreview progressive-preview media-viewer-stage media-fullscreen-stage${interactions.expanded?" is-expanded":""}${video?"":" image-zoom-stage"}${zoom.zoomed?" is-image-zoomed":""}${zoom.dragging?" is-image-dragging":""}`} aria-busy={loading} data-image-scale={video?undefined:zoom.scale} style={{"--media-brightness":interactions.brightness,...zoom.style} as CSSProperties}>
      {!visible && preview && !previewFailed && <img className="media-cover media-visual" src={preview} alt={file.name} width={file.thumb_w || undefined} height={file.thumb_h || undefined} decoding="async" onLoad={zoom.constrain} onError={() => setPreviewFailed(true)} />}
      {!visible && (!preview || previewFailed) && !autoLoad && <div className="preview-message"><p>{blocked || (file.archive_state === "failed" ? "归档失败，请重试归档" : file.archive_state !== "done" ? "归档中…" : previewFailed ? "预览图暂时无法加载" : "暂无预览图，可按需查看原件")}</p>{previewFailed && enabled && <button className="btn" onClick={() => { setPreviewFailed(false); setPreviewAttempt(value => value + 1); }}>重试预览图</button>}</div>}
      {enabled && source && (video ? <video ref={videoElement} className={`media-visual${visible ? "" : " original-pending"}`} key={source} src={source} muted={muted} playsInline preload="auto" poster={preview || undefined} onLoadedData={ready} onDurationChange={event=>setDuration(Number.isFinite(event.currentTarget.duration)?event.currentTarget.duration:0)} onTimeUpdate={event=>setPosition(event.currentTarget.currentTime)} onRateChange={event=>setRate(event.currentTarget.playbackRate)} onVolumeChange={event=>setMuted(event.currentTarget.muted||event.currentTarget.volume===0)} onError={() => void failed()} onEnded={() => { if (partIndex + 1 < parts.length) choosePart(partIndex + 1, true); }} /> : <img className={`media-visual${visible ? "" : " original-pending"}`} src={source} alt={file.name} onLoad={ready} onError={() => void failed()} />)}
      {interactions.overlay}
      {video&&<div className="media-progress-edge" style={{"--media-progress":`${duration>0?Math.min(100,position/duration*100):0}%`} as CSSProperties}>
        <input type="range" aria-label="视频进度" aria-valuetext={`${mediaTime(position)} / ${mediaTime(duration||file.duration)}`} min="0" max={duration||1} step="0.1" value={Math.min(position,duration||1)} disabled={!videoReady||!duration||!interactionActive} onChange={event=>interactions.seek(0,Number(event.target.value))}/>
      </div>}
      {loading && <MediaLoading label={phase} percent={stage === "preparing" && preparationPhase !== "queued" || stage === "loading" && !video ? percent : undefined}/>}
    </div>
    <div ref={dock} className="detail-action-dock">
      {status && <div className="detail-status-strip" aria-label="媒体状态">{status}</div>}
      <div className="media-control-info" aria-label={video?"视频播放状态":"图片缩放比例"}>
        <output>{video?`${mediaTime(position)} / ${mediaTime(duration||file.duration)}`:`${Math.round(zoom.scale*100)}%`}</output>
        {visible&&compatible&&<span>兼容查看版本</span>}
      </div>
      <div className="media-control-buttons" role="group" aria-label={video?"视频控制":"图片控制"} data-media-kind={video?"video":"image"}>
        {controls.map((control,index)=><button type="button" key={index} className={`media-control-button${index===0?" is-primary":""}`} aria-label={control.name} aria-keyshortcuts={index===5?"F":video&&index===0?"Space":undefined} aria-pressed={video&&index===4?muted:undefined} disabled={control.disabled||!interactionActive} onClick={control.action}>{control.icon}<span>{control.label}</span></button>)}
      </div>
      <div className="preview-actions" role="group" aria-label="媒体操作">
        {actions}
        {enabled && <>
          {parts.length > 1 ? <><label className="sr-only" htmlFor={`media-part-${file.id}`}>视频片段</label><select id={`media-part-${file.id}`} className="select" value={partIndex} onChange={event => choosePart(Number(event.target.value))}>{parts.map(part => <option key={part.index} value={part.index}>第 {part.index + 1} / {part.count} 段</option>)}</select><a className="btn" href={api.url(`/files/${encodeURIComponent(file.id)}/download?part=${partIndex}`)} download>下载当前无损片段</a></> : <a className="btn" href={sourceAdapter?.download || api.url(`/files/${encodeURIComponent(file.id)}/download`)} download>下载原件</a>}
        </>}
      </div>
      {error && <p className="detail-error media-load-error" role="alert">{error}</p>}
      {notice}
    </div>
  </>;
}
