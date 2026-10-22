import { IconChevronDown, IconChevronUp, IconLogin2, IconPlus, IconTrash } from "@tabler/icons-react";
import type { FooterLink } from "../api";
import { FooterIcon, footerIcons, githubExample } from "../components/SiteFooter";
import { clientID } from "../clientID";

export default function FooterLinksEditor({ links, onChange }: { links: FooterLink[]; onChange: (links: FooterLink[]) => void }) {
  const update = (id: string, patch: Partial<FooterLink>) => onChange(links.map(link => link.id === id ? { ...link, ...patch } : link));
  const add = (example = false) => onChange([...links, { ...(example ? githubExample : { label: "", icon: "link" as const, url: "" }), id: clientID() }]);
  const move = (index: number, step: number) => { const next = [...links]; [next[index], next[index + step]] = [next[index + step], next[index]]; onChange(next); };
  return <section className="footer-links-editor" aria-labelledby="footer-links-heading">
    <header><div><h3 id="footer-links-heading">页脚图标</h3><p>按下方顺序展示。GitHub 是示例，可修改或删除。</p></div><button type="button" className="btn btn-sm" disabled={links.length >= 12} onClick={() => add()}><IconPlus size={16} stroke={1.7} />添加链接</button></header>
    <div className="footer-link-list">{links.map((item, index) => <div className="footer-link-row" key={item.id}>
      <span className="footer-link-preview" aria-hidden="true"><FooterIcon item={item} /></span>
      <div className="footer-link-fields">
        <label>名称<input className="input" aria-label={`链接 ${index + 1} 名称`} value={item.label} maxLength={60} required placeholder="如：个人网站" onChange={e => update(item.id, { label: e.target.value })} /></label>
        <label>图标<select className="select" aria-label={`链接 ${index + 1} 图标`} value={item.icon} onChange={e => update(item.id, { icon: e.target.value as FooterLink["icon"] })}>{footerIcons.map(([key, name]) => <option key={key} value={key}>{name}</option>)}</select></label>
        <label className="footer-link-address">链接地址<input className="input" aria-label={`链接 ${index + 1} 地址`} value={item.url} maxLength={1024} required placeholder="https://… 或 mailto:…" spellCheck={false} onChange={e => update(item.id, { url: e.target.value })} /></label>
        {item.icon === "custom" && <label className="footer-link-image">图标图片地址<input className="input" aria-label={`链接 ${index + 1} 图标图片地址`} value={item.icon_url || ""} maxLength={1024} required placeholder="https://…（SVG / PNG / WebP）" spellCheck={false} onChange={e => update(item.id, { icon_url: e.target.value })} /></label>}
      </div>
      <div className="footer-link-order"><button type="button" className="btn btn-ghost" aria-label={`上移链接 ${index + 1}`} title="上移" disabled={!index} onClick={() => move(index, -1)}><IconChevronUp size={15} stroke={1.7} /></button><button type="button" className="btn btn-ghost" aria-label={`下移链接 ${index + 1}`} title="下移" disabled={index === links.length - 1} onClick={() => move(index, 1)}><IconChevronDown size={15} stroke={1.7} /></button><button type="button" className="btn btn-ghost footer-link-remove" aria-label={`删除链接 ${index + 1}`} title="删除链接" onClick={() => onChange(links.filter(link => link.id !== item.id))}><IconTrash size={16} stroke={1.7} /></button></div>
    </div>)}</div>
    <div className="footer-fixed-link"><IconLogin2 size={18} stroke={1.7} aria-hidden="true" /><span>管理后台</span><small>固定入口 · 始终在最后</small></div>
    <div className="footer-links-note"><span>内置图标保持统一线条；自定义图片建议使用单色透明图，加载失败时显示链接图标。</span>{!links.some(item => item.icon === "github") && links.length < 12 && <button type="button" className="btn btn-ghost btn-sm" onClick={() => add(true)}>添加 GitHub 示例</button>}</div>
  </section>;
}
