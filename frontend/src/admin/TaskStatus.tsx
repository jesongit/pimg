import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { api } from "../api";
import { filesUpdated } from "./interactions";

export type TaskCounts = { running: number; pending: number; failed: number };
const StatusContext = createContext<{ counts: TaskCounts | null; stale: boolean; refresh: () => void }>({ counts: null, stale: false, refresh: () => {} });
export function TaskStatusProvider({ children, enabled = true }: { children: ReactNode; enabled?: boolean }) {
  const [counts, setCounts] = useState<TaskCounts | null>(null);
  const [stale, setStale] = useState(false);
  const [revision, setRevision] = useState(0);
  const reconcile = useRef(false), previouslyActive=useRef(false);
  const refresh = useCallback(() => {reconcile.current=true;setRevision(value => value + 1)}, []);
  useEffect(() => {
    if (!enabled) return;
    let timer: ReturnType<typeof setTimeout>, controller: AbortController | undefined, stopped = false, sequence=0;
    const load = async () => {
      if (stopped) return;
      const request=++sequence;
      if (!document.hidden) {
        controller = new AbortController();
        try { const next = await api.get<TaskCounts>("/api/admin/tasks/summary", controller.signal); if (!stopped&&request===sequence) { setCounts(next); setStale(false); const active=next.running+next.pending>0; if(reconcile.current||previouslyActive.current||active)filesUpdated();reconcile.current=false;previouslyActive.current=active; } }
        catch(error:any) { if (!stopped&&request===sequence&&error.name!=="AbortError") setStale(true); }
      }
      if (!stopped&&request===sequence) timer = setTimeout(load, 8000);
    };
    void load();
    const visible = () => { if (!document.hidden) { clearTimeout(timer); controller?.abort(); void load(); } };
    document.addEventListener("visibilitychange", visible);
    window.addEventListener("pimg:tasks-updated", refresh);
    return () => { stopped = true; clearTimeout(timer); controller?.abort(); document.removeEventListener("visibilitychange", visible); window.removeEventListener("pimg:tasks-updated", refresh); };
  }, [revision, refresh, enabled]);
  return <StatusContext.Provider value={{ counts, stale, refresh }}>{children}</StatusContext.Provider>;
}
export const useTaskStatus = () => useContext(StatusContext);
export const tasksUpdated = () => window.dispatchEvent(new Event("pimg:tasks-updated"));
export function TaskBadge() {
  const { counts, stale } = useTaskStatus();
  const total = counts ? counts.running + counts.pending : 0;
  return <><span id="task-navigation-status" hidden>{counts ? `${stale ? "任务统计暂时不可用；上次统计：" : ""}当前 ${total}，其中运行中 ${counts.running}、等待 ${counts.pending}` : "任务统计暂时不可用"}</span>{counts && total > 0 && <span className="rail-task-badge" title={`${stale ? "上次统计：" : ""}运行中 ${counts.running}，等待 ${counts.pending}`} aria-hidden="true">
    {total > 99 ? "99+" : total}
  </span>}</>;
}
