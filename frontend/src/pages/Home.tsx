import { useContext, useEffect, useRef, useState } from "react";
import type { CSSProperties } from "react";
import { Link } from "react-router-dom";
import { displaySiteName, fmtDate, fmtDur, fmtSize } from "../api";
import type { HomeItem } from "../api";
import { SiteCtx, useToast } from "../App";
import { useDialog } from "../admin/interactions";
import useHomeFeed from "./useHomeFeed";
import SiteFooter from "../components/SiteFooter";
import useMediaInteractions from "../media/useMediaInteractions";
import MediaLoading from "../media/MediaLoading";
import "./home.css";

const icon = (d:string,size=20) => <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={d}/></svg>;
const icons={close:"M6 6l12 12M18 6 6 18",info:"M12 16v-4m0-4h.01M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0",copy:"M9 9h12v12H9zM15 9V3H3v12h6",download:"M12 3v12m-5-5 5 5 5-5M4 16v5h16v-5",admin:"M14 4h6v16h-6M3 12h12m-4-4 4 4-4 4"};

export default function Home(){
 const site=useContext(SiteCtx),feed=useHomeFeed(),sentinel=useRef<HTMLDivElement>(null),toast=useToast();
 const [lightID,setLightID]=useState<string|null>(null),index=lightID?feed.items.findIndex(i=>i.id===lightID):-1;
 const [pendingNext,setPendingNext]=useState<string|null>(null),nextRequest=useRef<{id:string;requested:boolean}|null>(null);
 const open=(id:string|null)=>{nextRequest.current=null;setPendingNext(null);setLightID(id)};
 const next=()=>{
  if(index<0||nextRequest.current)return;
  if(index<feed.items.length-1){open(feed.items[index+1].id);return}
  if(feed.items.length<feed.total){nextRequest.current={id:feed.items[index].id,requested:false};setPendingNext(feed.items[index].id)}
 };
 useEffect(()=>{
  const request=nextRequest.current;if(!pendingNext||!request)return;
  if(lightID!==request.id){nextRequest.current=null;setPendingNext(null);return}
  const at=feed.items.findIndex(item=>item.id===request.id),following=at>=0?feed.items[at+1]:undefined;
  if(following){nextRequest.current=null;setPendingNext(null);setLightID(following.id);return}
  if(feed.loading)return;
  if(request.requested||feed.items.length>=feed.total){nextRequest.current=null;setPendingNext(null);if(feed.error)toast("下一张未能加载，请重试","err");return}
  request.requested=true;void feed.loadMore();
 },[pendingNext,lightID,feed.items,feed.total,feed.loading,feed.error,feed.loadMore,toast]);
 useEffect(()=>{if(!sentinel.current)return;const observer=new IntersectionObserver(entries=>{if(entries[0].isIntersecting&&!feed.loading&&!feed.error&&feed.items.length<feed.total)void feed.loadMore()},{rootMargin:"400px"});observer.observe(sentinel.current);return()=>observer.disconnect()},[feed.items.length,feed.total,feed.loading,feed.error,feed.loadMore]);
 const photo=(item:HomeItem,index:number)=><figure className="exhibit-frame" key={item.id}><PublicCover item={item} eager={index<4} onOpen={()=>open(item.id)}/><figcaption><span title={item.name}>{item.name}</span><time>{fmtDate(item.media_at||item.created_at)}</time></figcaption></figure>;
 return <div className="exhibition"><header className="public-header-shell"><div className="public-header page-width"><Link className="public-brand" to="/">{site?.logo_url&&<img src={site.logo_url} alt="" onError={e=>{e.currentTarget.hidden=true}}/>}{displaySiteName(site?.site_name)}</Link></div></header>
 <main className="public-main page-width" aria-label="公开作品展示"><h1 className="sr-only">{displaySiteName(site?.site_name)} 公开作品</h1>{feed.error&&<div className="public-feedback" role="alert"><p>{feed.error}</p><button className="btn" disabled={feed.loading} onClick={()=>void feed.refresh()}>重新加载作品</button></div>}
 {!!feed.items.length&&<><div className="exhibit-intro">{site?.site_desc&&<p><Link to="/admin" title="进入管理后台" aria-label={`${site.site_desc}，进入管理后台`}>{site.site_desc}</Link></p>}<span>{feed.total} 件公开作品</span></div><section className="exhibit-sequence" aria-label="公开作品">{feed.items.map(photo)}</section></>}
 {!feed.items.length&&!feed.error&&<div className="public-empty"><p>{feed.loading?"正在读取作品…":"这里还没有公开作品"}</p>{!feed.loading&&<span>管理员公开文件或相册后，会在这里显示缩略图。</span>}</div>}<div ref={sentinel} className="exhibit-load" aria-live="polite">{feed.items.length<feed.total&&<button className="btn btn-ghost" disabled={feed.loading} onClick={()=>void feed.loadMore()}>{feed.loading?"正在读取更多作品…":"继续浏览"}</button>}</div></main>
 <SiteFooter site={site} />
 {index>=0&&<PublicViewer item={feed.items[index]} position={`${index+1} / ${feed.total}`} onClose={()=>open(null)} onPrev={index?()=>open(feed.items[index-1].id):undefined} onNext={index<feed.total-1?next:undefined} loadingNext={!!pendingNext}/>}</div>;
}
function PublicCover({item,eager,onOpen}:{item:HomeItem;eager?:boolean;onOpen:()=>void}){
 const [failed,setFailed]=useState(false),[revision,setRevision]=useState(0);
 const source=revision?(()=>{const u=new URL(item.thumb_url,location.href);u.searchParams.set("pimg_retry",String(revision));return u.href})():item.thumb_url;
 return <button className="exhibit-open" aria-label={failed?`重试封面 ${item.name}`:`预览缩略图 ${item.name}`} onClick={()=>{if(failed){setRevision(Date.now());setFailed(false)}else onOpen()}}><div className="exhibit-image">{!failed&&<img src={source} alt={item.name} loading={eager?"eager":"lazy"} decoding="async" onError={()=>setFailed(true)}/>} {failed&&<span className="public-image-error"><span>封面暂时无法加载</span><span>点击重试封面</span></span>}</div>{item.kind==="video"&&<span className="play-badge">{icon("M3 5h12v14H3zM15 9l6-3v12l-6-3",13)}{fmtDur(item.duration)}</span>}</button>;
}
function PublicViewer({item,position,onClose,onPrev,onNext,loadingNext}:{item:HomeItem;position:string;onClose:()=>void;onPrev?:()=>void;onNext?:()=>void;loadingNext:boolean}){
 const ref=useRef<HTMLDivElement>(null);const [info,setInfo]=useState(false);useDialog(ref,onClose);
 return <div ref={ref} className="public-viewer" role="dialog" aria-modal="true" aria-label={`查看作品 ${item.name}`}><header className="photo-viewer-header"><button className="viewer-icon" onClick={onClose} aria-label="关闭图片">{icon(icons.close)}</button><span className="viewer-name">{item.name}</span><span className="viewer-position">{position}</span></header>
 <div className="public-viewer-body"><div className="public-viewer-canvas"><PublicMedia key={`${item.id}:${item.thumb_url}`} item={item} active={!info} onPrev={onPrev} onNext={onNext}/>{loadingNext&&<div className="public-next-loading"><MediaLoading label="加载下一张"/></div>}</div>
 </div><footer className="public-viewer-dock">{info&&<aside className="public-viewer-info" aria-label="作品信息"><dl><div><dt>文件日期</dt><dd>{fmtDate(item.media_at||item.created_at)}</dd></div><div><dt>尺寸</dt><dd>{item.width} × {item.height}</dd></div><div><dt>文件</dt><dd>{item.ext.toUpperCase()} · {fmtSize(item.size)}</dd></div>{item.kind==="video"&&<div><dt>时长</dt><dd>{fmtDur(item.duration)}</dd></div>}</dl></aside>}<div className="public-viewer-actions"><button className="viewer-action" aria-label="图片信息" aria-pressed={info} onClick={()=>setInfo(!info)}>{icon(icons.info,18)}信息</button></div></footer></div>;
}

