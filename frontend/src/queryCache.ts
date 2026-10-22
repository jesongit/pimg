// Bounded metadata snapshots. Hydrate only after /auth/me validates this exact
// login; credentials and provider settings never enter persistence.
type Entry={value:any;etag?:string;at:number;size:number};
const entries=new Map<string,Entry>();
const MAX_ENTRIES=96,MAX_BYTES=16*1024*1024,MAX_AGE=15*60*1000;
let scope="",epoch=0,identityEpoch=0,bytes=0,snapshotKey="",saveTimer:number|undefined;
const SNAPSHOT_PREFIX="pimg:query-snapshot:v1:",SNAPSHOT_BYTES=2*1024*1024;
function removeSnapshots(){try{for(const key of Object.keys(localStorage))if(key.startsWith(SNAPSHOT_PREFIX))localStorage.removeItem(key)}catch{}}
export function clearQueryCache(){entries.clear();bytes=0;epoch++;identityEpoch++;if(saveTimer!==undefined)clearTimeout(saveTimer);saveTimer=undefined;snapshotKey="";removeSnapshots()}
export function setQuerySession(identity:string,session=""){
 const next=session?SNAPSHOT_PREFIX+identity+":"+session:"";
 if(scope===identity&&snapshotKey===next)return;
 entries.clear();bytes=0;epoch++;identityEpoch++;
 if(saveTimer!==undefined)clearTimeout(saveTimer);saveTimer=undefined;
 scope=identity;snapshotKey=next;
 try{
  for(const key of Object.keys(localStorage))if(key.startsWith(SNAPSHOT_PREFIX)&&key!==next)localStorage.removeItem(key);
  const raw=next?localStorage.getItem(next):null;if(!raw||raw.length*2>SNAPSHOT_BYTES)return;
  const saved=JSON.parse(raw);if(!Array.isArray(saved))return;
  for(const [key,entry] of saved){if(typeof key!=="string"||!key.startsWith(scope+"|")||!persistable(key)||!entry||typeof entry.at!=="number"||Date.now()-entry.at>MAX_AGE||entry.at>Date.now()+1000)continue;
   const size=JSON.stringify(entry.value).length*2;if(size>SNAPSHOT_BYTES||entries.size>=MAX_ENTRIES||bytes+size>SNAPSHOT_BYTES)continue;
   entries.set(key,{value:entry.value,etag:typeof entry.etag==="string"?entry.etag:undefined,at:entry.at,size});bytes+=size;
  }
 }catch{try{if(next)localStorage.removeItem(next)}catch{}}
}
function persistable(key:string){return /\/api\/admin\/(logs|tasks\/summary|schedules)(?:\?|$)/.test(key)}
function saveSnapshots(){
 if(!snapshotKey)return;
 try{let size=0;const saved=[];for(const pair of [...entries].reverse()){if(!persistable(pair[0])||Date.now()-pair[1].at>MAX_AGE)continue;const length=JSON.stringify(pair).length*2;if(size+length>SNAPSHOT_BYTES)continue;size+=length;saved.push(pair)}localStorage.setItem(snapshotKey,JSON.stringify(saved))}catch{}
}
function scheduleSave(){if(!snapshotKey||saveTimer!==undefined)return;saveTimer=window.setTimeout(()=>{saveTimer=undefined;saveSnapshots()},100)}
export function queryEpoch(){return epoch}
export function queryIdentityEpoch(){return identityEpoch}
export function invalidateQueryRequests(){epoch++}
export function queryKey(path:string,owner=""){const url=new URL(path,window.location.origin);url.searchParams.sort();return `${scope}|${owner}|${url.pathname}${url.search}`}
export function cacheableQuery(path:string){return /^\/api\/(admin|account)\/(files(?:\/(timeline|navigation|previews))?|albums|accounts|storage|settings|status|logs|schedules|tasks\/summary|telegram)$/.test(new URL(path,window.location.origin).pathname)}
export function queryEntry(key:string){const found=entries.get(key);if(found&&Date.now()-found.at>MAX_AGE){entries.delete(key);bytes-=found.size;return undefined}if(found){entries.delete(key);entries.set(key,found)}return found}
export function refreshQuery(key:string){const found=entries.get(key);if(found){found.at=Date.now();if(persistable(key))scheduleSave()}}
// Retain unchanged rows/fields so refreshes do not replace the displayed tree.
export function shareQuery(previous:any,next:any):any{
 if(Object.is(previous,next))return previous;
 if(!previous||!next||typeof previous!=="object"||typeof next!=="object"||Array.isArray(previous)!==Array.isArray(next))return next;
 const keys=Object.keys(next);let equal=Object.keys(previous).length===keys.length;
 const byID=Array.isArray(next)&&next.every(v=>v&&typeof v==="object"&&(v.record_key!==undefined||v.id!==undefined))?new Map(previous.map((v:any)=>[v?.record_key??v?.id,v])):null;
 for(const key of keys){next[key]=shareQuery(byID?byID.get(next[key].record_key??next[key].id):previous[key],next[key]);if(!Object.is(next[key],previous[key]))equal=false}
 return equal?previous:next;
}
export function storeQuery(key:string,value:any,etag:string|undefined,startEpoch:number){
 if(startEpoch!==epoch)return value;
 const old=queryEntry(key),shared=shareQuery(old?.value,value),size=JSON.stringify(shared).length*2;
 if(old){entries.delete(key);bytes-=old.size}
 if(size>MAX_BYTES)return shared;
 entries.set(key,{value:shared,etag,at:Date.now(),size});bytes+=size;
 while(entries.size>MAX_ENTRIES||bytes>MAX_BYTES){const first=entries.keys().next().value!;bytes-=entries.get(first)!.size;entries.delete(first)}
 if(persistable(key))scheduleSave();
 return shared;
}
if(typeof window!=="undefined")for(const event of ["pimg:auth-changed","pimg:unauthorized"])window.addEventListener(event,clearQueryCache);
if(typeof window!=="undefined"){
 window.addEventListener("pagehide",saveSnapshots);
 window.addEventListener("storage",event=>{if(event.key===snapshotKey&&event.newValue===null)clearQueryCache()});
}
