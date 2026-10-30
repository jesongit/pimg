/** Copy pixels decoded from the original file at their full resolution. */
export async function copyOriginalImage(url: string, ext: string, size: number, signal: AbortSignal) {
  if (!window.isSecureContext) throw new Error("复制图片需要 HTTPS 或本机地址，请通过安全地址打开后重试。");
  if (!navigator.clipboard?.write || typeof ClipboardItem === "undefined") throw new Error("当前浏览器不支持复制图片，请使用新版 Chrome、Edge 或 Safari。");

  let preparationError: unknown;
  const png = originalPNG(url, ext, size, signal).catch(error => { preparationError = error; throw error; });
  // Some browsers reject permission before consuming the promised image.
  void png.catch(() => {});
  try {
    // Start the clipboard write within the click's user activation, before fetching.
    await navigator.clipboard.write([new ClipboardItem({ "image/png": png })]);
  } catch (error) {
    signal.throwIfAborted();
    if (preparationError) throw preparationError;
    throw new Error(error instanceof DOMException && error.name === "NotAllowedError"
      ? "图片复制未获允许，请允许剪贴板权限后重试。"
      : "图片复制失败，请重试或下载原件。");
  }
}

export async function originalPNG(url: string, ext: string, size: number, signal: AbortSignal): Promise<Blob> {
  const response = await fetch(url, { credentials: "same-origin", signal });
  if (!response.ok) {
    if (response.status === 401) window.dispatchEvent(new Event("pimg:unauthorized"));
    throw new Error("原图读取失败，请重试。");
  }
  const original = await response.blob();
  signal.throwIfAborted();
  if (!original.size || size > 0 && original.size !== size) throw new Error("原图没有完整加载，请重试。");
  const urls = [URL.createObjectURL(original)];
  const image = new Image();
  const canvas = document.createElement("canvas");
  try {
    image.src = urls[0];
    try { await image.decode(); }
    catch {
      signal.throwIfAborted();
      if (!/^(heic|heif)$/i.test(ext)) throw new Error("浏览器无法解码此原图，请下载原件。");
      const { decodeHEIC } = await import("./heic");
      const decoded = await decodeHEIC(urls[0], signal);
      urls.push(decoded); image.src = decoded;
      await image.decode();
    }
    signal.throwIfAborted();
    canvas.width = image.naturalWidth; canvas.height = image.naturalHeight;
    const context = canvas.getContext("2d");
    if (!context || !canvas.width || !canvas.height) throw new Error("浏览器无法复制此原图，请下载原件。");
    context.drawImage(image, 0, 0);
    const png = await new Promise<Blob | null>(resolve => canvas.toBlob(resolve, "image/png"));
    signal.throwIfAborted();
    if (!png) throw new Error("原图过大或无法转换为剪贴板图片，请下载原件。");
    return png;
  } finally {
    image.src = ""; canvas.width = canvas.height = 0;
    urls.forEach(value => URL.revokeObjectURL(value));
  }
}
