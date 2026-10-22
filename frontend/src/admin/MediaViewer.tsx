import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { CSSProperties, ReactNode } from "react";
import { fmtSize } from "../api";
import type { FileItem, OriginalView } from "../api";
import useMediaInteractions from "../media/useMediaInteractions";
import useImageZoom from "../media/useImageZoom";
import MediaLoading from "../media/MediaLoading";

import { useGallery } from "./GalleryScope";

type Stage = "idle" | "preparing" | "loading" | "decoding" | "ready" | "error";

export type MediaSourceAdapter = {
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
  const video = file.kind === "video", enabled = file.archive_state === "done" && !blocked;
  const [requested, setRequested] = useState(!video && !!file.local_ready), [attempt, setAttempt] = useState(0);
  const [stage, setStage] = useState<Stage>("idle"), [percent, setPercent] = useState(0);
  const [source, setSource] = useState(""), [visible, setVisible] = useState(false), [error, setError] = useState("");
  const [previewFailed, setPreviewFailed] = useState(false), [previewAttempt, setPreviewAttempt] = useState(0);
  const [partIndex, setPartIndex] = useState(0), [compatible, setCompatible] = useState("");
  const objectURL = useRef(""), dock = useRef<HTMLDivElement>(null), videoElement = useRef<HTMLVideoElement>(null),stageElement=useRef<HTMLDivElement>(null);
  const activeSignal = useRef<AbortSignal>(), heicTried = useRef(false), playNext = useRef(false), expectedTime = useRef(0);
  const requestedRef=useRef(requested);requestedRef.current=requested;
  const parts = file.media_parts || [], currentPart = parts[partIndex];
  const preview = !sourceAdapter && enabled && (file.has_thumb || file.kind === "image" || file.kind === "video") ? api.url(`/files/${encodeURIComponent(file.id)}/content?kind=thumb&v=${previewAttempt}`) : "";
  const loading = ["preparing", "loading", "decoding"].includes(stage);
  const requestPlay=()=>{
    if(!enabled||requestedRef.current&&stage!=="error")return;
    requestedRef.current=true;playNext.current=video;setRequested(true);setAttempt(value=>value+1);
  };
  const interactions=useMediaInteractions({mediaKey:`${file.id}:${partIndex}:${compatible}`,video,enabled,active:interactionActive,source,videoElement,stageElement,onRequestPlay:requestPlay,onPrev,onNext});
  const zoom=useImageZoom({mediaKey:file.id,enabled:!video&&enabled&&!!(source||preview&&!previewFailed),active:interactionActive,stage:stageElement});
  useLayoutEffect(()=>{zoom.constrain()},[source,visible,previewAttempt,zoom.constrain]);
  useEffect(() => () => { if (objectURL.current) URL.revokeObjectURL(objectURL.current); }, []);
  useEffect(()=>{const media=videoElement.current;return()=>media?.pause()},[source,enabled]);
  useEffect(()=>{
    if(enabled)return;
    videoElement.current?.pause();playNext.current=false;expectedTime.current=0;requestedRef.current=false;
    setRequested(false);setVisible(false);setSource("");setStage("idle");
  },[enabled]);
  useEffect(() => {
    setRequested(!video && !!file.local_ready); setPartIndex(0); setCompatible(""); setVisible(false); setSource(""); heicTried.current = false;
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
    setStage("preparing"); setPercent(0); setError(""); setVisible(false); setSource("");
    const poll = async (retry: boolean) => {
      try {
        const result = sourceAdapter ? await sourceAdapter.prepare(compatible, retry, controller.signal) : await api.post<OriginalView>(`/files/${encodeURIComponent(file.id)}/original`, { retry, ...(file.media_version === 2 ? { part: partIndex, compatible } : {}) });
        if (stopped) return;
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
    if (file.media_version === 2 && compatible !== (video ? "transcode" : "image")) {
      setCompatible(video ? compatible === "remux" ? "transcode" : "remux" : "image"); return;
    }
    setVisible(false); setSource(""); setStage("error"); setError(video ? "视频暂时无法播放，请重试或下载视频文件。" : "原图暂时无法显示，请重试或下载原件。");
  };
  const choosePart = (index: number, autoplay = false) => { expectedTime.current=0;playNext.current = autoplay;requestedRef.current=true;setPartIndex(index); setCompatible(""); setRequested(true); setAttempt(value => value + 1); };
  const label = video ? "视频" : "原图";
  const phase = stage === "preparing" ? `准备${label}` : stage === "decoding" ? "正在解码原图" : `加载${label}`;
  return <>
    <div ref={stageElement} {...zoom.bind} className={`dpreview progressive-preview media-viewer-stage media-fullscreen-stage${interactions.expanded?" is-expanded":""}${video?"":" image-zoom-stage"}${zoom.zoomed?" is-image-zoomed":""}${zoom.dragging?" is-image-dragging":""}`} aria-busy={loading} data-image-scale={video?undefined:zoom.scale} style={{"--media-brightness":interactions.brightness,...zoom.style} as CSSProperties}>
      {!visible && preview && !previewFailed && <img className="media-cover media-visual" src={preview} alt={file.name} width={file.thumb_w || undefined} height={file.thumb_h || undefined} decoding="async" onLoad={zoom.constrain} onError={() => setPreviewFailed(true)} />}
      {!visible && (!preview || previewFailed) && <div className="preview-message"><p>{blocked || (file.archive_state === "failed" ? "归档失败，请重试归档" : file.archive_state !== "done" ? "归档中…" : previewFailed ? "预览图暂时无法加载" : "暂无预览图，可按需查看原件")}</p>{previewFailed && enabled && <button className="btn" onClick={() => { setPreviewFailed(false); setPreviewAttempt(value => value + 1); }}>重试预览图</button>}</div>}
      {enabled && source && (video ? <video ref={videoElement} className={`media-visual${visible ? "" : " original-pending"}`} key={source} src={source} controls controlsList="nofullscreen" playsInline preload="auto" poster={preview || undefined} onLoadedData={ready} onError={() => void failed()} onEnded={() => { if (partIndex + 1 < parts.length) choosePart(partIndex + 1, true); }} /> : <img className={`media-visual${visible ? "" : " original-pending"}`} src={source} alt={file.name} onLoad={ready} onError={() => void failed()} />)}
      {interactions.overlay}
      {zoom.controls}
      {loading && <MediaLoading label={phase} percent={stage === "preparing" || stage === "loading" && !video ? percent : undefined}/>}
    </div>
    <div ref={dock} className="detail-action-dock">
      {status && <div className="detail-status-strip" aria-label="媒体状态">{status}</div>}
      <div className="preview-actions" role="group" aria-label="媒体操作">
        {actions}
        {enabled && <>
          {!visible && <button className="btn btn-crayon" disabled={loading} onClick={requestPlay}>{loading ? `正在加载${label}…` : stage === "error" ? `重试查看${label}` : video ? `播放视频 · ${fmtSize(currentPart?.size || file.size)}` : `查看原图 · ${fmtSize(file.size)}`}</button>}
          {visible && compatible && <span className="preview-original-label" role="status">兼容查看版本</span>}
          {parts.length > 1 ? <><label className="sr-only" htmlFor={`media-part-${file.id}`}>视频片段</label><select id={`media-part-${file.id}`} className="select" value={partIndex} onChange={event => choosePart(Number(event.target.value))}>{parts.map(part => <option key={part.index} value={part.index}>第 {part.index + 1} / {part.count} 段</option>)}</select><a className="btn" href={api.url(`/files/${encodeURIComponent(file.id)}/download?part=${partIndex}`)} download>下载当前无损片段</a></> : <a className="btn" href={sourceAdapter?.download || api.url(`/files/${encodeURIComponent(file.id)}/download`)} download>下载原件</a>}
        </>}
      </div>
      {error && <p className="detail-error media-load-error" role="alert">{error}</p>}
      {notice}
    </div>
  </>;
}
