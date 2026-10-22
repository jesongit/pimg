import {useState} from "react";
import type {ReactNode, SelectHTMLAttributes} from "react";
import type { Album } from "../api";

export type AlbumOwner = { id: string; username: string; disabled?: boolean; deleting?: boolean };
export function albumOwnerLabel(owner: string, users: AlbumOwner[], actor: {owner_id:string;username:string;role?:string}) {
  const user = users.find(user => user.id === owner);
  return user ? `${user.username}${user.disabled || user.deleting ? "（已停用）" : ""}` : owner === actor.owner_id ? `${actor.username}${actor.role === "admin" || owner === "admin:1" ? "（管理员）" : ""}` : owner === "admin:1" ? "管理员" : owner;
}
export function transferDescription(user: string, album: string) {
  return `将文件归属转给「${user}」，放入「${album}」。原件复制并校验成功后才切换归属；新文件默认私有，使用新编号，旧编号停止通过本站访问。旧公开副本随后清理，已缓存或保存的副本无法收回。转移期间暂停其他整理操作，失败时保留原文件，可在任务页重试。`;
}
type AlbumOptionsProps={albums:Album[];grouped:boolean;users:AlbumOwner[];actor:{owner_id:string;username:string};owner:string};
export function moveAlbumAvailable(album:Pick<Album,'owner_id'|'telegram'>|undefined,users:AlbumOwner[]) {
  return !!album&&!['deleting','deleted'].includes(album.telegram?.state||'')&&!users.some(user=>user.id===album.owner_id&&(user.disabled||user.deleting));
}
/** Native menus keep their option order until the user finishes choosing. */
export function MoveAlbumSelect({albums,grouped,users,actor,owner,children,onFocus,onBlur,...props}:AlbumOptionsProps&Omit<SelectHTMLAttributes<HTMLSelectElement>,'children'>&{children:ReactNode}) {
  const [snapshot,setSnapshot]=useState<AlbumOptionsProps|null>(null);
  return <select {...props} onFocus={event=>{setSnapshot({albums,grouped,users,actor,owner});onFocus?.(event)}} onBlur={event=>{setSnapshot(null);onBlur?.(event)}}>{children}<MoveAlbumOptions {...(snapshot||{albums,grouped,users,actor,owner})}/></select>;
}
export default function MoveAlbumOptions({albums, grouped, users, actor, owner}:AlbumOptionsProps) {
  const option = (album: Album) => <option key={album.id} value={album.id} disabled={["deleting","deleted"].includes(album.telegram?.state || "")}>{album.name}{album.is_home ? "（已发布内容上首页）" : ""}</option>;
  if (!grouped) return <>{albums.map(option)}</>;
  const groups = new Map<string, Album[]>();
  for (const album of albums) { const key=album.owner_id || owner; groups.set(key,[...(groups.get(key)||[]),album]); }
  return <>{[...groups].sort(([a],[b])=>a===owner?-1:b===owner?1:albumOwnerLabel(a,users,actor).localeCompare(albumOwnerLabel(b,users,actor),"zh-CN")).map(([id,items])=><optgroup key={id} label={`${albumOwnerLabel(id,users,actor)}${id===owner?" · 当前图库":""}`} disabled={users.some(user=>user.id===id&&(user.disabled||user.deleting))}>{items.map(option)}</optgroup>)}</>;
}
