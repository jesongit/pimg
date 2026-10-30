import FloatingFeedback from "../FloatingFeedback";
import {useCallback,useEffect,useRef,useState} from 'react';
import {createPortal} from 'react-dom';
import type {FormEvent} from 'react';
import {useGallery} from './GalleryScope';
import type {Album,TelegramStatus,TelegramTopic} from '../api';
import {api as siteApi} from '../api';
import {useToast} from '../App';
import {useDialog,useUnsavedChanges} from './interactions';
import TelegramScan,{TelegramAlbumState} from './TelegramScan';
import {useLibraryPopover} from './LibraryPopover';
import ConfirmButton from './ConfirmButton';
import FilterSelect from './FilterSelect';
export function CreateAlbum({ albums, onClose, onCreated, allowOwnerSelection = true }: { albums: Album[]; onClose: () => void; onCreated: (response: { id: number; task_id?: number; owner_id?: string }, existing: boolean) => Promise<void>; allowOwnerSelection?: boolean }) {
  const {ownApi:api,admin,actor,owner:galleryOwner}=useGallery();
  const [owner,setOwner]=useState(admin&&allowOwnerSelection?galleryOwner:actor.owner_id),[users,setUsers]=useState<{id:string;username:string;disabled:boolean;deleting:boolean}[]>([]),[usersError,setUsersError]=useState("");
  const loadUsers=useCallback(async()=>{if(!admin||!allowOwnerSelection)return;try{const response=await siteApi.get<{items:{id:string;username:string;disabled:boolean;deleting:boolean}[]}>("/api/admin/accounts");if(alive.current){setUsers(response.items.filter(user=>!user.disabled&&!user.deleting));setUsersError("")}}catch(cause:any){if(alive.current)setUsersError(cause.message||"用户列表读取失败，请重试。")}},[admin,allowOwnerSelection]);
  const [name, setName] = useState(""), [mode, setMode] = useState<"new" | "existing">("new");
  const [status, setStatus] = useState<TelegramStatus | null>(null), [statusLoading, setStatusLoading] = useState(true), [statusError, setStatusError] = useState("");
  const [topics, setTopics] = useState<TelegramTopic[]>([]), [topicID, setTopicID] = useState("");
  const [topicsLoading, setTopicsLoading] = useState(false), [topicsError, setTopicsError] = useState("");
  const [saving, setSaving] = useState(false), [error, setError] = useState("");
  const modal = useRef<HTMLDivElement>(null), nameInput = useRef<HTMLInputElement>(null), alive = useRef(true), submitting = useRef(false), topicRequest = useRef(0);
  const dirty = !!name.trim() || mode === "existing" && !!topicID;
  useUnsavedChanges(dirty);
  const requestClose = () => { if (!submitting.current && (!dirty || confirm("相册尚未创建，放弃已填写内容？"))) onClose(); };
  useDialog(modal, requestClose);
  const loadStatus = useCallback(async () => {
    if(!admin||owner!==actor.owner_id){setStatus(null);setStatusLoading(false);setStatusError("");return;}
    setStatusLoading(true); setStatusError("");
    try { const response = await api.get<TelegramStatus>("/telegram"); if (alive.current) setStatus(response); }
    catch (cause: any) { if (alive.current) setStatusError(cause.message || "无法读取 TG 状态，可重试或按名称创建相册。"); }
    finally { if (alive.current) setStatusLoading(false); }
  }, [api,admin,owner,actor.owner_id]);
  useEffect(() => { alive.current = true; nameInput.current?.focus(); void loadStatus(); return () => { alive.current = false; topicRequest.current++; }; }, [loadStatus]);
  useEffect(()=>{void loadUsers()},[loadUsers]);
  const canChoose = admin && owner===actor.owner_id && status?.state === "authorized" && status.chat_id > 0;
  const linked = new Set(albums.flatMap(album => album.telegram && album.telegram.chat_id === status?.chat_id ? [album.telegram.topic_id] : []));
  const available = topics.filter(topic => topic.id > 1 && !topic.closed && !linked.has(topic.id));
  const chosen = available.find(topic => String(topic.id) === topicID);
  const loadTopics = async () => {
    if (!canChoose || topicsLoading) return;
    const serial = ++topicRequest.current; setTopicsLoading(true); setTopicsError("");
    try { const response = await api.get<{ items: TelegramTopic[] }>(`/telegram/topics?chat_id=${status.chat_id}`); if (alive.current && topicRequest.current === serial) setTopics(response.items || []); }
    catch (cause: any) { if (alive.current && topicRequest.current === serial) setTopicsError(cause.message || "话题读取失败，请重试。"); }
    finally { if (alive.current && topicRequest.current === serial) setTopicsLoading(false); }
  };
  const submit = async (event: FormEvent) => {
    event.preventDefault(); if (submitting.current || mode === "existing" && !chosen || mode === "new" && !name.trim()) return;
    submitting.current = true; setSaving(true); setError("");
    try {
      const response = await api.post<{ id: number; task_id?: number; owner_id?: string }>("/albums", mode === "existing" ? { name: chosen!.name, topic_id: chosen!.id } : { name: name.trim(), ...(admin?{owner_id:owner}:{}) });
      if (alive.current) await onCreated(response, mode === "existing");
    } catch (cause: any) { if (alive.current) setError(cause.message || "相册未能创建，输入已保留，请重试。"); }
    finally { submitting.current = false; if (alive.current) setSaving(false); }
  };
  return <><div className="overlay" onClick={requestClose} /><div ref={modal} className="modal album-create-dialog" role="dialog" aria-modal="true" aria-labelledby="album-create-title">
    <div className="dhead"><h2 id="album-create-title">新建相册</h2><button type="button" className="btn btn-ghost" disabled={saving} onClick={requestClose} aria-label="关闭新建相册">关闭</button></div>
    <form onSubmit={event => void submit(event)} className="album-create-form">
      {admin&&allowOwnerSelection&&<><label htmlFor="album-create-owner">所属用户</label><select id="album-create-owner" className="select" value={owner} disabled={saving} onChange={event=>{setOwner(event.target.value);setMode("new");setTopicID("");setError("")}}><option value={actor.owner_id}>自己（{actor.username}）</option>{users.filter(user=>user.id!==actor.owner_id).map(user=><option key={user.id} value={user.id}>{user.username}</option>)}</select>{usersError&&<FloatingFeedback className="detail-error" role="alert">{usersError}<button type="button" className="btn btn-ghost" onClick={()=>void loadUsers()}>重新读取用户</button></FloatingFeedback>}</>}
      {statusLoading && <p className="detail-hint" role="status">正在读取 TG 关联方式…</p>}
      {statusError && <FloatingFeedback className="detail-hint" role="alert"><p>{statusError}</p><button type="button" className="btn btn-ghost" onClick={() => void loadStatus()}>重试 TG 状态</button></FloatingFeedback>}
      {canChoose && <fieldset className="album-create-modes" disabled={saving}><legend>话题方式</legend><label><input type="radio" name="album-topic-mode" checked={mode === "new"} onChange={() => { setMode("new"); setError(""); }} />新建同名 TG 话题</label><label><input type="radio" name="album-topic-mode" checked={mode === "existing"} onChange={() => { setMode("existing"); setError(""); void loadTopics(); }} />关联已有话题</label></fieldset>}
      {mode === "existing" && canChoose ? <>
        <label htmlFor="album-create-topic">已有话题</label><div className="album-create-topic"><select id="album-create-topic" className="select" value={topicID} disabled={saving || topicsLoading} onChange={event => { setTopicID(event.target.value); setError(""); }} required><option value="">{topicsLoading ? "正在读取话题…" : "请选择未关联的话题"}</option>{available.map(topic => <option key={topic.id} value={topic.id}>{topic.name}</option>)}</select><button type="button" className="btn btn-ghost" disabled={saving || topicsLoading} onClick={() => void loadTopics()}>刷新话题</button></div>
        {topicsError && <FloatingFeedback className="detail-error" role="alert">{topicsError}</FloatingFeedback>}{!topicsLoading && !topicsError && !available.length && <p className="detail-hint">没有可关联的话题。General、已关闭或已被相册关联的话题不在列表中，可改为新建同名话题。</p>}
        <p className="detail-hint">相册名称跟随已有话题，不会再创建新话题。创建后可在相册设置中选择首次扫描起点，以启用自动同步。</p>
      </> : <p className="detail-hint">{!admin ? "创建自己的相册，归档话题由系统自动分配。" : owner!==actor.owner_id ? "相册归所选用户所有，可在服务器导入时选择。归档话题由系统自动分配。" : canChoose ? "在固定归档群中创建同名话题，文件归入这个相册。" : "按名称创建相册。登录 TG 并固定归档群后，可在此选择已有话题。"}</p>}
      <label htmlFor="album-create-name">相册名称</label><input ref={nameInput} id="album-create-name" className="input" value={mode === "existing" ? chosen?.name || "" : name} readOnly={mode === "existing"} disabled={saving} onChange={event => setName(event.target.value)} maxLength={128} required />
      {error && <FloatingFeedback className="detail-error" role="alert">{error}</FloatingFeedback>}
      <div className="album-create-actions"><button className="btn btn-crayon" disabled={saving || statusLoading || mode === "existing" && topicsLoading || mode === "new" && !name.trim() || mode === "existing" && !chosen}>{saving ? "创建中…" : mode === "existing" ? "创建并关联话题" : "创建相册"}</button><button type="button" className="btn" disabled={saving} onClick={requestClose}>取消创建</button></div>
    </form>
  </div></>;
}

