import type {FileItem} from "../api";
import "./file-encryption.css";

// This is the file's upload/import choice. Actual replica encryption is shown
// separately by StorageLabels and must not determine this badge.
export function fileEncryptionSetting(file: FileItem) {
  const encrypted = typeof file.encryption_requested === "boolean" ? file.encryption_requested
    : file.media_version === 2 ? null
    : typeof file.encrypted === "boolean" ? file.encrypted
    : file.storage === "direct" ? false : file.storage === "tg" ? true : null;
  return {encrypted, state: encrypted === null ? "unknown" : encrypted ? "encrypted" : "plaintext", label: encrypted === null ? "未知" : encrypted ? "加密" : "未加密"};
}

export default function FileEncryption({file}: {file: FileItem}) {
  const {encrypted, state, label} = fileEncryptionSetting(file);
  return <span className="file-encryption" data-encryption-setting={state} role="img" aria-label={`文件加密设置：${label}`}>
    <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {encrypted === null ? <><circle cx="12" cy="12" r="8" /><path d="M12 8v4m0 4h.01" /></> : <><rect x="5" y="10" width="14" height="11" rx="2" />{encrypted ? <path d="M8 10V6a4 4 0 0 1 8 0v4" /> : <path d="M9 10V6a4 4 0 0 1 7.5-2" />}</>}
    </svg>
  </span>;
}
