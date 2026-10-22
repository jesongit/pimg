import FilterSelect from "./FilterSelect";
import CycleFilter from "./CycleFilter";
import { useGallery } from "./GalleryScope";
import type { ReactNode } from "react";
import type { DateGrouping } from "./Timeline";

const glyph = (path: string) => <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={path} /></svg>;

export function BrowseFilters({ children, reset, kind, state, encryption, sort, grouping, groupingDisabled, onChange, onGroupingChange }: { children?: ReactNode; reset?: ReactNode; kind: string; state: string; encryption: string; sort: string; grouping: DateGrouping; groupingDisabled: boolean; onChange: (key: string, value: string) => void; onGroupingChange: (value: DateGrouping) => void }) {
  const {admin}=useGallery();
  return <div className="browse-inline-filters" role="group" aria-label="图库筛选条件">
    <div className="library-filter-scope">{children}</div>
    <div className="library-filter-content">
    <CycleFilter label="文件类型" prefix="类型" displayLabels={{"":"全部"}} value={kind} active={!!kind} onChange={value=>onChange("kind",value)} options={[{value:"",label:"全部类型"},{value:"image",label:"图片"},{value:"video",label:"视频"}]}/>
    <FilterSelect label="发布与归档状态" prefix="状态" displayLabel={state?undefined:"全部"} value={state} active={!!state} onChange={value=>onChange("state",value)} options={[{value:"",label:"全部状态"},{value:"unpublished",label:"未发布"},{value:"published",label:"已发布"},{value:"archived",label:"已归档"},...(admin?[{value:"show_home",label:"首页公开"}]:[]),{value:"failed",label:"有失败"}]}/>
    <FilterSelect label="加密条件" prefix="加密" displayLabel={encryption?undefined:"不限"} value={encryption} active={!!encryption} onChange={value=>onChange("encryption",value)} options={[{value:"",label:"任意加密"},{value:"encrypted",label:"已加密"},{value:"plaintext",label:"未加密"}]}/>
    </div>
    <div className="library-filter-view">
    <FilterSelect label="排序" prefix="排序" displayLabel={({newest:"拍摄日期 ↓",oldest:"拍摄日期 ↑",name:"名称",size:"大小 ↓",random:"随机排列"} as Record<string,string>)[sort]} value={sort} active={sort!=="newest"} onChange={value=>{if(value!==sort)onChange("sort",value)}} options={[{value:"newest",label:"拍摄日期从新到旧"},{value:"oldest",label:"拍摄日期从旧到新"},{value:"name",label:"名称排序"},{value:"size",label:"大小递减"},{value:"random",label:"随机排列"}]}/>
    <CycleFilter label="时间分区" prefix="分区" displayLabels={{day:"按日",week:"按周",month:"按月",year:"按年"}} value={grouping} active={grouping!=="day"} disabled={groupingDisabled} title={groupingDisabled?"按拍摄日期排序后可选择时间分区":"按拍摄／录制日期分区；缺少拍摄信息时使用源文件创建时间或导入时间"} onChange={value=>onGroupingChange(value as DateGrouping)} options={[{value:"day",label:"按日分区"},{value:"week",label:"按周分区"},{value:"month",label:"按月分区"},{value:"year",label:"按年分区"}]}/>
    {reset}
    </div>
  </div>;
}

export function UploadActions({ dragging, onDragging, onUpload, onFiles, onImport, onCreateAlbum }: { dragging: boolean; onDragging: (value: boolean) => void; onUpload: () => void; onFiles: (files: File[]) => void; onImport?: () => void; onCreateAlbum: () => void }) {
  return <div className="library-upload-actions" role="group" aria-label="上传、新建相册和导入">
    <button type="button" className={`btn btn-crayon library-upload-button${dragging ? " drag" : ""}`} title="选择文件并设置相册、加密选项；也可拖入文件" onClick={onUpload} onDragOver={event=>{if(event.dataTransfer.types.includes("Files")){event.preventDefault();event.dataTransfer.dropEffect="copy";onDragging(true)}}} onDragLeave={event=>{if(!event.currentTarget.contains(event.relatedTarget as Node|null))onDragging(false)}} onDrop={event=>{const files=Array.from(event.dataTransfer.files);if(files.length){event.preventDefault();event.stopPropagation();onDragging(false);onFiles(files)}}}>{glyph("M12 16V4m-4 4 4-4 4 4M5 16v4h14v-4")}上传</button>
    <button type="button" className="btn library-upload-button library-create-album-button" onClick={onCreateAlbum}>{glyph("M3 7V5h6l2 2h10v13H3V7m9 3v7m-3-3h6")}新建相册</button>
    {onImport && <button type="button" className="btn library-upload-button" onClick={onImport}>{glyph("M3 7V5a1 1 0 0 1 1-1h5l2 3h9a1 1 0 0 1 1 1v11a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V7m9 3v7m-3-3 3 3 3-3")}导入</button>}
  </div>;
}
