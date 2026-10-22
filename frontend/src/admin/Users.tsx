import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { api, fmtSize } from "../api";
import { useToast } from "../App";
import { useDialog } from "./interactions";
import ConfirmButton from "./ConfirmButton";
import "./users.css";

type Space = { chat_id:number; chat_title:string; state:string; desired_title:string; last_error?:string; title_error?:string; title_retry_after:number };
type User = { id:string; username:string; role:"admin"|"user"; disabled:boolean; deleting:boolean; delete_error?:string; files:number; albums:number; bytes:number; telegram?:Space };
type Editor = { mode:"create"|"edit"|"password"|"group"; user?:User };
const spaceStates:Record<string,string> = {pending:"等待分配",creating:"正在建群",uncertain:"正在核对建群结果",failed:"分配待重试",blocked:"分配受限",ready:"已就绪"};

export default function Users() {
  const toast=useToast(), mounted=useRef(true),sequence=useRef(0);
  const initial=api.cached<{items:User[]}>("/api/admin/accounts");
  const [items,setItems]=useState<User[]>(initial?.items||[]),[loading,setLoading]=useState(!initial),[error,setError]=useState("");
  const [query,setQuery]=useState(""),[filter,setFilter]=useState("all"),[editor,setEditor]=useState<Editor|null>(null),[busy,setBusy]=useState("");
  const load=useCallback(async()=>{
    const request=++sequence.current;
    try {const result=await api.get<{items:User[]}>("/api/admin/accounts");if(mounted.current&&request===sequence.current){setItems(result.items);setError("")}return result.items}
    catch(e:any){if(mounted.current&&request===sequence.current&&e.name!=="AbortError")setError(e.message)}
    finally{if(mounted.current&&request===sequence.current)setLoading(false)}
  },[]);
  useEffect(()=>{mounted.current=true;void load().then(rows=>{if(rows?.some(u=>u.telegram?.state==="ready"&&!u.telegram.chat_title))void api.post<{items:User[]}>("/api/admin/accounts/telegram/refresh",{}).then(r=>{if(mounted.current)setItems(r.items)}).catch(()=>{})});return()=>{mounted.current=false}},[load]);
  const pending=items.some(u=>u.deleting || u.telegram?.desired_title || u.telegram && !["ready","blocked"].includes(u.telegram.state) && !u.disabled);
  useEffect(()=>{if(!pending)return;const id=window.setInterval(()=>{if(!document.hidden)void load()},3000);return()=>clearInterval(id)},[pending,load]);
  useEffect(()=>{const updated=()=>void load(),visible=()=>{if(!document.hidden)void load()};window.addEventListener("pimg:files-updated",updated);document.addEventListener("visibilitychange",visible);return()=>{window.removeEventListener("pimg:files-updated",updated);document.removeEventListener("visibilitychange",visible)}},[load]);
  const action=async(key:string,work:()=>Promise<unknown>,message:string)=>{
    setBusy(key);try{await work();toast(message);await load()}catch(e:any){toast(e.message,"err")}finally{if(mounted.current)setBusy("")}
  };
  const shown=items.filter(u=>(filter==="all"||(filter==="disabled")==u.disabled)&&`${u.username} ${u.telegram?.chat_title||""}`.toLocaleLowerCase().includes(query.toLocaleLowerCase()));
  return <div className="users-page">
    <header className="users-heading"><div><h1>用户管理</h1><p>管理账号访问与专属归档空间。</p></div><button className="btn btn-crayon" onClick={()=>setEditor({mode:"create"})}>创建用户</button></header>
    <div className="users-toolbar"><label className="users-search"><span className="sr-only">搜索用户或群名</span><input className="input" type="search" placeholder="搜索用户或群名" value={query} onChange={e=>setQuery(e.target.value)}/></label><label className="users-filter">状态<select className="input" value={filter} onChange={e=>setFilter(e.target.value)}><option value="all">全部</option><option value="active">启用</option><option value="disabled">停用</option></select></label><span className="users-count" aria-live="polite">{loading?"读取中…":`${shown.length} 个账号`}</span><button className="btn" disabled={!!busy} onClick={()=>void action("refresh",async()=>{const result=await api.post<{items:User[]}>("/api/admin/accounts/telegram/refresh",{});if(mounted.current)setItems(result.items)},"TG 群名已同步")}>{busy==="refresh"?"同步中…":"同步 TG 群名"}</button></div>
    {error&&<div className="users-error" role="alert"><span>{error}</span><button className="btn btn-sm" onClick={()=>void load()}>重试读取</button></div>}
    <div className="users-registry"><table className="users-table" aria-label="用户列表"><colgroup><col className="users-account-column"/><col className="users-role-column"/><col className="users-status-column"/><col className="users-usage-column"/><col className="users-space-column"/><col className="users-actions-column"/></colgroup><thead><tr><th scope="col">用户</th><th scope="col">角色</th><th scope="col">状态</th><th scope="col">用量</th><th scope="col">TG 归档群</th><th scope="col" className="users-actions-heading">操作</th></tr></thead><tbody>
      {shown.map(user=>{const space=user.telegram;return <tr key={user.id} className="users-row" data-owner-id={user.id}>
        <th scope="row" className="users-account-cell"><strong className="users-name" title={user.username}>{user.username}</strong></th>
        <td className="users-role-cell" data-label="角色">{user.role==="admin"?"管理员":"普通用户"}</td>
        <td className="users-status-cell"><span className={`users-status${user.disabled?" is-disabled":""}`} title={user.delete_error}>{user.deleting ? user.delete_error ? "删除待重试" : "删除中" : user.disabled?"已停用":"已启用"}</span></td>
        <td className="users-usage-cell" data-label="用量"><p className="users-usage">{user.files} 个文件<span>·</span>{user.albums} 个相册</p><span className="users-size">{fmtSize(user.bytes)}</span></td>
        <td className="users-space-cell" data-label="TG 归档群">
        <p className="users-space" title={space?.chat_id?`TG 群 ID：${space.chat_id}`:undefined}>{space?.state==="ready"?<strong>{space.chat_title||"已分配"}</strong>:<span>{space?spaceStates[space.state]||space.state:"等待启用 TG 后自动分配"}</span>}</p>
          {(space?.desired_title||space?.last_error||space?.title_error)&&<span className="users-space-warning" title={space.title_error||space.last_error||`待同步：${space.desired_title}`}>{space.title_error||space.last_error?"待处理":"待同步"}</span>}
        </td>
        <td className="users-actions-cell"><div className="users-actions" role="group" aria-label={`${user.username}的操作`}>
          {user.role==="admin"?<><Link className="btn btn-sm" to="/admin/settings#settings-security">账号设置</Link>{space?.state==="ready"&&<button className="btn btn-sm" onClick={()=>setEditor({mode:"group",user})}>修改群名</button>}</>:<>
            <button className="btn btn-sm" disabled={user.deleting} onClick={()=>setEditor({mode:"edit",user})}>编辑</button>
            <button className="btn btn-sm" disabled={user.deleting} onClick={()=>setEditor({mode:"password",user})}>重置密码</button>
            {user.disabled ? <button className="btn btn-sm" disabled={!!busy || user.deleting} onClick={()=>void action(user.id,()=>api.patch(`/api/admin/accounts/${encodeURIComponent(user.id)}`,{disabled:false}),"账号已启用")}>启用</button> : <ConfirmButton className="btn btn-sm" disabled={!!busy || user.deleting} confirmLabel="确认停用" busyLabel="停用中…" resetKey={`${user.id}:${user.disabled}`} description={`停用「${user.username}」后，该用户无法登录或访问资料，现有会话和令牌会撤销。文件、相册及 TG 群继续保留。`} onConfirm={()=>action(user.id,()=>api.patch(`/api/admin/accounts/${encodeURIComponent(user.id)}`,{disabled:true}),"账号已停用，资料与归档已保留")}>停用</ConfirmButton>}
            <ConfirmButton className="btn btn-sm btn-danger" disabled={!!busy || user.deleting && !user.delete_error} confirmLabel="确认删除" busyLabel="提交中…" resetKey={`${user.id}:${user.files}:${user.deleting}`} description={`将永久删除「${user.username}」及其全部图片、视频、发布与归档副本（当前 ${user.files} 个文件）。后台清理完成后移除账号，失败时保留进度供重试。此操作不可恢复。`} onConfirm={()=>action(user.id,()=>api.del(`/api/admin/accounts/${encodeURIComponent(user.id)}`,{confirm:"DELETE"}),"删除已安排，后台将清理文件和归档")}>删除</ConfirmButton>
            {user.deleting && <Link className="btn btn-sm btn-ghost" to="/admin/tasks?type=account_delete">查看任务</Link>}
          </>}
          {space&&space.state!=="ready"&&!user.disabled&&<button className="btn btn-sm" disabled={!!busy||space.state==="creating"} onClick={()=>void action(user.id,()=>api.post(`/api/admin/accounts/${encodeURIComponent(user.id)}/storage/retry`,{}),"已安排重试分配")}>重试分配</button>}
          {space?.title_error&&space.desired_title&&<button className="btn btn-sm" disabled={!!busy} onClick={()=>void action(user.id,()=>api.patch(`/api/admin/accounts/${encodeURIComponent(user.id)}/telegram`,{name:space.desired_title}),"已安排重试群名同步")}>重试同步</button>}
        </div></td>
      </tr>})}
    </tbody></table>{!loading&&!shown.length&&<div className="empty"><p>没有匹配的用户</p><span>调整搜索或状态筛选后重试。</span></div>}{loading&&!items.length&&<p className="users-loading" role="status">正在读取用户…</p>}</div>
    <p className="users-note">停用账号保留文件、相册和 TG 群。TG 内改名后可同步群名；账号和归档位置保持关联。</p>
    {editor&&<UserEditor editor={editor} close={()=>setEditor(null)} saved={async(message)=>{setEditor(null);toast(message);await load()}}/>}
  </div>;
}

