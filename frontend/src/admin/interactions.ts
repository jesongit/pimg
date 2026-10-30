import { useEffect, useRef, useSyncExternalStore } from "react";
import type { RefObject } from "react";
import { useBlocker } from "react-router-dom";
import type { FileItem } from "../api";

// React Router supports one navigation blocker. Aggregate the inline album,
// image inspector and settings drafts so one form cannot mask another.
const dirtyForms = new Set<symbol>();
const dirtyListeners = new Set<() => void>();
const notifyDirty = () => dirtyListeners.forEach(listener => listener());
const subscribeDirty = (listener: () => void) => { dirtyListeners.add(listener); return () => { dirtyListeners.delete(listener); }; };

export function useUnsavedChanges(dirty: boolean) {
  const key = useRef(Symbol("dirty-form"));
  useEffect(() => {
    if (dirty) dirtyForms.add(key.current); else dirtyForms.delete(key.current);
    notifyDirty();
    return () => { dirtyForms.delete(key.current); notifyDirty(); };
  }, [dirty]);
}

export function UnsavedChangesGuard() {
  const dirty = useSyncExternalStore(subscribeDirty, () => dirtyForms.size > 0, () => false);
  const blocker = useBlocker(dirty);
  useEffect(() => {
    if (blocker.state !== "blocked") return;
    if (window.confirm("有未保存的更改，确定离开并放弃更改？")) blocker.proceed();
    else blocker.reset();
  }, [blocker]);
  useEffect(() => {
    if (!dirty) return;
    const beforeUnload = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ""; };
    window.addEventListener("beforeunload", beforeUnload);
    return () => window.removeEventListener("beforeunload", beforeUnload);
  }, [dirty]);
  return null;
}

export function useDialog(ref: RefObject<HTMLElement>, onClose: () => void) {
  const close = useRef(onClose);
  close.current = onClose;
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    const overflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    ref.current?.querySelector<HTMLElement>("button, input, select, [tabindex='0']")?.focus({ preventScroll: true });
    const key = (event: KeyboardEvent) => {
      if (event.key === "Escape") { event.preventDefault(); close.current(); }
      if (event.key !== "Tab") return;
      const nodes = Array.from(ref.current?.querySelectorAll<HTMLElement>("button:not(:disabled),a[href],input:not(:disabled),select:not(:disabled),summary,[tabindex='0']") || []).filter((el) => el.getClientRects().length);
      const first = nodes[0], last = nodes[nodes.length - 1];
      if (event.shiftKey && (document.activeElement === first || !ref.current?.contains(document.activeElement))) { event.preventDefault(); last?.focus(); }
      else if (!event.shiftKey && (document.activeElement === last || !ref.current?.contains(document.activeElement))) { event.preventDefault(); first?.focus(); }
    };
    document.addEventListener("keydown", key);
    return () => { document.body.style.overflow = overflow; document.removeEventListener("keydown", key); previous?.isConnected && previous.focus({ preventScroll: true }); };
  }, [ref]);
}

export function operationBusy(f: FileItem) {
  return f.operation?.state === "pending" || f.operation?.state === "running" || f.conversion?.state === "pending" || f.conversion?.state === "running";
}

export function archiveEncrypted(file: Pick<FileItem, "encrypted" | "storage" | "media_version">) {
	if(file.media_version===2)return false;
  return typeof file.encrypted === "boolean" ? file.encrypted : file.storage !== "direct";
}

export function permanentPublishAllowed(file: FileItem) {
  return !archiveEncrypted(file) && file.permanent_publish_allowed !== false && file.archive_state === "done" && !file.purge_state && !operationBusy(file) && !(file.conversion && file.conversion.state !== "done");
}

export function permanentPublishReason(file: FileItem) {
  if (archiveEncrypted(file)) return "加密原件仅支持临时发布；转为未加密后才能永久发布。";
  if (file.conversion && file.conversion.state !== "done") return "归档转换尚未完成，暂不能永久发布。";
  if (file.archive_state !== "done") return "归档尚未完成，暂不能永久发布。";
  if (file.purge_state) return "正在清理存储，不能永久发布。";
  if (operationBusy(file)) return "有操作正在进行，完成后再永久发布。";
  return "当前原件暂不允许永久发布。";
}

