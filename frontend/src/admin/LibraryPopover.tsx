import { useEffect, useRef, useState } from "react";

// Non-modal, anchored controls stay outside the scrolling photo canvas.
export function useLibraryPopover(width: number, align: "left" | "right" = "left") {
  const [open, setOpen] = useState(false), [position, setPosition] = useState({ top: 0, left: 0, width, maxHeight: 420 });
  const trigger = useRef<HTMLButtonElement>(null), popup = useRef<HTMLDivElement>(null);
  const close = (restore = false) => { setOpen(false); if (restore) trigger.current?.focus({ preventScroll: true }); };
  const toggle = () => {
    if (open) { close(); return; }
    const rect = trigger.current?.getBoundingClientRect(); if (!rect) return;
    const available = Math.min(width, window.innerWidth - 24), top = Math.min(rect.bottom + 7, Math.max(12, window.innerHeight - 210));
    setPosition({ top, left: Math.max(12, Math.min(align === "right" ? rect.right - available : rect.left, window.innerWidth - available - 12)), width: available, maxHeight: Math.min(460, window.innerHeight - top - 12) }); setOpen(true);
  };
  useEffect(() => {
    if (!open) return;
    const outside = (event: PointerEvent | FocusEvent) => { if (!popup.current?.contains(event.target as Node) && !trigger.current?.contains(event.target as Node)) setOpen(false); };
    const key = (event: KeyboardEvent) => { if (event.key === "Escape") { event.preventDefault(); setOpen(false); trigger.current?.focus({ preventScroll: true }); } };
    const scroll = (event: Event) => { if (!popup.current?.contains(event.target as Node)) setOpen(false); };
    const resize = () => setOpen(false);
    popup.current?.querySelector<HTMLElement>("input,button,select")?.focus({ preventScroll: true });
    document.addEventListener("pointerdown", outside); document.addEventListener("focusin", outside); document.addEventListener("keydown", key); window.addEventListener("scroll", scroll, true); window.addEventListener("resize", resize);
    return () => { document.removeEventListener("pointerdown", outside); document.removeEventListener("focusin", outside); document.removeEventListener("keydown", key); window.removeEventListener("scroll", scroll, true); window.removeEventListener("resize", resize); };
  }, [open]);
  return { open, setOpen, trigger, popup, position, close, toggle };
}
