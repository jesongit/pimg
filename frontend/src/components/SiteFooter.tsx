import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { IconBrandGithub, IconBrandTelegram, IconHeart, IconLink, IconLogin2, IconMail, IconPhoto, IconRss, IconWorld } from "@tabler/icons-react";
import { displaySiteName } from "../api";
import type { FooterLink, SiteInfo } from "../api";
import "./site-footer.css";

export const githubExample: FooterLink = { id: "github", label: "GitHub", icon: "github", url: "https://github.com" };
export const footerIcons = [
  ["github", "GitHub"], ["website", "网站"], ["link", "链接"], ["mail", "邮件"],
  ["rss", "RSS"], ["telegram", "Telegram"], ["heart", "喜欢"], ["photo", "相册"], ["custom", "自定义图片"],
] as const;
const iconComponents = { github: IconBrandGithub, website: IconWorld, link: IconLink, mail: IconMail, rss: IconRss, telegram: IconBrandTelegram, heart: IconHeart, photo: IconPhoto };

export function safeFooterURL(value: string, image = false): string {
  const text = value.trim();
  if (!text || /[\s\\\u0000-\u001f\u007f]/.test(text)) return "";
  if (text.startsWith("/") && !text.startsWith("//")) return text;
  try {
    const url = new URL(text);
    if ((url.protocol === "https:" || url.protocol === "http:") && url.hostname && !url.username && !url.password) return text;
    if (!image && url.protocol === "mailto:" && url.pathname) return text;
  } catch { /* Invalid links are omitted from the public footer. */ }
  return "";
}

export function FooterIcon({ item }: { item: Pick<FooterLink, "icon" | "icon_url"> }) {
  const [failed, setFailed] = useState(false);
  useEffect(() => setFailed(false), [item.icon_url]);
  const source = safeFooterURL(item.icon_url || "", true);
  if (item.icon === "custom" && source && !failed) return <img src={source} alt="" width="18" height="18" referrerPolicy="no-referrer" onError={() => setFailed(true)} />;
  const Icon = iconComponents[item.icon as keyof typeof iconComponents] || IconLink;
  return <Icon size={18} stroke={1.7} aria-hidden="true" />;
}

export default function SiteFooter({ site, preview = false }: { site: Partial<SiteInfo> | null; preview?: boolean }) {
  const links = (site?.footer_links ?? [githubExample]).filter(item => safeFooterURL(item.url));
  const registered = !!(site?.icp_number || site?.police_number);
  return <footer className={`site-footer${preview ? " site-footer-preview" : " page-width"}`}>
    <div className="footer-copy"><div className="footer-signature"><span className="footer-site-name">{displaySiteName(site?.site_name)}</span>{site?.footer_text && <span>{site.footer_text}</span>}</div>
      {registered && <><span className="footer-separator" aria-hidden="true">|</span><div className="footer-registrations">
        {site?.icp_number && <a href={safeFooterURL(site.icp_url || "") || "https://beian.miit.gov.cn"} target="_blank" rel="noopener noreferrer">{site.icp_number}</a>}
        {site?.police_number && <a href={safeFooterURL(site.police_url || "") || "https://beian.mps.gov.cn"} target="_blank" rel="noopener noreferrer">{site.police_number}</a>}
      </div></>}
    </div>
    <nav className="footer-icons" aria-label={preview ? "页脚图标预览" : "页脚链接"}>
      {links.map(item => preview ? <span className="footer-icon-link" title={item.label} key={item.id}><FooterIcon item={item} /></span> : <a className="footer-icon-link" key={item.id} href={safeFooterURL(item.url)} target="_blank" rel="noopener noreferrer" aria-label={item.label} title={item.label}><FooterIcon item={item} /></a>)}
      {preview ? <span className="footer-icon-link" title="管理后台 · 固定入口"><IconLogin2 size={18} stroke={1.7} aria-hidden="true" /></span> : <Link className="footer-icon-link" to="/admin" aria-label="管理后台" title="管理后台"><IconLogin2 size={18} stroke={1.7} aria-hidden="true" /></Link>}
    </nav>
  </footer>;
}
