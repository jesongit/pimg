import { useId, useLayoutEffect, useRef, useState } from "react";

/** A real image surface for the browser's native Copy Image command on HTTP. */
export default function ManualImageCopy({ image, onClose }: { image: Blob; onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null), title = useId();
  const [source, setSource] = useState("");
  useLayoutEffect(() => {
    const url = URL.createObjectURL(image), element = dialog.current;
    setSource(url); element?.showModal();
    return () => { element?.close(); URL.revokeObjectURL(url); };
  }, [image]);
  const close = () => { dialog.current?.close(); onClose(); };
  return <dialog ref={dialog} className="media-manual-copy" aria-labelledby={title}
    onCancel={event => { event.preventDefault(); close(); }}
    onKeyDown={event => event.stopPropagation()} onKeyUp={event => event.stopPropagation()}>
    <header><h2 id={title}>手动复制原图</h2><button type="button" className="btn" onClick={close}>返回详情</button></header>
    <p>请右键（手机上长按）下方原图，选择“复制图片”，再到 QQ、微信粘贴。当前地址无法自动复制图片；使用 HTTPS 地址可一键复制。</p>
    <img src={source || undefined} alt="完整原图，可右键复制图片" tabIndex={0} draggable={false}/>
  </dialog>;
}
