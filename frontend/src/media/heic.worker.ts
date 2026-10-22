import libheif from "libheif-js/libheif-wasm/libheif-bundle.mjs";

self.onmessage = async (event: MessageEvent<ArrayBuffer>) => {
  try {
    const module = await (typeof libheif === "function" ? libheif() : libheif);
    const decoder = new module.HeifDecoder();
    const images = decoder.decode(new Uint8Array(event.data));
    const image = images.find((item: any) => item.is_primary?.()) || images[0];
    if (!image) throw new Error("HEIC 中没有可显示的图片");
    const width = image.get_width(), height = image.get_height();
    if (width <= 0 || height <= 0 || width * height > 24_000_000) throw new Error("改用服务端解码高分辨率图片");
    const pixels = { width, height, data: new Uint8ClampedArray(width * height * 4) };
    await new Promise<void>((resolve, reject) => image.display(pixels, (result: unknown) => result ? resolve() : reject(new Error("HEIC 解码失败"))));
    (self as any).postMessage(pixels, [pixels.data.buffer]);
    for (const item of images) item.free?.();
  } catch (error) {
    self.postMessage({ error: error instanceof Error ? error.message : "HEIC 解码失败" });
  }
};
