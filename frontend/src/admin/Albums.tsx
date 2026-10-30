import FloatingFeedback from "../FloatingFeedback";
import {useCallback,useEffect,useRef,useState} from "react";
import {Link} from "react-router-dom";
import {api,type Album} from "../api";
import {useToast} from "../App";
import {useGallery} from "./GalleryScope";
import {CreateAlbum,AlbumActionDialog} from "./AlbumManager";
import TelegramScan,{TelegramAlbumState} from "./TelegramScan";
import ConfirmButton from "./ConfirmButton";
import "./users.css";
import "./albums.css";

type User={id:string;username:string;disabled:boolean;deleting:boolean};
const endpoint="/api/admin/albums?owner_id=all";
export default function Albums(){
 const {actor,admin,ownApi}=useGallery(),toast=useToast(),alive=useRef(true),sequence=useRef(0),submitting=useRef(false);
 const initial=admin?api.cached<{items:Album[]}>(endpoint):ownApi.cached<{items:Album[]}>("/albums");
 const [items,setItems]=useState(initial?.items||[]),[users,setUsers]=useState<User[]>([]),[loading,setLoading]=useState(!initial),[error,setError]=useState("");
 const [search,setSearch]=useState(""),[owner,setOwner]=useState(""),[display,setDisplay]=useState(""),[creating,setCreating]=useState(false),[busy,setBusy]=useState(0),[editing,setEditing]=useState(0),[name,setName]=useState("");
 const [syncing,setSyncing]=useState<Album|null>(null);
 const load=useCallback(async()=>{const seq=++sequence.current;try{const [albums,accounts]=await Promise.all([admin?api.get<{items:Album[]}>(endpoint):ownApi.get<{items:Album[]}>("/albums"),admin?api.get<{items:User[]}>("/api/admin/accounts"):Promise.resolve({items:[]})]);if(alive.current&&seq===sequence.current){setItems(albums.items);setUsers(accounts.items);setError("")}}catch(e:any){if(alive.current&&seq===sequence.current&&e.name!=="AbortError")setError(e.message)}finally{if(alive.current&&seq===sequence.current)setLoading(false)}},[admin,ownApi]);
 const patch=(id:number,body:unknown)=>admin?api.patch(`/api/admin/albums/${id}`,body):ownApi.patch(`/albums/${id}`,body);
 const remove=(id:number)=>admin?api.del(`/api/admin/albums/${id}`):ownApi.del(`/albums/${id}`);
 useEffect(()=>{alive.current=true;void load();const changed=()=>void load(),visible=()=>{if(!document.hidden)void load()};window.addEventListener("pimg:files-updated",changed);window.addEventListener("focus",visible);const timer=setInterval(visible,15000);return()=>{alive.current=false;sequence.current++;clearInterval(timer);window.removeEventListener("pimg:files-updated",changed);window.removeEventListener("focus",visible)}},[load]);
 const ownerName=(id?:string)=>users.find(u=>u.id===id)?.username||(id===actor.owner_id?actor.username:id||"未知用户");
 const change=async(album:Album,work:()=>Promise<unknown>,message:string)=>{if(submitting.current)return;submitting.current=true;setBusy(album.id);try{await work();if(alive.current){setEditing(0);toast(message);window.dispatchEvent(new Event("pimg:files-updated"));await load()}}catch(e:any){if(alive.current)toast(e.message,"err")}finally{submitting.current=false;if(alive.current)setBusy(0)}};
 const shown=items.filter(a=>(!owner||a.owner_id===owner)&&(!display||String(!!a.displayed)===display)&&`${a.name} ${ownerName(a.owner_id)}`.toLocaleLowerCase().includes(search.toLocaleLowerCase()));
 return <div className="users-page albums-page">
  <header className="users-heading"><div><h1>相册管理</h1><p>{admin?"管理所有用户的相册，选择公开到首页的缩略图。":"创建、重命名和整理自己的相册；删除非空相册前请先移走文件。"}</p></div><button className="btn btn-crayon" disabled={!!busy} onClick={()=>setCreating(true)}>新建相册</button></header>
  <div className="users-toolbar"><label className="users-search"><span className="sr-only">{admin?"搜索相册或用户":"搜索相册"}</span><input className="input" type="search" placeholder={admin?"搜索相册或用户":"搜索相册"} value={search} onChange={e=>setSearch(e.target.value)}/></label>{admin&&<><label className="users-filter">用户<select className="input" value={owner} onChange={e=>setOwner(e.target.value)}><option value="">全部用户</option>{users.map(u=><option key={u.id} value={u.id}>{u.username}</option>)}</select></label><label className="users-filter">公开<select className="input" value={display} onChange={e=>setDisplay(e.target.value)}><option value="">全部</option><option value="true">已公开</option><option value="false">未公开</option></select></label></>}<span className="users-count" aria-live="polite">{loading?"读取中…":`${shown.length} 个相册`}</span><button className="btn" onClick={()=>void load()}>刷新</button></div>
  {error&&<FloatingFeedback className="users-error" role="alert"><span>{error}</span><button className="btn" onClick={()=>void load()}>重试读取</button></FloatingFeedback>}
  <div className="users-registry albums-registry"><table className="albums-table" aria-label="相册列表"><thead><tr><th scope="col">相册</th>{admin&&<th scope="col">所属用户</th>}<th scope="col">文件</th>{admin&&<th scope="col">公开状态</th>}<th scope="col">操作</th></tr></thead><tbody>{shown.map(a=><tr key={a.id} data-album-id={a.id}>
   <th scope="row">{editing===a.id?<form className="album-inline-rename" onSubmit={e=>{e.preventDefault();void change(a,()=>patch(a.id,{name:name.trim()}),"相册名称已保存")}}><input autoFocus className="input" aria-label="相册新名称" value={name} onChange={e=>setName(e.target.value)} maxLength={128} disabled={!!busy} required/><button className="btn btn-sm" disabled={!!busy||!name.trim()||name.trim()===a.name}>保存</button><button type="button" className="btn btn-sm" disabled={!!busy} onClick={()=>setEditing(0)}>取消</button></form>:<><strong>{a.name}</strong>{a.is_unclassified&&<span className="album-fixed-label">固定相册</span>}</>}</th>
   {admin&&<td data-label="所属用户">{ownerName(a.owner_id)}</td>}<td data-label="文件" className="album-count">{a.count} 项</td>{admin&&<td data-label="公开状态"><span className={`album-display-state${a.displayed?" is-enabled":""}`}>{a.displayed?"已公开":"未公开"}</span></td>}
   <td><div className="users-actions"><Link className="btn btn-sm" to={admin?`/admin?owner_id=${encodeURIComponent(a.owner_id||actor.owner_id)}&album_id=${a.id}`:`/library?album_id=${a.id}`}>打开图库</Link>{admin&&<><ConfirmButton className="btn btn-sm" disabled={!!busy} resetKey={`${a.id}:${!!a.displayed}`} confirmLabel={a.displayed?"确认取消公开":"确认公开"} description={a.displayed?`取消「${a.name}」的公开后，随相册公开的缩略图将隐藏；单独公开的文件仍然公开。`:`公开「${a.name}」的现有及以后加入文件的缩略图，包含未发布和加密文件；单独取消公开的文件保持隐藏。此功能不开放原件或原视频。`} onConfirm={()=>change(a,()=>patch(a.id,{displayed:!a.displayed}),a.displayed?"相册已取消公开，单独公开的文件不受影响":"相册已公开缩略图")}>{a.displayed?"取消公开":"公开"}</ConfirmButton><button className="btn btn-sm" disabled={!!busy} onClick={()=>setSyncing(a)}>TG 话题与同步</button></>}{!a.is_unclassified&&<><button className="btn btn-sm" disabled={!!busy||!!editing} onClick={()=>{setEditing(a.id);setName(a.name)}}>重命名</button><ConfirmButton className="btn btn-sm btn-danger" disabled={!!busy||!!editing||!admin&&a.count>0} title={!admin&&a.count>0?"请先移走相册内的文件":undefined} resetKey={`${a.id}:${a.count}`} confirmLabel="确认删除相册" description={admin?`删除「${a.name}」后，文件保留并迁入所属用户的未分类。关联的 TG 话题将在文件迁移校验完成后清理，话题内其他内容也会删除。`:`删除自己的空相册「${a.name}」，此操作不能撤销。`} onConfirm={()=>change(a,()=>remove(a.id),"相册删除请求已提交")}>删除相册</ConfirmButton></>}</div></td>
  </tr>)}</tbody></table>{loading&&!items.length&&<p className="users-loading" role="status">正在读取相册…</p>}{!loading&&!shown.length&&<div className="empty"><p>没有匹配的相册</p><span>{admin?"调整筛选，或新建相册后再公开缩略图。":"调整搜索条件，或新建相册。"}</span></div>}</div>
  {admin&&<p className="users-note">公开仅开放缩略图。文件的永久发布、加密和归属分别管理；停用或删除中的用户不对外公开。</p>}
  {admin&&syncing&&<AlbumActionDialog title={`${syncing.name} · TG 同步`} busy={false} onClose={()=>setSyncing(null)}><TelegramAlbumState album={syncing}/><TelegramScan album={syncing} initialOpen onChanged={()=>void load()}/></AlbumActionDialog>}
  {creating&&<CreateAlbum albums={items} allowOwnerSelection={admin} onClose={()=>setCreating(false)} onCreated={async()=>{setCreating(false);toast("相册已创建");await load()}}/>}
 </div>;
}
