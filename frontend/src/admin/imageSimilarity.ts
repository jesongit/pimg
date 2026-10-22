// A compact perceptual fingerprint: low-frequency luminance DCT, guarded by
// aspect ratio and sampled RGB differences. Matches are review candidates,
// never proof that the originals have equal quality or interchangeable content.
export type Fingerprint={hash:bigint;rgb:Uint8Array;aspect:number;variance:number};
type DetailTile={texture:number;flat:number;residual:number};
export type SimilarityImage={fingerprint:Fingerprint;pixels:Uint8ClampedArray;tiles?:DetailTile[]};
export type DifferenceRegion={x:number;y:number;width:number;height:number};
export type SimilarityComparison={similar:boolean;kind:"different"|"near"|"edited";meanError:number;maxTileError:number;localizedDifference:boolean;detailPreference:-1|0|1;regions:DifferenceRegion[]};
const detailSide=256;
const side=32,cos=Array.from({length:8},(_,u)=>Float64Array.from({length:side},(_,x)=>Math.cos((2*x+1)*u*Math.PI/(side*2))));
export function fingerprint(pixels:Uint8ClampedArray,width:number,height:number):Fingerprint {
 if(pixels.length!==side*side*4||width<=0||height<=0)throw Error("无效的图片采样");
 const gray=new Float64Array(side*side),rgb=new Uint8Array(16*16*3);
 for(let i=0;i<gray.length;i++)gray[i]=pixels[i*4]*.299+pixels[i*4+1]*.587+pixels[i*4+2]*.114;
 for(let y=0;y<16;y++)for(let x=0;x<16;x++)for(let c=0;c<3;c++)rgb[(y*16+x)*3+c]=(pixels[((y*2)*side+x*2)*4+c]+pixels[((y*2)*side+x*2+1)*4+c]+pixels[((y*2+1)*side+x*2)*4+c]+pixels[((y*2+1)*side+x*2+1)*4+c])/4;
 const rows=new Float64Array(side*8),values:number[]=[];
 for(let y=0;y<side;y++)for(let u=0;u<8;u++)for(let x=0;x<side;x++)rows[y*8+u]+=gray[y*side+x]*cos[u][x];
 for(let v=0;v<8;v++)for(let u=0;u<8;u++){let value=0;for(let y=0;y<side;y++)value+=rows[y*8+u]*cos[v][y];if(u||v)values.push(value)}
 const median=[...values].sort((a,b)=>a-b)[31];let hash=0n;
 // Ignore floating point noise in uniform images.
 for(let i=0;i<values.length;i++)if(values[i]>median+1e-6)hash|=1n<<BigInt(i);
 const mean=gray.reduce((sum,value)=>sum+value,0)/gray.length;
 const variance=gray.reduce((sum,value)=>sum+(value-mean)**2,0)/gray.length;
 return {hash,rgb,aspect:width/height,variance};
}
export function hamming(a:bigint,b:bigint){let bits=a^b,count=0;while(bits){bits&=bits-1n;count++}return count}
export function pixelDifference(a:Fingerprint,b:Fingerprint){if(Math.abs(Math.log(a.aspect/b.aspect))>.02)return Infinity;let sum=0;for(let i=0;i<a.rgb.length;i++)sum+=Math.abs(a.rgb[i]-b.rgb[i]);return sum/a.rgb.length}
type Entry<T>={value:T;fingerprint:Fingerprint};
type Node<T>={hash:bigint;entries:Entry<T>[];children:Map<number,Node<T>>};
export class SimilarIndex<T>{
 private root?:Node<T>;
 add(value:T,fingerprint:Fingerprint){if(fingerprint.variance<16)return;const entry={value,fingerprint};let node=this.root;if(!node){this.root={hash:fingerprint.hash,entries:[entry],children:new Map()};return}for(;;){const distance=hamming(node.hash,fingerprint.hash);if(!distance){node.entries.push(entry);return}const next=node.children.get(distance);if(next){node=next;continue}node.children.set(distance,{hash:fingerprint.hash,entries:[entry],children:new Map()});return}}
 // Hashes retrieve candidates only. A caller must verify their detailed pixels.
 candidates(fingerprint:Fingerprint):T[]{
  if(fingerprint.variance<16)return [];
  const queue=this.root?[this.root]:[],found:{value:T;score:number}[]=[];
  while(queue.length){const node=queue.pop()!,distance=hamming(node.hash,fingerprint.hash);if(distance<=10)for(const entry of node.entries){const error=pixelDifference(fingerprint,entry.fingerprint);if(error<18)found.push({value:entry.value,score:error+distance})}for(const [edge,child]of node.children)if(edge>=distance-10&&edge<=distance+10)queue.push(child)}
  return found.sort((a,b)=>a.score-b.score).slice(0,32).map(entry=>entry.value);
 }
}

