import { Link } from 'react-router-dom';
import type { TaskItem } from '../api';
export const typeNames: Record<string, string> = {
  system_schedule:"系统定时维护",
  local_import_batch:"服务器导入",
  sync_group:"归档同步",
  archive_group:"归档文件",
  upload_group:"上传文件",
  purge_group:"批量删除",
  publish_group:"批量发布",
  unpublish_group:"批量撤销发布",
  show_home_group:"批量公开",
  hide_home_group:"批量取消公开",
  move_album_group:"批量移动相册",
  set_media_time_group:"批量修改日期",
  convert_group:"转换归档",
  maintenance_group:"整理本地归档",
  telegram_receive_group:"接收 Telegram 文件",
  storage_probe: "检查归档可用性",
  archive_sync: "补齐归档存储",
  account_space: "分配账号归档空间",
  account_group_title: "同步 TG 群名",
  account_delete: "删除用户及归档",
  local_layout: "整理本地归档目录",
  local_import_scan: "预扫描服务器文件",
  local_import: "导入服务器文件",
  local_import_idle: "空闲时导入服务器文件",
  source_scan: "扫描导入来源",
  source_cleanup: "整理已归档源文件",
  archive: "加密归档",
  direct_archive: "未加密归档",
  import: "导入",
  temp_materialize: "准备临时预览",
  publish: "永久发布",
  unpublish: "撤销发布",
  verify: "恢复校验",
  purge_file: "彻底删除",
  cleanup_temp: "清理临时",
  transfer_owner: "转移文件归属",
  convert: "转换归档",
  cleanup_converted: "清理旧归档",
  tg_scan: "扫描 TG 话题",
  tg_receive: "接收 TG 新文件",
  tg_delete_album: "迁出并删除 TG 相册",
  tg_move_file: "迁移 TG 文件",
  tg_rename_album: "重命名 TG 话题",
  tg_create_album: "创建 TG 话题",
  cold_sync: "同步至副仓",
  cold_delete: "清理副仓副本",
};
export const taskCategories=[{value:"intake",label:"导入"},{value:"archival",label:"归档与同步"},{value:"publication",label:"发布"},{value:"cleanup",label:"删除与清理"},{value:"telegram",label:"TG 同步"},{value:"maintenance",label:"存储维护"}];
export const stateNames: Record<string, string> = { pending: "等待 / 自动重试", running: "执行中", done: "已完成", failed: "失败", canceled: "已取消" };
// file_exists means a *visible gallery record*, not that a server source was deleted.
export const fileUnavailableText="文件记录不可用";
const nonFileTasks=new Set(["system_schedule","storage_probe","account_space","account_group_title","account_delete","local_import_batch","local_import_scan","local_import","local_import_idle","source_scan","operation_intake","tg_scan","tg_delete_album","tg_rename_album","tg_create_album"]);
export function isFileReference(id:string|null|undefined):id is string {
 return !!id?.trim()&&!/^(?:tg-album|tg-space|tg-title|account-delete|system-schedule|local-import|storage-probe):/.test(id);
}
export function taskHasFile(task:{type:string;file_id?:string|null}){
 return !nonFileTasks.has(task.type)&&isFileReference(task.file_id);
}
export function taskResult(value:TaskItem["result"]):Record<string,unknown>{
 try{const result=typeof value==="string"?JSON.parse(value||"{}"):value;return result&&typeof result==="object"&&!Array.isArray(result)?result:{}}catch{return {}}
}
export function taskState(task:TaskItem){
 if(task.group?.kind==="import"){
  if(task.group.stage==="canceled"&&["running","pending"].includes(task.state))return "完成已接收文件的归档";
  if(task.group.stage==="paused")return "已暂停";
  if(task.group.stage==="ready")return "扫描完成";
  if(task.group.stage==="scanning")return task.state==="running"?"扫描中":"等待扫描";
 }
 if(task.cancel_requested&&["running","pending"].includes(task.state))return "停止中";
 if(task.group?.kind==="upload"&&["open","receiving"].includes(task.group.input_state||"")&&["running","pending"].includes(task.state))return "接收中";
 if(task.state==="failed"&&(task.group?.succeeded_files||0)>0)return "部分失败";
 if(task.state==="pending")return task.attempts>0?"等待重试":task.idle_only?"等待空闲":"排队中";
 if(task.state==="done"){const result=taskResult(task.result);if(result.skipped)return "已跳过";if(task.type==="verify"&&result.ok===true)return "校验通过"}
 return stateNames[task.state]||task.state;
}
export function taskSummary(task:TaskItem):string {
 const group=task.group;
 if(group){
  const total=group.total_files||0,success=group.succeeded_files,failed=group.failed_files??group.failed_steps,skipped=group.skipped_files,canceled=group.canceled_files;
  if(success!==undefined)return [total&&`共 ${total} 项`,success&&`成功 ${success} 项`,failed&&`失败 ${failed} 项`,skipped&&`跳过 ${skipped} 项`,canceled&&`取消 ${canceled} 项`].filter(Boolean).join(" · ")||(total?`共 ${total} 项`:"");
  return [total&&`共 ${total} 项`,failed&&`失败 ${failed} 步`].filter(Boolean).join(" · ");
 }
 const result=taskResult(task.result);
 if(task.type==="verify"&&result.ok===true)return "原件恢复校验通过";
 if(result.skipped)return ({album_removed_or_deleting:"相册已删除或正在删除，已跳过",source_import_rules_removed:"旧导入规则已移除，已跳过",telegram_disabled:"TG 归档未启用，已跳过"} as Record<string,string>)[String(result.skipped)]||`已跳过：${String(result.skipped)}`;
 return "";
}
export function taskCancelMessage(task:TaskItem):string {
 if(task.group?.kind==="purge"||task.type==="purge_file")return "只停止剩余删除；已经删除的文件无法恢复，服务器导入源文件保留。";
 if(task.group?.kind==="import"||task.group?.kind==="upload")return "停止接收后续文件，已经接收的文件继续归档；源文件保留。";
 if(task.group?.kind==="publish"||task.type==="publish")return "停止剩余发布；已经发布的文件继续公开，可另行撤销发布。";
 if(task.group?.kind==="unpublish"||task.type==="unpublish")return "停止剩余撤销；已经撤销发布的文件不会重新公开。";
 if(task.group?.kind==="move_album"||task.type==="tg_move_file")return "停止剩余迁移；已经完成的相册调整保留。";
 return "停止剩余步骤，已经完成的操作保留。";
}
export function RelatedContent({ id,owner,label }: { id: string;owner?:string;label?:string }) {
  if (!id) return <>—</>;
  if (id.startsWith("account-delete:")) return <>用户资料与归档</>;
  if (id.startsWith("tg-space:")) return <>账号归档空间</>;
  if (id.startsWith("tg-title:")) return <>TG 群名</>;
  if (id.startsWith("system-schedule:")) return <>系统定时维护</>;
  const album = /^tg-album:([1-9]\d*)$/.exec(id);
  if(!album&&!isFileReference(id))return null;
  const query=new URLSearchParams(album?{album_id:album[1]}:{file:id});if(owner)query.set("owner_id",owner);
  return <Link to={`/admin?${query}`}>{label||(album ? `相册 ${album[1]}` : id.slice(-6))}</Link>;
}
