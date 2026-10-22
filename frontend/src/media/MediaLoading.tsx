export default function MediaLoading({label,percent}:{label:string;percent?:number}){
  const known=typeof percent==="number",value=known?Math.max(0,Math.min(100,percent)):0;
  return <div className="media-load-status compact-media-load" role="status" aria-live="polite"><svg className={`media-load-ring${known?"":" is-indeterminate"}`} viewBox="0 0 28 28" role="progressbar" aria-label={label} aria-valuemin={known?0:undefined} aria-valuemax={known?100:undefined} aria-valuenow={known?Math.round(value):undefined}><circle className="media-load-ring-track" cx="14" cy="14" r="10"/><circle className="media-load-ring-value" cx="14" cy="14" r="10" strokeDasharray={known?`${value/100*62.832} 62.832`:undefined}/></svg><span>{label}{known?` · ${Math.round(value)}%`:"…"}</span></div>;
}