// Sample fine texture independently of the pair. This is cached only with the
// bounded decoded-image cache, not in the album-wide fingerprint index.
function detailTiles(image:SimilarityImage):DetailTile[]{
 if(image.tiles)return image.tiles;
 const size=detailSide,gray=Float64Array.from({length:size*size},(_,i)=>image.pixels[i*4]*.299+image.pixels[i*4+1]*.587+image.pixels[i*4+2]*.114),tiles:DetailTile[]=[];
 for(let y=0;y<size;y+=16)for(let x=0;x<size;x+=16){
  let texture=0,flat=0,residual=0;
  for(let dy=1;dy<15;dy++)for(let dx=1;dx<15;dx++){
   const i=(y+dy)*size+x+dx,difference=Math.abs(gray[i]-gray[i+1])+Math.abs(gray[i]-gray[i+size]);
   texture+=Math.min(difference,20);if(difference<1.5)flat++;
   residual+=Math.min(10,Math.abs(gray[i]-(gray[i-1]+gray[i+1]+gray[i-size]+gray[i+size])/4));
  }
  tiles.push({texture:texture/196,flat:flat/196,residual:residual/196});
 }
 return image.tiles=tiles;
}

function differenceRegions(changed:number[]):DifferenceRegion[]{
 const remaining=new Set(changed),regions:DifferenceRegion[]=[];
 while(remaining.size){
  const first=remaining.values().next().value!,queue=[first];remaining.delete(first);let minX=16,minY=16,maxX=0,maxY=0;
  while(queue.length){const tile=queue.pop()!,x=tile%16,y=Math.floor(tile/16);minX=Math.min(minX,x);maxX=Math.max(maxX,x);minY=Math.min(minY,y);maxY=Math.max(maxY,y);
   for(let dy=-1;dy<=1;dy++)for(let dx=-1;dx<=1;dx++){const nx=x+dx,ny=y+dy,next=ny*16+nx;if(nx>=0&&nx<16&&ny>=0&&ny<16&&remaining.delete(next))queue.push(next)}
  }
  regions.push({x:minX/16,y:minY/16,width:(maxX-minX+1)/16,height:(maxY-minY+1)/16});
 }
 // Keep disconnected edits visible without creating hundreds of DOM elements.
 if(regions.length<=6)return regions;
 const x=Math.min(...regions.map(r=>r.x)),y=Math.min(...regions.map(r=>r.y));
 return [{x,y,width:Math.max(...regions.map(r=>r.x+r.width))-x,height:Math.max(...regions.map(r=>r.y+r.height))-y}];
}

