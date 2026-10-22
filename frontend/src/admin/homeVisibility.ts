import type { FileItem } from "../api";
export function homeVisibility(file: FileItem, _legacyAlbumID?: number) {
 if(file.purge_state || file.archive_state!=="done")return {state:"busy",title:"处理完成后更新公开状态",description:"首页不会公开未归档或待删除的文件。"};
 if(file.show_home)return {state:"visible",title:file.home_visibility==="public"?"已单独公开":"随相册公开",description:"游客只可浏览缩略图，不能读取原图或播放原视频。原件的永久发布独立管理。"};
 return {state:"outside",title:file.home_visibility==="private"?"已单独取消公开":"未公开",description:"管理员可公开文件或相册的缩略图；单独取消公开会优先隐藏此文件。原件的发布和加密状态独立管理。"};
}
