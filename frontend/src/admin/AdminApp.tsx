import { useContext, useEffect, useRef, useState } from "react";
import { Link, NavLink, Route, Routes, useLocation, useNavigate, Navigate, useParams, useSearchParams } from "react-router-dom";
import { api, displaySiteName, setStorageCatalog, setApiSession } from "../api";
import type { AuthUser } from "../api";
import { SiteCtx } from "../App";
import Library from "./Library";
import Albums from "./Albums";

import Logs from "./Logs";
import Tasks from "./Tasks";
import { TaskBadge, TaskStatusProvider } from "./TaskStatus";
import Users from "./Users";
import StoragePage from "./StoragePage";
import Settings from "./Settings";
import AdminCredentials from "./AdminCredentials";
import TwoFactor from "./TwoFactor";
import "./admin.css";
import "./album-layout.css";
import "./workspace.css";
import "./library-revision.css";
import "./library-navigation.css";
import "./gallery-scope.css";
import "./gallery-layout.css";
import "./task-navigation.css";
import "./filter-select.css";
import "./logs.css";
import { UploadProvider } from "./Uploads";
import { GalleryProvider, useGallery } from "./GalleryScope";
import {libraryHref,rememberedLibrary} from "./libraryMemory";

export default function AdminApp() {
  const nav = useNavigate(), location = useLocation();
  const [, setCatalogVersion] = useState(0);
  const [me, setMe] = useState<AuthUser | null>(null);
  const authenticatedOwner = useRef<string>();
  const site = useContext(SiteCtx);
  const siteName = displaySiteName(site?.site_name);
  const personalRoute = location.pathname.startsWith("/library");
  const admin = me?.role === "admin", root = admin ? "/admin" : "/library";

  useEffect(() => {
    if (me?.role !== "admin" || me.setup_required) return;
    const catalog = () => {void api.get<{instances:{id:string;name:string}[]}>("/api/admin/storage").then(r=>{setStorageCatalog(r.instances);setCatalogVersion(v=>v+1)}).catch(()=>{});};catalog();window.addEventListener("pimg:policy-updated",catalog);return()=>window.removeEventListener("pimg:policy-updated",catalog);
  },[me?.role, me?.setup_required]);
  useEffect(() => {
    let active = true;
    const check = () => api.get<AuthUser>("/api/auth/me").then(user => { if (!active) return; setApiSession(`${user.owner_id}:${user.role}:${!!user.setup_required}`,user.cache_scope); if(authenticatedOwner.current && authenticatedOwner.current!==user.owner_id)window.dispatchEvent(new Event("pimg:auth-changed"));authenticatedOwner.current=user.owner_id;setMe(user); if (user.role === "user" && !personalRoute) nav("/library", {replace:true}); if (user.role === "admin" && personalRoute) nav("/admin",{replace:true}); }).catch(() => {if(active){setMe(null);nav("/login",{replace:true});}});
    void check(); window.addEventListener("focus",check); window.addEventListener("pimg:account-updated", check);
    const onUnauth = () => {setMe(null);nav("/login",{replace:true});};
    window.addEventListener("pimg:unauthorized", onUnauth);
    return () => {active=false;window.removeEventListener("focus",check);window.removeEventListener("pimg:account-updated",check);window.removeEventListener("pimg:unauthorized", onUnauth);};
  }, [nav, personalRoute]);

  const logout = async () => {
    await api.post("/api/auth/logout").catch(() => {});
    window.dispatchEvent(new Event("pimg:auth-changed"));
    nav("/login", { replace: true });
  };

  if (!me) return <div className="auth-pending" role="status">正在验证账号…</div>;
  if (admin && me.setup_required) return <div className="admin">
    <header className="admin-topbar"><div className="admin-topbar-inner"><Link className="brand" to="/">{siteName}</Link><div className="admin-utilities"><span className="account-name">{me.username}</span><button className="btn btn-ghost account-logout" onClick={logout}>退出登录</button></div></div></header>
    <main className="main"><div className="admin-setup settings-page settings-workbench"><section className="settings-section" aria-labelledby="admin-setup-title"><div className="settings-section-heading"><h1 id="admin-setup-title">设置管理员账号</h1><p>首次登录请设置新密码，用户名可以一起修改。</p></div><AdminCredentials username={me.username} setup /></section></div></main>
  </div>;
  return (
    <GalleryProvider actor={me}><UploadProvider key={me.owner_id}><TaskStatusProvider enabled={admin}><div className="admin">
      <header className="admin-topbar"><div className="admin-topbar-inner">
        <Link className="brand" to="/" aria-label={`${siteName}，返回首页`} title="返回首页">{site?.logo_url && <img className="brand-logo" src={site.logo_url} alt="" onError={event => { event.currentTarget.hidden = true; }} />}{siteName}</Link>
        <nav className="admin-nav" aria-label="后台导航">
          <NavLink to={libraryHref(me,root)} end className="rail-link">图库</NavLink>
          <NavLink to={`${root}/albums`} className="rail-link">相册</NavLink>
          {admin && <>
          <NavLink to="/admin/users" className="rail-link">用户</NavLink>
          <NavLink to="/admin/storage" className="rail-link">存储</NavLink>
          <NavLink to="/admin/tasks" className="rail-link" aria-describedby="task-navigation-status"><span className="task-nav-label">任务<TaskBadge /></span></NavLink>
          <NavLink to="/admin/logs" className="rail-link">日志</NavLink>
          <NavLink to="/admin/settings" className="rail-link">设置</NavLink>
          </>}
          {!admin && <NavLink to="/library/security" className="rail-link">账号安全</NavLink>}
        </nav>
        <div className="admin-utilities"><span className="account-name" title={me.username}>{me.username}</span><button type="button" className="btn btn-ghost account-logout" onClick={logout}>退出登录</button></div>
      </div></header>
      <main className="main">
        <Routes>
          <Route index element={<ScopedLibrary />} />
          <Route path="albums" element={<Albums />} />
          {!admin && <Route path="security" element={<div className="account-security-page"><h1>账号安全</h1><TwoFactor /></div>} />}
          {admin && <>
          <Route path="albums/:albumId" element={<LegacyAlbum />} />
          <Route path="tasks" element={<TaskRoute />} />
          <Route path="logs" element={<LogRoute />} />
          <Route path="tokens" element={<Navigate to="/admin/settings#settings-tokens" replace />} />
          <Route path="users" element={<Users />} />
          <Route path="storage" element={<StoragePage />} />
          <Route path="settings" element={<Settings username={me.username} />} />
          </>}
          <Route path="*" element={<Navigate to={root} replace />} />
        </Routes>
      </main>
    </div></TaskStatusProvider></UploadProvider></GalleryProvider>
  );
}

function ScopedLibrary() {
 const scope=useGallery(),[params]=useSearchParams();
 const restore=useRef(!params.toString()?rememberedLibrary(scope.actor):"");
 if(params.toString())restore.current="";
 if(restore.current)return <Navigate to={{search:restore.current}} replace/>;
 return <Library key={scope.key}/>;
}

function LegacyAlbum(){const {albumId}=useParams();return <Navigate to={`/admin?album_id=${encodeURIComponent(albumId||"")}`} replace/>}

function TaskRoute(){const location=useLocation(),params=new URLSearchParams(location.search);return params.get("record")==="receipts"||params.get("view")==="receipts"?<Navigate to={`/admin/logs${location.search}`} replace/>:<Tasks/>}
function LogRoute(){const location=useLocation(),params=new URLSearchParams(location.search);if(params.get("view")==="current")return <Navigate to="/admin/tasks" replace/>;return <Logs/>}