type PublicMediaProps={item:HomeItem;active:boolean;onPrev?:()=>void;onNext?:()=>void};
function PublicMedia({item,active,onPrev,onNext}:PublicMediaProps){
 const [retry,setRetry]=useState(0);
 return <PublicMediaAttempt key={retry} item={item} active={active} onPrev={onPrev} onNext={onNext} revision={retry} onRetry={()=>setRetry(Date.now())}/>;
}

function PublicMediaAttempt({item,active,onPrev,onNext,revision,onRetry}:PublicMediaProps&{revision:number;onRetry:()=>void}){
 const [ready,setReady]=useState(false),[failed,setFailed]=useState(false),alive=useRef(true);
 const videoElement=useRef<HTMLVideoElement>(null),stageElement=useRef<HTMLDivElement>(null);
 useEffect(()=>{alive.current=true;return()=>{alive.current=false}},[]);
 const source=revision?(()=>{const u=new URL(item.thumb_url,location.href);u.searchParams.set("pimg_retry",String(revision));return u.href})():item.thumb_url;
 const interactions=useMediaInteractions({mediaKey:`public:${item.id}:${revision}`,video:false,enabled:!failed,active,source,videoElement,stageElement,onPrev,onNext,onRequestPlay:()=>{if(videoElement.current)void videoElement.current.play().catch(()=>{})}});
 const loaded=async(image:HTMLImageElement)=>{
  try{await image.decode()}catch{if(!image.complete||!image.naturalWidth){if(alive.current)setFailed(true);return}}
  if(alive.current)setReady(true);
 };
 const mediaName="缩略图";
 return <div ref={stageElement} className={`public-viewer-media media-fullscreen-stage${interactions.expanded?" is-expanded":""}`} aria-busy={!ready&&!failed} style={{"--media-brightness":interactions.brightness} as CSSProperties}>
  <img className={`public-viewer-original media-visual${ready?" is-ready":""}`} src={source} alt={item.name} decoding="async" onLoad={event=>void loaded(event.currentTarget)} onError={()=>setFailed(true)}/>

  {interactions.overlay}
  {failed?<div className="public-media-status" role="alert"><span>{mediaName}暂时无法加载</span><button type="button" className="viewer-action" onClick={onRetry}>重试{mediaName}</button></div>:!ready&&<MediaLoading label={`加载${mediaName}`}/>}
 </div>;
}
