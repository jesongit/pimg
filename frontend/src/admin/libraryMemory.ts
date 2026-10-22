import type {AuthUser} from "../api";

const key=(actor:AuthUser)=>`pimg:library:last:${actor.owner_id}:${actor.role}`;
const fields=["owner_id","album_id","q","kind","state","encryption","sort","seed","group","start_page","pages"];
export function libraryQuery(actor:AuthUser,params:URLSearchParams){
 const next=new URLSearchParams();
 for(const field of fields)if(params.has(field)&&(field!=="owner_id"||actor.role==="admin"))next.set(field,params.get(field)!);
 return next.toString();
}
export function rememberedLibrary(actor:AuthUser){try{return libraryQuery(actor,new URLSearchParams(sessionStorage.getItem(key(actor))||""))}catch{return ""}}
export function rememberLibrary(actor:AuthUser,params:URLSearchParams){try{sessionStorage.setItem(key(actor),libraryQuery(actor,params))}catch{}}
export function libraryHref(actor:AuthUser,root:string){const query=rememberedLibrary(actor);return query?`${root}?${query}`:root}
