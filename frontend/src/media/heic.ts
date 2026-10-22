export async function decodeHEIC(url: string, signal: AbortSignal): Promise<string> {
  const response = await fetch(url, { signal });
  if (!response.ok) throw new Error("原图读取失败");
  const buffer = await response.arrayBuffer();
  if (signal.aborted) throw new DOMException("已取消", "AbortError");
  const worker = new Worker(new URL("./heic.worker.ts", import.meta.url), { type: "module" });
  return new Promise<string>((resolve, reject) => {
    const stop = () => { clearTimeout(timer); signal.removeEventListener("abort", aborted); worker.terminate(); };
    const aborted = () => { stop(); reject(new DOMException("已取消", "AbortError")); };
    const timer = setTimeout(() => { stop(); reject(new Error("改用服务端兼容查看")); }, 12000);
    signal.addEventListener("abort", aborted, { once: true });
    worker.onerror = () => { stop(); reject(new Error("浏览器解码失败")); };
    worker.onmessage = async event => {
      if (event.data.error) { stop(); reject(new Error(event.data.error)); return; }
      const { width, height, data } = event.data;
      const canvas = document.createElement("canvas"); canvas.width = width; canvas.height = height;
      const context = canvas.getContext("2d");
      if (!context) { stop(); reject(new Error("浏览器无法显示原图")); return; }
      context.putImageData(new ImageData(data, width, height), 0, 0);
      canvas.toBlob(blob => { stop(); if (blob && !signal.aborted) resolve(URL.createObjectURL(blob)); else reject(new Error("图片显示已取消")); }, "image/png");
    };
    worker.postMessage(buffer, [buffer]);
  });
}
