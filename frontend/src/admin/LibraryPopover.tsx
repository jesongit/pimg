import { useEffect, useRef, useState } from "react";

// Non-modal, anchored controls stay outside the scrolling photo canvas.
export function useLibraryPopover(width: number, align: "left" | "right" = "left", options?: { placement?: "below" | "left"; autoFocus?: boolean }) {
  const [open, setOpen] = useState(false), [position, setPosition] = useState({ top: 0, left: 0, width, maxHeight: 420 });
  const trigger = useRef<HTMLButtonElement>(null), popup = useRef<HTMLDivElement>(null);
  const autoFocus = options?.autoFocus !== false;
  const close = (restore = false) => { setOpen(false); if (restore) trigger.current?.focus({ preventScroll: true }); };
  const toggle = () => {
    if (open) { close(); return; }
    const rect = trigger.current?.getBoundingClientRect(); if (!rect) return;
    const available = Math.min(width, window.innerWidth - 24);
    let top: number, left: number;
    if (options?.placement === "left") {
      // Open beside the trigger on the left so it never covers the parent list.
      left = rect.left - available - 6;
      if (left < 12) left = Math.min(rect.right + 6, window.innerWidth - available - 12);
      top = Math.max(12, Math.min(rect.top, window.innerHeight - 210));
    } else {
      top = Math.min(rect.bottom + 7, Math.max(12, window.innerHeight - 210));
      left = Math.max(12, Math.min(align === "right" ? rect.right - available : rect.left, window.innerWidth - available - 12));
    }
    setPosition({ top, left, width: available, maxHeight: Math.min(460, window.innerHeight - top - 12) }); setOpen(true);
  };
  useEffect(() => {
    if (!open) return;
    // Interacting with the bookmark bar, any bookmark menu (including nested
    // submenus) or pressing a draggable item must not dismiss this popover;
    // otherwise a drag would remove its drop target.
    const keepOpen = (target: EventTarget | null) => {
      const el = target as HTMLElement | null;
      return !!el?.closest?.(".file-browser-bookmarks-bar, .file-bookmark-menu");
    };
    const outside = (event: PointerEvent | FocusEvent) => {
      const target = event.target as HTMLElement | null;
      if (keepOpen(target)) return;
      if (event.type === "pointerdown" && target?.closest?.('[draggable="true"]')) return;
      if (!popup.current?.contains(event.target as Node) && !trigger.current?.contains(event.target as Node)) setOpen(false);
    };
    const key = (event: KeyboardEvent) => { if (event.key === "Escape") { event.preventDefault(); setOpen(false); trigger.current?.focus({ preventScroll: true }); } };
    const scroll = (event: Event) => { if (keepOpen(event.target)) return; if (!popup.current?.contains(event.target as Node)) setOpen(false); };
    const resize = () => setOpen(false);
    if (autoFocus) popup.current?.querySelector<HTMLElement>("input,button,select")?.focus({ preventScroll: true });
    document.addEventListener("pointerdown", outside); document.addEventListener("focusin", outside); document.addEventListener("keydown", key); window.addEventListener("scroll", scroll, true); window.addEventListener("resize", resize);
    return () => { document.removeEventListener("pointerdown", outside); document.removeEventListener("focusin", outside); document.removeEventListener("keydown", key); window.removeEventListener("scroll", scroll, true); window.removeEventListener("resize", resize); };
  }, [open]);
  return { open, setOpen, trigger, popup, position, close, toggle };
}
