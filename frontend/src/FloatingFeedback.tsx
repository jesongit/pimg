import { createContext, isValidElement, useCallback, useContext, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { HTMLAttributes, ReactNode } from "react";
import { createPortal } from "react-dom";
import { IconCheck, IconExclamationCircle, IconInfoCircle, IconX } from "@tabler/icons-react";

export const FloatingHostContext = createContext<HTMLElement | null>(null);

function textOf(node: ReactNode): string {
  if (typeof node === "string" || typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map(textOf).join("");
  return isValidElement<{ children?: ReactNode }>(node) ? textOf(node.props.children) : "";
}
function actionable(node: ReactNode): boolean {
  if (Array.isArray(node)) return node.some(actionable);
  return isValidElement<{ children?: ReactNode; onClick?: unknown; href?: string; to?: string }>(node)
    && (!!node.props.onClick || !!node.props.href || !!node.props.to || actionable(node.props.children));
}

/** Declarative feedback keeps its handlers and accessibility IDs, without taking layout space. */
export default function FloatingFeedback({ children, role = "alert", className = "", persistent = false, id }: HTMLAttributes<HTMLDivElement> & { persistent?: boolean }) {
  const host = useContext(FloatingHostContext), anchor = useRef<HTMLSpanElement>(null);
  const message = textOf(children), error = role === "alert" || /is-err|error/.test(className);
  const [allowed, setAllowed] = useState(true), [dismissed, setDismissed] = useState<string | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>();
  const keep = persistent || actionable(children), duration = error ? 6500 : 3200;
  const pause = () => clearTimeout(timer.current);
  const resume = () => { pause(); if (!keep) timer.current = setTimeout(() => setDismissed(message), duration); };
  useLayoutEffect(() => {
    const parent = anchor.current?.parentElement;
    if (!parent) return;
    const measure = () => setAllowed(!!parent.getClientRects().length && !parent.closest('[hidden],[aria-hidden="true"]'));
    const observer = new MutationObserver(measure);
    for (let element: HTMLElement | null = parent; element; element = element.parentElement) observer.observe(element, { attributes: true, attributeFilter: ["hidden", "aria-hidden", "class", "style"] });
    measure(); return () => observer.disconnect();
  }, []);
  useEffect(() => { setDismissed(null); if (allowed && message) resume(); return pause; }, [message, allowed, keep, duration]);
  const Icon = error ? IconExclamationCircle : /is-ok/.test(className) ? IconCheck : IconInfoCircle;
  return <><span ref={anchor} hidden data-feedback-anchor=""/>{host && allowed && message && dismissed !== message && createPortal(
    <div id={id} className={`notification floating-feedback ${error ? "toast-err" : "toast-ok"}`} role={role}
      onMouseEnter={pause} onMouseLeave={resume} onFocusCapture={pause}
      onBlurCapture={event => { if (!event.currentTarget.contains(event.relatedTarget as Node | null)) resume(); }}>
      <Icon className="notification-icon" size={17} aria-hidden="true"/>
      <div className="notification-content floating-feedback-body">{children}</div>
      <button type="button" className="notification-close" aria-label="关闭通知" onClick={() => setDismissed(message)}><IconX size={15} aria-hidden="true"/></button>
    </div>, host)}</>;
}

/** A shared top-layer surface also stays visible above dialogs and native fullscreen. */
export function NotificationLayer({ children, onHost }: { children: ReactNode; onHost: (host: HTMLDivElement | null) => void }) {
  const ref = useRef<HTMLDivElement | null>(null), previousFocus = useRef<HTMLElement | null>(null);
  const attach = useCallback((element: HTMLDivElement | null) => { ref.current = element; if (element?.showPopover) element.setAttribute("popover", "manual"); onHost(element); }, [onHost]);
  const [parent, setParent] = useState<Element>(() => document.fullscreenElement || document.body);
  useEffect(() => {
    const change = () => setParent(document.fullscreenElement || document.body);
    document.addEventListener("fullscreenchange", change);
    const focus = (event: KeyboardEvent) => {
      if (event.key !== "F8") return;
      const button = ref.current?.querySelector<HTMLElement>('button,a[href]');
      if (button) { event.preventDefault(); previousFocus.current = document.activeElement as HTMLElement; button.focus({ preventScroll: true }); }
    };
    document.addEventListener("keydown", focus, true);
    return () => { document.removeEventListener("fullscreenchange", change); document.removeEventListener("keydown", focus, true); };
  }, []);
  useLayoutEffect(() => {
    const element = ref.current;
    if (!element) return;
    const update = () => {
      if (!element.isConnected || !element.showPopover) return;
      try {
        if (!element.querySelector(".notification")) { element.hidePopover(); return; }
        if (element.matches(":popover-open") && element.contains(document.activeElement)) return;
        element.hidePopover(); element.showPopover();
      } catch { /* The host may be moving between fullscreen and the document. */ }
    };
    // Portals from page components have different React ancestors. Capture
    // keyboard navigation on the shared DOM host before a dialog's focus trap.
    const key = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault(); event.stopPropagation();
        (event.target as Element).closest(".notification")?.querySelector<HTMLButtonElement>(".notification-close")?.click();
        const dialogs = Array.from(document.querySelectorAll<HTMLElement>('[role="dialog"][aria-modal="true"]'));
        const dialog = dialogs[dialogs.length - 1], previous = previousFocus.current;
        const target = previous?.isConnected && (!dialog || dialog.contains(previous)) ? previous : Array.from(dialog?.querySelectorAll<HTMLElement>('button:not(:disabled),a[href],input:not(:disabled)') || []).find(node => node.getClientRects().length);
        target?.focus({ preventScroll: true });
      } else if (event.key === "Tab") {
        event.preventDefault(); event.stopPropagation();
        const nodes = Array.from(element.querySelectorAll<HTMLElement>('button:not(:disabled),a[href]')).filter(node => node.getClientRects().length);
        const index = nodes.indexOf(document.activeElement as HTMLElement);
        nodes[(index + (event.shiftKey ? -1 : 1) + nodes.length) % nodes.length]?.focus();
      }
    };
    const observer = new MutationObserver(update); observer.observe(element, { childList: true, subtree: true }); update();
    element.addEventListener("keydown", key, true);
    return () => { observer.disconnect(); element.removeEventListener("keydown", key, true); };
  }, [parent]);
  return createPortal(<div ref={attach} className="toasts notification-stack" aria-label="通知" aria-keyshortcuts="F8">{children}</div>, parent);
}
