import type {TaskItem} from '../api';
import {taskResult,taskState,taskSummary,typeNames} from './taskPresentation';

export type LogEntry=Omit<TaskItem,'attempts'|'max_attempts'>&{record:'task'|'receipt';record_key?:string;attempts:number|null;max_attempts:number|null;reason?:string;source?:string};
const legacy:Record<string,string>={local_import_batch:'import',local_import:'import',local_import_idle:'import',local_import_scan:'import',archive:'archive',direct_archive:'archive',archive_sync:'sync',cold_sync:'sync',purge_file:'purge',publish:'publish',unpublish:'unpublish',tg_move_file:'move_album',transfer_owner:'move_album',convert:'convert',local_layout:'maintenance',tg_receive:'telegram_receive'};
export function logState(row:LogEntry){return row.record==='receipt'?({processing:'pending',archived:'done',published:'done',ignored:'canceled',deleted:'done',duplicate:'done'}[row.state]||row.state):row.state}
export function logActive(row:LogEntry){return ['pending','running'].includes(logState(row))||row.group?.kind==='import'&&row.group.stage==='ready'}
export function logTask(row:LogEntry):TaskItem{return {...row,state:logState(row),attempts:row.attempts||0,max_attempts:row.max_attempts||0}}
export function logKind(row:LogEntry){return row.group?.kind||legacy[row.type]||row.type.replace(/_group$/,'')}
const quoted=(name:string)=>`「${name}」`;
export function logMessage(row:LogEntry){
 const kind=logKind(row),context=row.context,files=row.group?.total_files?`${row.group.total_files} 个文件`:row.file_name?quoted(row.file_name):'文件';
 const target=context?.to_album?.name||row.album_name,from=context?.from_albums?.map(a=>quoted(a.name)).join('、')||(row.album_name?quoted(row.album_name):''),stores=context?.storages?.join('、');
 const destination=target?quoted(target):'目标相册未记录';
 if(row.type==='system_schedule')return row.file_name||'系统维护';
 if(kind==='import')return `${row.type==='local_import_scan'||row.group?.stage==='scanning'?'扫描':'导入'} ${files} → ${destination}`;
 if(kind==='upload')return `上传 ${files} → ${destination}`;
 if(kind==='move_album')return `移动 ${files} · ${from||'原相册未记录'} → ${destination}`;
 if(kind==='purge')return `删除 ${files}${from?` · 来自${from}`:''}`;
 if(kind==='publish')return `发布 ${files}${from?` · ${from}`:''}${stores?` → ${stores}`:''}`;
 if(kind==='unpublish')return `撤销发布 ${files}${from?` · ${from}`:''}${stores?` · ${stores}`:''}`;
 if(kind==='sync'||kind==='archive')return `${kind==='sync'?'同步归档':'归档'} ${files}${from?` · ${from}`:''}${stores?` → ${stores}`:''}`;
 if(kind==='telegram_receive')return `接收 Telegram ${files} → ${destination}`;
 if(kind==='convert')return `转换归档 ${files}${context?.description?` · ${context.description}`:''}${stores?` · ${stores}`:''}`;
 if(kind==='maintenance')return `整理本地归档 ${files}${from?` · ${from}`:''}`;
 if(row.record==='receipt')return `${row.state==='processing'?'接收':row.type==='duplicate'?'跳过重复文件':row.type==='retired_ignored'?'忽略旧来源文件':'接收'} ${files}${target?` → ${quoted(target)}`:''}`;
 if(row.type==='account_delete')return `删除用户${row.file_name?quoted(row.file_name):''}及其归档`;
 const subject=context?.description||(row.file_name&&row.file_name!==typeNames[row.type]?quoted(row.file_name):row.file_id?.startsWith('tg-space:')?`账号 ${row.file_id.slice(9)}`:row.file_id?.startsWith('tg-title:')?`账号 ${row.file_id.slice(9)}`:'');
 return `${typeNames[row.type]||'后台操作'}${subject?` · ${subject}`:''}${from?` · ${from}`:''}${stores?` · ${stores}`:''}`;
}
export function logOutcome(row:LogEntry){
 const result=taskResult(row.result);
 if(row.type==='system_schedule'){
  if(Array.isArray(result.checks))return result.checks.map((check:any)=>`${check.name} ${check.available===true?`可用 ${Math.round(check.latency_ms||0)}ms`:check.available===false?`不可用${check.error?`：${check.error}`:''}`:'未完成检查'}`).join(' · ');
  return typeof result.summary==='string'?result.summary:'';
 }
 const g=row.group;
 if(g)return [g.succeeded_files?`成功 ${g.succeeded_files}`:'',g.failed_files?`失败 ${g.failed_files}`:'',g.skipped_files?`跳过 ${g.skipped_files}`:'',g.canceled_files?`取消 ${g.canceled_files}`:'',g.unsubmitted_files?`未提交 ${g.unsubmitted_files}`:''].filter(Boolean).join(' · ');
 if(row.record==='receipt')return ({duplicate:'沿用已有文件',published:'已归档并发布',archived:'原件已归档'}[row.state]||'');
 return taskSummary(logTask(row));
}
export function logReason(row:LogEntry){
 const result=taskResult(row.result),reason=row.last_error||(logState(row)==='failed'?row.reason:'')||'';
 if(reason.trim()===logOutcome(row).trim()||Array.isArray(result.checks)&&reason===result.warning)return '';
 return reason;
}
export function logStatus(row:LogEntry){
 if(row.type==='system_schedule'&&Number(taskResult(row.result).unavailable)>0)return '有异常';
 if(row.record==='receipt'&&!row.group)return ({processing:'处理中',archived:'已完成',published:'已完成',duplicate:'已跳过',failed:'失败',deleted:'已完成',ignored:'已忽略'}[row.state]||row.state);
 return taskState(logTask(row));
}
export function logTone(row:LogEntry){
 if(logState(row)==='failed')return 'failed';
 if(row.type==='system_schedule'&&Number(taskResult(row.result).unavailable)>0)return 'warning';
 return logState(row);
}
