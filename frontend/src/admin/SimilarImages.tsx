import FloatingFeedback from "../FloatingFeedback";
import {useEffect,useId,useRef,useState} from "react";
import {createPortal} from "react-dom";
import type {Album,FileItem} from "../api";
import {fileMediaTime,fmtSize,fmtTime} from "../api";
import {useGallery} from "./GalleryScope";
import {filesUpdated,operationBusy,useDialog} from "./interactions";
import {tasksUpdated} from "./TaskStatus";
import {compareSimilarity,readSimilarityImage,SimilarIndex} from "./imageSimilarity";
import type {SimilarityComparison,SimilarityImage} from "./imageSimilarity";
import {orderImagePair} from "./imageRetention";
import ConfirmButton from "./ConfirmButton";
import MediaViewer from "./MediaViewer";
import "./similar-images.css";

type Pair={left:FileItem;right:FileItem;kind:"image"|"video";comparison?:SimilarityComparison;recommendation?:string;aspect?:number};
const maxPairs=200;
export default function SimilarImages({query,albums,onClose}:{query:string;albums:Album[];onClose:()=>void}){
 const heading="查重",unit="项";
 const {api}=useGallery(),ref=useRef<HTMLDivElement>(null),run=useRef<AbortController>(),alive=useRef(true),lock=useRef(false),title=useId();
 const [activeVideo,setActiveVideo]=useState(""),[showDifferences,setShowDifferences]=useState(true);
 const [candidateCount,setCandidateCount]=useState(0);
 const [pairs,setPairs]=useState<Pair[]>([]),[position,setPosition]=useState(0),[scanning,setScanning]=useState(false),[started,setStarted]=useState(false),[progress,setProgress]=useState({done:0,total:0,skipped:0}),[error,setError]=useState(""),[notice,setNotice]=useState(""),[busy,setBusy]=useState(false);
 const close=()=>{if(!lock.current){run.current?.abort();onClose()}};useDialog(ref,close);
 useEffect(()=>{alive.current=true;return()=>{alive.current=false;run.current?.abort()}},[]);
 const preview=(file:FileItem)=>api.url(`/files/${encodeURIComponent(file.id)}/content?kind=thumb`);
 const scan=async()=>{
  if(lock.current||scanning)return;const controller=new AbortController();run.current=controller;const signal=controller.signal;setScanning(true);setStarted(true);setPairs([]);setCandidateCount(0);setPosition(0);setError("");setNotice("");setProgress({done:0,total:0,skipped:0});
  let done=0,skipped=0,total=0,foundCount=0;const index=new SimilarIndex<FileItem>(),found:Pair[]=[],seen=new Set<string>(),exact=new Map<string,FileItem>(),images=new Map<string,SimilarityImage>(),sizes=new Map<number,FileItem>();
  const offer=(pair:Pair)=>{foundCount++;if(found.length<maxPairs){found.push(pair);setPairs([...found])}setCandidateCount(foundCount)};
  // Freeze the gallery's query, not its current page window or time-axis seek.
  const filter=new URLSearchParams(query);for(const key of ["page","start_page","pages","seek","before","after","group","file"])filter.delete(key);filter.set("per_page","200");
  const load=async(file:FileItem)=>{let image=images.get(file.id);if(image)images.delete(file.id);else image=await readSimilarityImage(preview(file),signal);images.set(file.id,image);while(images.size>12)images.delete(images.keys().next().value!);return image};
  try {
   for(let page=1;;page++){
    filter.set("page",String(page));const response=await api.get<{items:FileItem[];total:number}>(`/files?${filter}`,signal);total=response.total;if(signal.aborted)return;
    if(!response.items.length)break;
    for(const file of response.items){
     if(signal.aborted)return;if(seen.has(file.id))continue;seen.add(file.id);
     if(!["image","video"].includes(file.kind)||file.archive_state!=="done"||file.purge_state||operationBusy(file)||file.conversion){skipped++}else if(file.kind==="video"){
      if(!Number.isSafeInteger(file.size)||file.size<=0){skipped++}else{
       const same=sizes.get(file.size);
       // One representative per size avoids quadratic pair explosion while
       // every video in the full result set is still examined.
       if(same)offer({left:same,right:file,kind:"video"});else sizes.set(file.size,file);
      }
     }else{
      try {
       const validHash=/^[0-9a-f]{64}$/i.test(file.sha256);let match=validHash?exact.get(file.sha256.toLowerCase()):undefined,comparison:SimilarityComparison|undefined;
       const image=await load(file);if(signal.aborted)return;
       if(!match)for(const candidate of index.candidates(image.fingerprint)){
        try{const other=await load(candidate);if(signal.aborted)return;const result=compareSimilarity(other,image);if(result.similar){match=candidate;comparison=result;break}}
        catch{if(signal.aborted)return}
       }
       if(match)offer({...orderImagePair(match,file,comparison),kind:"image",comparison,aspect:image.fingerprint.aspect});
       index.add(file,image.fingerprint);if(validHash)exact.set(file.sha256.toLowerCase(),file);
      }catch(cause:any){if(signal.aborted)return;skipped++}
     }
     done++;
     // Video matching only inspects metadata; yield in batches rather than add
     // a browser timer delay to every file. Image decoding still yields per image.
     if(file.kind!=="video"||done%50===0){setProgress({done,total,skipped});await new Promise<void>(resolve=>setTimeout(resolve,0))}
    }
    if(page*200>=total)break;
   }
   if(alive.current&&!signal.aborted)setNotice(`扫描完成，已检查当前筛选的全部 ${done} 项。${foundCount>maxPairs?`共发现 ${foundCount} 组候选，当前展示前 ${maxPairs} 组；处理后可重新扫描。`:""}图片相似和视频大小相同均需人工核对后再删除。`);
  }catch(cause:any){if(alive.current&&!signal.aborted)setError(cause.message||"扫描失败，请重试。")}
  finally{if(alive.current){setScanning(false);setProgress({done,total,skipped})}}
 };
 const purge=async(keep:FileItem,remove:FileItem)=>{
  if(lock.current||scanning)return false;lock.current=true;setBusy(true);setError("");
  try{
   await api.post("/files/similar-purge",{keep_album_id:keep.album_id??0,remove_album_id:remove.album_id??0,keep_id:keep.id,remove_id:remove.id,keep_sha256:keep.sha256,remove_sha256:remove.sha256,comparison:keep.kind==="video"?"video_size":"image",confirm:"PURGE"});
   if(alive.current){
    const retained=(pair:Pair)=>pair.left.id!==remove.id&&pair.right.id!==remove.id;
    const remaining=pairs.filter(retained),next=pairs.slice(0,position).filter(retained).length;
    setPairs(remaining);setPosition(Math.min(next,Math.max(0,remaining.length-1)));setNotice(`已保留「${keep.name}」，「${remove.name}」的删除任务已提交。`)
   }tasksUpdated();filesUpdated();return true;
  }catch(cause:any){if(alive.current)setError(cause.message||"删除提交失败，请重新扫描后核对。");return false}
  finally{lock.current=false;if(alive.current)setBusy(false)}
 };
 const pair=pairs[Math.min(position,Math.max(0,pairs.length-1))];
 const video=pair?.kind==="video",item=video?"这个":"这张";
 const selectedVideo=pair&&[pair.left.id,pair.right.id].includes(activeVideo)?activeVideo:pair?.left.id;
 return createPortal(<>
  <div className="overlay" onClick={close}/>
  <section ref={ref} className="modal similar-dialog" role="dialog" aria-modal="true" aria-labelledby={title} aria-busy={busy}>
   <header className="dhead"><h2 id={title}>查重 · 当前筛选结果</h2><button className="btn btn-ghost" disabled={busy} onClick={close}>关闭</button></header>
   <div className="similar-body">
    <p className="detail-hint">扫描当前筛选的全部结果，包含尚未加载的分页，可跨相册。自动比较图片画面和视频原文件大小；视频大小相同不代表内容相同。建议保留的图片在右侧，删除仍需人工确认；关闭窗口会停止扫描。</p>
    <div className="similar-toolbar">
     <button className="btn btn-crayon" disabled={busy||scanning} onClick={()=>void scan()}>{started?"重新扫描":"开始扫描"}</button>
     {scanning&&<button className="btn" onClick={()=>{run.current?.abort();setNotice("扫描已停止，当前筛选结果尚未检查完整。")}}>停止扫描</button>}
     <span role="status" aria-live="polite">{started?`${scanning?"扫描中":"已扫描"} ${progress.done} / ${progress.total} ${unit} · ${candidateCount} 组候选${progress.skipped?` · 跳过 ${progress.skipped} 项（未就绪、大小未知或预览不可用）`:""}`:"点击开始后扫描当前筛选的全部结果"}</span>
    </div>
    {error&&<FloatingFeedback className="detail-error" role="alert">{error}</FloatingFeedback>}{notice&&<FloatingFeedback className="detail-hint" role="status">{notice}</FloatingFeedback>}
    {pair?<>
     <nav className="similar-navigation" aria-label={`${heading}对比`}>
      <button className="btn" disabled={busy||position===0} onClick={()=>setPosition(value=>value-1)}>上一组</button>
      <span>第 {Math.min(position+1,pairs.length)} / {pairs.length} 组 · {video?"文件大小相同，需人工确认":pair.left.sha256.toLowerCase()===pair.right.sha256.toLowerCase()?"原件内容相同":pair.comparison?.kind==="edited"?"存在局部修改，需人工确认":"画面高度相似，需人工确认"}</span>
      <button className="btn" disabled={busy||position>=pairs.length-1} onClick={()=>setPosition(value=>value+1)}>下一组</button>
     </nav>
     {!video&&pair.comparison?.kind==="edited"&&<label className="similar-difference-toggle"><input type="checkbox" checked={showDifferences} onChange={event=>setShowDifferences(event.target.checked)}/>标出差异区域</label>}
     {!video&&pair.recommendation&&<p className="similar-advice">右侧建议保留：{pair.recommendation}。</p>}
     <div className="similar-comparison">{[pair.left,pair.right].map((file,i)=>{
      const other=i?pair.left:pair.right,recommended=!video&&i===1&&!!pair.recommendation;
      return <article key={file.id} className={`similar-image${recommended?" is-recommended":""}`}>
       {video?<div className="similar-video" onPointerDownCapture={()=>setActiveVideo(file.id)} onFocusCapture={()=>setActiveVideo(file.id)}>
        <MediaViewer file={file} interactionActive={selectedVideo===file.id&&!busy&&!scanning}/>
       </div>:<div className="similar-image-stage">
        <div className="similar-image-frame" style={{aspectRatio:pair.aspect||1,maxWidth:`calc(var(--similar-image-height) * ${pair.aspect||1})`}}>
         <img src={preview(file)} alt={file.name}/>
         {showDifferences&&pair.comparison?.regions.map((region,index)=><span key={index} className="similar-difference-region" aria-hidden="true" style={{left:`${region.x*100}%`,top:`${region.y*100}%`,width:`${region.width*100}%`,height:`${region.height*100}%`}}/>)}
        </div>
        {recommended&&<strong className="similar-recommendation">建议保留</strong>}
       </div>}
       <h3 title={file.name}>{file.name}</h3>
       <dl><dt>原文件名</dt><dd>{file.orig_name}</dd><dt>相册</dt><dd>{albums.find(album=>album.id===file.album_id)?.name||(file.album_id?"相册不可用":"未分类")}</dd><dt>尺寸</dt><dd>{file.width&&file.height?`${file.width} × ${file.height}`:"未知"}</dd>
        <dt>大小</dt><dd>{fmtSize(file.size)}{video&&`（${file.size.toLocaleString()} 字节）`}</dd>
        {video&&<><dt>时长</dt><dd>{file.duration>0?`${file.duration.toFixed(2)} 秒`:"未知"}</dd></>}
        <dt>媒体日期</dt><dd>{fmtTime(fileMediaTime(file))}</dd><dt>导入时间</dt><dd>{fmtTime(file.created_at)}</dd><dt>格式</dt><dd>{file.ext.toUpperCase()}</dd>
       </dl>
       {!video&&<a className="btn" href={api.url(`/files/${encodeURIComponent(file.id)}/download`)} download>下载原件核对</a>}
       <ConfirmButton className="btn btn-danger" disabled={busy||scanning} resetKey={`${file.id}:${other.id}`} confirmLabel={`确认保留${item}并删除另${video?"一个":"一张"}`} description={`保留「${file.name}」，永久删除「${other.name}」及其存储副本和索引，无法恢复。${video?"文件大小相同不代表内容相同，请先播放核对。":"相似画面可能存在内容或画质差异，请先仔细核对。"}`} onConfirm={()=>purge(file,other)}>保留{item}，删除另{video?"一个":"一张"}</ConfirmButton>
      </article>
     })}</div>
    </>:started&&!scanning&&!error?<div className="empty"><p>{progress.done?"当前没有待核对的查重结果":"没有可对比的图片或视频"}</p></div>:null}
   </div>
  </section>
 </>,document.body);
}
