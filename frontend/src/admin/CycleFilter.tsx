import type { FilterOption } from "./FilterSelect";
import "./cycle-filter.css";

const cycleIcon = <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M4 7h13l-3-3m6 13H7l3 3M17 7l-3 3M7 17l3-3" /></svg>;

export default function CycleFilter({ label, value, options, onChange, active = false, disabled = false, title, prefix, displayLabels }: {
  label: string;
  value: string;
  options: FilterOption[];
  onChange: (value: string) => void;
  active?: boolean;
  disabled?: boolean;
  title?: string;
  prefix?: string;
  displayLabels?: Record<string, string>;
}) {
  const index = options.findIndex(option => option.value === value);
  const selected = options[index] || options[0];
  const next = options[(index + 1 + options.length) % options.length];
  const hint = disabled ? title || `${label}当前不可用` : `${title ? `${title}；` : ""}点击切换为${next?.label || label}`;
  return <button type="button" className={`cycle-filter${active ? " is-active" : ""}`} disabled={disabled || options.length < 2} title={hint} aria-label={`${label}：${selected?.label || label}；${hint}`} onClick={() => { if (next) onChange(next.value); }}>
    {prefix&&<span className="library-filter-prefix" aria-hidden="true">{prefix}</span>}<span className="cycle-filter-label">
      {options.map(option => <span key={option.value} className="cycle-filter-measure" aria-hidden="true">{displayLabels?.[option.value]??option.label}</span>)}
      <span className="cycle-filter-current">{displayLabels?.[selected?.value]??selected?.label??label}</span>
    </span>{cycleIcon}
  </button>;
}
