import "./select-all.css";

export default function SelectAllControl({ checked, indeterminate, disabled, busy, scope, className = "", onChange }: {
  checked: boolean; indeterminate?: boolean; disabled?: boolean; busy?: boolean;
  scope: string; className?: string; onChange: () => void;
}) {
  return <button type="button" className={`select-all-control ${className}`} role="checkbox"
    aria-checked={checked ? true : indeterminate ? "mixed" : false} aria-label={scope}
    aria-busy={busy || undefined} title="全选" disabled={disabled} onClick={onChange}>
    <svg width="17" height="17" viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <circle cx="10" cy="10" r="7.5"/><path d={indeterminate && !checked ? "M6 10h8" : "m6 10 3 3 5-6"}/>
    </svg><span>全选</span>
  </button>;
}
