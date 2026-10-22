import type {FileItem} from "../api";
import type {SimilarityComparison} from "./imageSimilarity";

type ImageFile=Pick<FileItem,"id"|"size"|"width"|"height"|"ext"|"sha256">;
const pixels=(file:ImageFile)=>file.width>0&&file.height>0?file.width*file.height:0;
const encoding=(file:ImageFile)=>file.ext.toLowerCase().replace(/^jpeg$/,"jpg").replace(/^heif$/,"heic");

// A recommendation describes evidence, never certifies that a file is the
// camera original. More complete local detail outranks both resolution and bytes.
export function orderImagePair<T extends ImageFile>(a:T,b:T,comparison?:SimilarityComparison):{left:T;right:T;recommendation?:string}{
 let preferred:-1|0|1=0,reason:string|undefined;
 const sameContent=/^[0-9a-f]{64}$/i.test(a.sha256)&&a.sha256.toLowerCase()===b.sha256.toLowerCase(),pa=pixels(a),pb=pixels(b);
 if(!sameContent&&comparison?.kind==="edited"&&comparison.detailPreference){preferred=comparison.detailPreference;reason="局部细节更完整，较少打码或遮挡迹象"}
 if(!sameContent&&!preferred&&comparison?.kind==="near"){
  if(pa>0&&pb>0&&Math.max(pa,pb)>=Math.min(pa,pb)*1.25){
   const bigger=pa>pb?a:b,smaller=pa>pb?b:a;
   if(bigger.width>=smaller.width*.98&&bigger.height>=smaller.height*.98){preferred=pa>pb?-1:1;reason="画面高度相似，原文件分辨率更高"}
  }
  if(!preferred&&pa>0&&pa===pb&&encoding(a)===encoding(b)&&a.size>0&&b.size>0&&comparison.meanError<=3&&Math.max(a.size,b.size)>=Math.min(a.size,b.size)*1.3){preferred=a.size>b.size?-1:1;reason="相同格式和分辨率，原文件更大"}
 }
 // Weak evidence only orders the pair. Cross-format file size alone is never
 // enough to display a badge; JPEG and HEIC byte counts are not comparable quality.
 if(!preferred){
  if(pa&&pb&&pa!==pb)preferred=pa>pb?-1:1;
  else if(a.size>0&&b.size>0&&a.size!==b.size)preferred=a.size>b.size?-1:1;
  else preferred=a.id.localeCompare(b.id)<=0?-1:1;
 }
 return preferred===-1?{left:b,right:a,recommendation:reason}:{left:a,right:b,recommendation:reason};
}
