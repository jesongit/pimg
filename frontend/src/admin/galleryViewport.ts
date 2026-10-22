// Use the rendered toolbar height: narrow screens wrap filters onto more rows.
export function galleryViewportTop() {
  return Math.max(0,
    document.querySelector(".admin-topbar")?.getBoundingClientRect().bottom || 0,
    document.querySelector(".library-tools-surface")?.getBoundingClientRect().bottom || 0);
}