export function fileStatus(f: FileItem) {
  if (f.purge_state) return f.purge_state === "failed" ? "删除未完成" : "删除中";
  if (f.conversion?.kind === "transfer_owner") return ["failed","canceled"].includes(f.conversion.state) ? "归属转移未完成" : f.conversion.state === "done" ? "归属转移完成" : "转移归属中";
  if (f.conversion) return f.conversion.state === "failed" ? "转换未完成" : f.conversion.state === "done" ? "转换完成" : f.conversion.phase === "cleanup" ? "清理旧副本中" : "转换归档中";
  if (operationBusy(f)) return ({ publish: "发布中", unpublish: "撤销中", verify: "校验中" } as Record<string, string>)[f.operation!.type] || "处理中";
  if (f.archive_state === "failed") return "归档失败";
  if (f.archive_state !== "done") return "归档中";
  if (f.published && archiveEncrypted(f)) return "永久发布待撤销";
  return f.published ? f.show_home ? "已发布 · 首页" : "已发布" : "未发布";
}

export async function copyText(text: string) {
  if (navigator.clipboard?.writeText) {
    try { await navigator.clipboard.writeText(text); return; } catch { /* Try the user-initiated text selection below. */ }
  }
  // LAN HTTP does not expose the Clipboard API. Keep the selection inside an
  // open dialog so native dialog focus containment cannot redirect the copy.
  const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const selection = window.getSelection();
  const ranges = selection ? Array.from({ length: selection.rangeCount }, (_, i) => selection.getRangeAt(i).cloneRange()) : [];
  const field = previous instanceof HTMLInputElement || previous instanceof HTMLTextAreaElement ? previous : null;
  const start = field?.selectionStart, end = field?.selectionEnd, direction = field?.selectionDirection;
  const dialogs = Array.from(document.querySelectorAll<HTMLElement>('dialog[open],[role="dialog"][aria-modal="true"]')).filter(dialog => dialog.getClientRects().length);
  const host = previous?.closest('dialog[open],[role="dialog"][aria-modal="true"]') || dialogs[dialogs.length - 1] || document.body;
  const input = document.createElement("textarea");
  input.value = text; input.readOnly = true; input.tabIndex = -1;
  input.setAttribute("aria-hidden", "true");
  Object.assign(input.style, { position: "fixed", top: "0", left: "0", width: "1px", height: "1px", opacity: "0", pointerEvents: "none" });
  host.appendChild(input);
  try {
    input.focus({ preventScroll: true }); input.select(); input.setSelectionRange(0, text.length);
    if (!document.execCommand("copy")) throw new Error();
  } catch { throw new Error("无法自动复制，请选中链接或令牌后手动复制。"); }
  finally {
    input.remove();
    if (previous?.isConnected) previous.focus({ preventScroll: true });
    if (selection) { selection.removeAllRanges(); ranges.forEach(range => selection.addRange(range)); }
    if (field?.isConnected && start != null && end != null) field.setSelectionRange(start, end, direction || undefined);
  }
}

export type LinkFormat = "url" | "md" | "html";
export function fileLink(f: FileItem, format: LinkFormat = "url") {
  const url = new URL(f.url!, window.location.origin).href;
  if (format === "md") return `![${f.name.replace(/[\r\n]/g, " ").replace(/([\\\[\]])/g, "\\$1")}](${url.replace(/\(/g, "%28").replace(/\)/g, "%29")})`;
  if (format === "html") return `<${f.kind === "video" ? "video controls" : "img"} src="${url.replace(/&/g, "&amp;").replace(/"/g, "&quot;")}"${f.kind === "video" ? "" : ` alt="${f.name.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;")}"`}>${f.kind === "video" ? "</video>" : ""}`;
  return url;
}

export function filesUpdated() {
  window.dispatchEvent(new Event("pimg:files-updated"));
  try { localStorage.setItem("pimg:files-updated", `${Date.now()}:${Math.random()}`); } catch { /* Private browsing still has same-tab updates. */ }
}