// The strict path still rejects changed numbers/captions. The separate edited
// path requires aligned photographic texture over most of the frame AND
// directional evidence of lost texture inside a small set of changed tiles.
export function compareSimilarity(a:SimilarityImage,b:SimilarityImage):SimilarityComparison{
 const rejected:SimilarityComparison={similar:false,kind:"different",meanError:Infinity,maxTileError:Infinity,localizedDifference:true,detailPreference:0,regions:[]};
 if(a.pixels.length!==detailSide*detailSide*4||b.pixels.length!==detailSide*detailSide*4||Math.abs(Math.log(a.fingerprint.aspect/b.fingerprint.aspect))>.02)return rejected;
 const size=detailSide,block=16,tiles:{error:number;ssim:number}[]=[];let total=0,maxTileError=0;
 for(let y=0;y<size;y+=block)for(let x=0;x<size;x+=block){
  let error=0,s1=0,s2=0,ss1=0,ss2=0,s12=0;
  for(let dy=0;dy<block;dy++)for(let dx=0;dx<block;dx++){
   const i=((y+dy)*size+x+dx)*4;
   const ar=a.pixels[i],ag=a.pixels[i+1],ab=a.pixels[i+2],br=b.pixels[i],bg=b.pixels[i+1],bb=b.pixels[i+2];
   error+=Math.abs(ar-br)+Math.abs(ag-bg)+Math.abs(ab-bb);
   const p=ar*.299+ag*.587+ab*.114,q=br*.299+bg*.587+bb*.114;
   s1+=p;s2+=q;ss1+=p*p;ss2+=q*q;s12+=p*q;
  }
  const n=block*block,m1=s1/n,m2=s2/n,v1=Math.max(0,ss1/n-m1*m1),v2=Math.max(0,ss2/n-m2*m2),cov=s12/n-m1*m2;
  const ssim=(2*m1*m2+6.5025)*(2*cov+58.5225)/((m1*m1+m2*m2+6.5025)*(v1+v2+58.5225));
  const mean=error/(n*3);tiles.push({error:mean,ssim});maxTileError=Math.max(maxTileError,mean);total+=error;
 }
 const meanError=total/(size*size*3);
 const localizedDifference=tiles.some(tile=>tile.error>10&&tile.error>meanError*4+3&&tile.ssim<.9);
 const result={...rejected,meanError,maxTileError,localizedDifference};
 if(meanError<=12&&maxTileError<=28&&!localizedDifference)return {...result,similar:true,kind:"near"};
 if(meanError>10||hamming(a.fingerprint.hash,b.fingerprint.hash)>6||pixelDifference(a.fingerprint,b.fingerprint)>8)return result;
 const ta=detailTiles(a),tb=detailTiles(b),changed:number[]=[],quadrants=new Set<number>();let preserved=0,textured=0,preferA=0,preferB=0;
 for(let i=0;i<tiles.length;i++){
  const tile=tiles[i];
  if(tile.error<=4&&tile.ssim>=.94){preserved++;if(ta[i].residual>1.2&&tb[i].residual>1.2&&ta[i].flat<.6&&tb[i].flat<.6){textured++;quadrants.add((i%16>=8?1:0)+(i>=128?2:0))}}
  if(tile.error>8&&tile.ssim<.94){
   changed.push(i);
   const losesTexture=(source:DetailTile,edited:DetailTile)=>source.texture>2&&((edited.flat-source.flat>.1&&edited.texture<source.texture*.95)||(source.residual>1.8&&edited.residual<source.residual*.6));
   if(losesTexture(ta[i],tb[i]))preferA++;
   if(losesTexture(tb[i],ta[i]))preferB++;
  }
 }
 const votes=Math.max(preferA,preferB),opposing=Math.min(preferA,preferB),confident=votes>=Math.max(2,changed.length*.3)&&opposing<=votes*.2;
 if(preserved<tiles.length*.75||textured<32||quadrants.size<3||changed.length<2||changed.length>tiles.length*.22)return result;
 // When lost-detail evidence is inconclusive, only an exceptionally aligned
 // photographic pair can be shown, and neither side receives a recommendation.
 if(!confident&&(preserved<tiles.length*.9||textured<96||quadrants.size<4))return result;
 return {...result,similar:true,kind:"edited",detailPreference:confident?(preferA>preferB?-1:1):0,regions:differenceRegions(changed)};
}

export async function readSimilarityImage(url:string,signal:AbortSignal):Promise<SimilarityImage>{
 const controller=new AbortController(),abort=()=>controller.abort(),timer=setTimeout(abort,20000);signal.addEventListener("abort",abort,{once:true});if(signal.aborted)abort();
 let bitmap:ImageBitmap|undefined;
 try {
  const response=await fetch(url,{credentials:"same-origin",signal:controller.signal,cache:"no-store"});if(!response.ok)throw Error(`预览读取失败 (${response.status})`);
  bitmap=await createImageBitmap(await response.blob());if(controller.signal.aborted)throw new DOMException("Aborted","AbortError");
  const sample=(size:number)=>{const canvas=document.createElement("canvas");canvas.width=canvas.height=size;const context=canvas.getContext("2d",{willReadFrequently:true});if(!context)throw Error("浏览器不支持图片对比");context.imageSmoothingQuality="high";context.fillStyle="#fff";context.fillRect(0,0,size,size);context.drawImage(bitmap!,0,0,size,size);return context.getImageData(0,0,size,size).data};
  return {fingerprint:fingerprint(sample(side),bitmap.width,bitmap.height),pixels:sample(detailSide)};
 }finally{bitmap?.close();clearTimeout(timer);signal.removeEventListener("abort",abort)}
}