export function AlbumFilter({albums,value,onSelect}:{albums:Album[];value:string;onSelect:(id:string)=>void}) {
  const unclassified=albums.find(a=>a.is_unclassified),selected=albums.find(a=>String(a.id)===value);
  const others=albums.filter(a=>!a.is_home&&!a.is_unclassified).sort((a,b)=>a.name.localeCompare(b.name,'zh-CN',{numeric:true})||a.id-b.id);
  return <FilterSelect label="相册" prefix="相册" displayLabel={value?undefined:'全部'} value={value==='-1'&&unclassified?String(unclassified.id):value} active={!!value} width={240} fallback={selected?.name||'相册不可用'} onChange={onSelect} options={[{value:'',label:'全部相册'},{value:unclassified?String(unclassified.id):'-1',label:'未分类',count:unclassified?.count},...albums.filter(a=>a.is_home).map(a=>({value:String(a.id),label:a.name,count:a.count})),...others.map((a,index)=>({value:String(a.id),label:a.name,count:a.count,section:index===0}))]}/>;
}

export default function AlbumManager({albums,album,value,onChanged,onSelect}:{albums:Album[];album:Album|null;value:string;onChanged:()=>Promise<void>;onSelect:(id:string)=>void}) {
  const {api,admin,readOnly}=useGallery();
  const toast=useToast(), actions=useLibraryPopover(210);
  const [panel,setPanel]=useState<"rename"|"sync"|null>(null);
  const [name,setName]=useState(album?.name||""), [busy,setBusy]=useState(false), [error,setError]=useState("");
  const editable=!readOnly&&!!album&&!album.is_home&&!album.is_unclassified;
  const dirty=panel==="rename"&&editable&&name!==album?.name;
  useUnsavedChanges(dirty);
  useEffect(()=>{setName(album?.name||"");setError("");setPanel(null);actions.setOpen(false)},[album?.id,album?.name,value]);
  const closePanel=()=>{if(busy)return;if(dirty&&!confirm("相册名称尚未保存，放弃更改？"))return;setPanel(null);setName(album?.name||"");setError("")};
  const rename=async(e:FormEvent)=>{e.preventDefault();if(!album||!editable||!name.trim()||busy)return;setBusy(true);setError("");try{await api.patch('/albums/'+album.id,{name:name.trim()});setPanel(null);await onChanged();toast('相册名称已保存')}catch(e:any){setError(e.message)}finally{setBusy(false)}};
  const remove=async()=>{actions.close();if(!album||!editable||busy)return;if(!admin&&album.count>0){setError('请先移走或删除相册内的图片');return;}setBusy(true);setError('');try{await api.del('/albums/'+album.id);onSelect('');await onChanged();toast('相册删除请求已提交')}catch(e:any){setError(e.message)}finally{setBusy(false)}};
  const show=(mode:"rename"|"sync")=>{actions.close();setPanel(mode);setError("")};
  const unclassified=albums.find(a=>a.is_unclassified);
  const selectedValue=value==='-1'&&unclassified?String(unclassified.id):value;
  const scopeName=album?.name||(value==='-1'?'未分类':value?'相册不可用':'全部相册');
  const icon=(path:string)=><svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={path}/></svg>;
  const others=albums.filter(a=>!a.is_home&&!a.is_unclassified).sort((a,b)=>a.name.localeCompare(b.name,'zh-CN',{numeric:true})||a.id-b.id);
  const options=[{value:'',label:'全部相册'},{value:unclassified?String(unclassified.id):'-1',label:'未分类',count:unclassified?.count},...albums.filter(a=>a.is_home).map(a=>({value:String(a.id),label:'游客相册',count:a.count})),...others.map((a,index)=>({value:String(a.id),label:a.name,count:a.count,section:index===0}))];
  return <><div className={`album-context-switch${value ? " is-active" : ""}`}>
    <FilterSelect label="相册" value={selectedValue} options={options} fallback={scopeName} onChange={onSelect} active={!!value} disabled={busy} width={240}/>
    {album&&!readOnly&&(admin||editable)&&<button ref={actions.trigger} type="button" className="btn btn-ghost album-settings-trigger" aria-label={`相册设置：${album.name}`} title={`相册设置：${album.name}`} aria-haspopup="menu" aria-expanded={actions.open} disabled={busy} onClick={actions.toggle}>{icon('M12 8v.01M12 12v.01M12 16v.01')}</button>}
  </div>
  {actions.open&&album&&createPortal(<div ref={actions.popup} className="library-popover album-action-menu" role="menu" aria-label="当前相册设置" style={actions.position}>
    <p>{album.name}</p>{editable&&<button role="menuitem" onClick={()=>show('rename')}>重命名相册</button>}{admin&&<button role="menuitem" onClick={()=>show('sync')}>TG 话题与同步</button>}{editable&&<ConfirmButton role="menuitem" className="btn-danger" disabled={busy} confirmLabel="确认删除" busyLabel="删除中…" resetKey={`${album.id}:${album.count}:${admin}`} description={admin?`删除相册「${album.name}」后，图片、视频迁入未分类并校验，再删除对应的 TG 话题；话题中的其他内容会一并删除。操作在后台执行。`:`将删除空相册「${album.name}」，对应归档话题也会清理。`} onConfirm={remove}>删除相册</ConfirmButton>}
  </div>,document.body)}
  {error&&!panel&&<FloatingFeedback className="album-inline-error" role="alert">{error}</FloatingFeedback>}
  {panel&&album&&<AlbumActionDialog title={panel==='rename'?'重命名相册':album.name+' · TG 同步'} busy={busy} onClose={closePanel}>{panel==='rename'?<form className="album-rename-form" onSubmit={rename}><label htmlFor="album-rename">相册名称</label><input autoFocus id="album-rename" className="input" value={name} maxLength={128} onChange={e=>setName(e.target.value)} disabled={busy} required/>{error&&<FloatingFeedback role="alert" className="detail-error">{error}</FloatingFeedback>}<div className="album-create-actions"><button className="btn btn-crayon" disabled={busy||!name.trim()||name===album.name}>{busy?'保存中…':'保存名称'}</button><button type="button" className="btn" onClick={closePanel} disabled={busy}>取消</button></div></form>:<><TelegramAlbumState album={album}/><TelegramScan key={album.id} album={album} initialOpen onChanged={()=>void onChanged()}/></>}</AlbumActionDialog>}
  </>;
}

export function AlbumActionDialog({title,busy,onClose,children}:{title:string;busy:boolean;onClose:()=>void;children:React.ReactNode}){
 const ref=useRef<HTMLDivElement>(null);useDialog(ref,onClose);
 return createPortal(<><div className="overlay" onClick={onClose}/><div ref={ref} className="modal album-action-dialog" role="dialog" aria-modal="true" aria-labelledby="album-action-title"><div className="dhead"><h2 id="album-action-title">{title}</h2><button className="btn btn-ghost" onClick={onClose} disabled={busy} aria-label="关闭相册操作">关闭</button></div><div className="album-action-body">{children}</div></div></>,document.body);
}