function UserEditor({editor,close,saved}:{editor:Editor;close:()=>void;saved:(message:string)=>Promise<void>}) {
  const ref=useRef<HTMLDivElement>(null),saving=useRef(false);
  const {mode,user}=editor;
  const initial=mode==="group"?user?.telegram?.desired_title||user?.telegram?.chat_title||"":mode==="edit"?user?.username||"":"";
  const initialGroup=user?.telegram?.desired_title||user?.telegram?.chat_title||"";
  const [group,setGroup]=useState(initialGroup);
  const [name,setName]=useState(initial),[password,setPassword]=useState(""),[busy,setBusy]=useState(false),[error,setError]=useState("");
  const requestClose=()=>{if(saving.current)return;if((name!==initial||password||group!==initialGroup)&&!confirm("放弃尚未保存的修改？"))return;close()};
  useDialog(ref,requestClose);
  const title={create:"创建用户",edit:"编辑用户资料",password:"重置密码",group:"修改 TG 群名"}[mode];
  const endpoint=`/api/admin/accounts/${encodeURIComponent(user?.id||"")}`;
  return <><div className="overlay" onClick={requestClose}/><div ref={ref} className="modal users-editor" role="dialog" aria-modal="true" aria-labelledby="users-editor-title"><header><h2 id="users-editor-title">{title}</h2><button type="button" className="btn btn-ghost" disabled={busy} onClick={requestClose}>关闭</button></header><form onSubmit={async e=>{
    e.preventDefault();if(saving.current)return;saving.current=true;setBusy(true);setError("");
    try {
      if(mode==="create")await api.post("/api/admin/accounts",{username:name.trim(),password});
      if(mode==="edit") {if(name.trim()!==initial)await api.patch(endpoint,{username:name.trim()});if(user?.telegram?.state==="ready"&&group.trim()!==initialGroup)await api.patch(endpoint+"/telegram",{name:group.trim()});}
      if(mode==="password")await api.post(endpoint+"/password",{password});
      if(mode==="group")await api.patch(endpoint+"/telegram",{name:name.trim()});
      await saved({create:"用户已创建，归档空间将自动分配",edit:"用户资料已保存，群名变更将自动同步",password:"密码已重置，旧会话与令牌已撤销",group:"群名修改已安排，完成后自动更新"}[mode]);
    }catch(err:any){setError(err.message);setBusy(false);saving.current=false}
  }}>
    {user&&<p className="users-editor-account">{user.username}</p>}
    {["create","edit","group"].includes(mode)&&<label>{mode==="group"?"群名":"用户名"}<input className="input" required maxLength={mode==="group"?128:64} autoComplete={mode==="group"?"off":"username"} value={name} onChange={e=>setName(e.target.value)} disabled={busy}/></label>}
    {mode==="edit"&&user?.telegram?.state==="ready"&&<label>TG 群名<input className="input" aria-label="TG 群名" required maxLength={128} value={group} onChange={e=>setGroup(e.target.value)} disabled={busy}/><span className="users-secondary">用户名与群名可独立修改，归档位置保持不变。</span></label>}
    {["create","password"].includes(mode)&&<label>{mode==="create"?"初始密码":"新密码"}<input className="input" aria-label={mode==="create"?"初始密码":"新密码"} type="password" autoComplete="new-password" required minLength={8} maxLength={72} value={password} onChange={e=>setPassword(e.target.value)} disabled={busy}/><span className="users-secondary">至少 8 位。</span></label>}
    {mode==="group"&&<p className="users-editor-note">同步修改 TG 群名，文件、话题和“相册”文件夹中的归属保持不变。</p>}
    {mode==="password"&&<p className="users-editor-note">该用户的现有登录会话和访问令牌将撤销，需使用新密码登录。</p>}
    {error&&<p className="users-error-text" role="alert">{error}</p>}
    <footer><button type="button" className="btn" disabled={busy} onClick={requestClose}>取消</button><button className="btn btn-crayon" disabled={busy}>{busy?"提交中…":{create:"创建用户",edit:"保存资料",password:"重置密码",group:"同步群名"}[mode]}</button></footer>
  </form></div></>;
}
