import {useEffect,useId} from "react";
import type {KeyboardEvent,ReactNode} from "react";
import {createPortal} from "react-dom";
import {useLibraryPopover} from "./LibraryPopover";

export type FilterOption={value:string;label:string;count?:number;section?:boolean};
const chevron=<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="m8 10 4 4 4-4"/></svg>;
const check=<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="m5 12 4 4 10-10"/></svg>;

export default function FilterSelect({label,value,options,onChange,active=false,disabled=false,title,className="",width=176,fallback,footer,prefix,displayLabel}:{label:string;value:string;options:FilterOption[];onChange:(value:string)=>void;active?:boolean;disabled?:boolean;title?:string;className?:string;width?:number;fallback?:string;footer?:ReactNode;prefix?:string;displayLabel?:string}){
 const popup=useLibraryPopover(width),id=useId();
 const selected=options.find(option=>option.value===value),name=selected?.label||fallback||label;
 useEffect(()=>{popup.setOpen(false)},[value,disabled]);
 useEffect(()=>{if(popup.open){const current=popup.popup.current?.querySelector<HTMLElement>('[aria-selected="true"]');current?.focus({preventScroll:true});current?.scrollIntoView({block:"nearest"})}},[popup.open]);
 const choose=(next:string)=>{popup.close(true);onChange(next)};
 const move=(event:KeyboardEvent<HTMLDivElement>)=>{
  if(!["ArrowDown","ArrowUp","Home","End"].includes(event.key))return;
  event.preventDefault();const rows=Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>('[role="option"]')),index=rows.indexOf(document.activeElement as HTMLButtonElement);
  rows[event.key==="Home"?0:event.key==="End"?rows.length-1:(index+(event.key==="ArrowDown"?1:-1)+rows.length)%rows.length]?.focus();
 };
 return <div className={`filter-select${active?" is-active":""}${className?" "+className:""}`}>
  <button ref={popup.trigger} type="button" className="filter-select-trigger" aria-label={`${label}：${name}`} title={title||name} aria-haspopup="listbox" aria-controls={popup.open?id:undefined} aria-expanded={popup.open} disabled={disabled} onClick={popup.toggle} onKeyDown={event=>{if(["ArrowDown","ArrowUp"].includes(event.key)){event.preventDefault();if(!popup.open)popup.toggle()}}}>{prefix&&<span className="library-filter-prefix" aria-hidden="true">{prefix}</span>}<span className="filter-select-value">{displayLabel??name}</span>{chevron}</button>
  {popup.open&&createPortal(<div ref={popup.popup} className="library-popover filter-select-popover" style={popup.position} onKeyDown={event=>{if(event.key!=="Tab")return;const inFooter=!!(event.target as HTMLElement).closest('.filter-select-footer');if(event.shiftKey&&!inFooter||!event.shiftKey&&(!footer||inFooter))popup.close(true)}}>
   <div id={id} className="filter-select-options" role="listbox" aria-label={label} onKeyDown={move}>{options.map(option=><button key={option.value} type="button" role="option" aria-selected={value===option.value} tabIndex={value===option.value?0:-1} className={option.section?"starts-section":undefined} title={option.label} onClick={()=>choose(option.value)}><span>{option.label}</span>{option.count!==undefined&&<small>{option.count}</small>}<i className="filter-select-check">{value===option.value&&check}</i></button>)}</div>
   {footer&&<div className="filter-select-footer" onClick={()=>popup.close()}>{footer}</div>}
  </div>,document.body)}
 </div>;
}
