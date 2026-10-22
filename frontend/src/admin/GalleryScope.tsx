import { createContext, useContext, useMemo } from "react";
import type { ReactNode } from "react";
import { Navigate, useSearchParams } from "react-router-dom";
import { galleryApi } from "../api";
import type { AuthUser } from "../api";

const fallback: AuthUser = { username: "admin", owner_id: "admin:1", role: "admin" };
const defaultApi = galleryApi(fallback);
const GalleryContext = createContext({ actor: fallback, admin: true, owner: fallback.owner_id, readOnly: false, key: "admin:1:admin:1", api: defaultApi, ownApi: defaultApi });

export function GalleryProvider({ actor, children }: { actor: AuthUser; children: ReactNode }) {
  const [params] = useSearchParams();
  const requestedOwner = params.get("owner_id");
  const owner = actor.role === "admin" && requestedOwner !== "all" ? requestedOwner || actor.owner_id : actor.owner_id;
  const ownApi = useMemo(() => galleryApi(actor), [actor.owner_id, actor.role]);
  const api = useMemo(() => galleryApi(actor, owner), [actor.owner_id, actor.role, owner]);
  const value = useMemo(() => ({ actor, owner, admin: actor.role === "admin", readOnly: actor.role !== "admin" && owner !== actor.owner_id, key: `${actor.owner_id}:${owner}`, api, ownApi }), [actor, owner, api, ownApi]);
  if (requestedOwner === "all") {
    const next = new URLSearchParams(params);
    for (const key of ["owner_id", "album_id", "file", "pages", "start_page", "before", "after", "seek"]) next.delete(key);
    return <Navigate to={{ search: next.toString() }} replace />;
  }
  return <GalleryContext.Provider value={value}>{children}</GalleryContext.Provider>;
}

export const useGallery = () => useContext(GalleryContext);
