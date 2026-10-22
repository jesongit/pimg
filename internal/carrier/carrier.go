// Package carrier 实现"单文件 PNG 导入导出"可选能力：
// 一张显示模糊预览的 PNG，其后附加完整的加密原件（密文分片 + 封装密钥）。
// 它不是主存储格式，导出内容仍受主密钥约束（换主密钥的实例无法导入解密）。
package carrier

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"pimg/internal/filenames"
)

// Magic 载体标识（紧跟 PNG IEND 之后）。
var Magic = []byte("PIMGEXP1")

// Header 附加数据头。
type Header struct {
	V      int            `json:"v"`
	File   CarrierFile    `json:"file"`
	Chunks []CarrierChunk `json:"chunks"`
}

// CarrierFile 文件元数据（与 files 表对应字段子集）。
type CarrierFile struct {
	Name       string             `json:"name"`
	OrigName   string             `json:"orig_name"`
	Ext        string             `json:"ext"`
	Mime       string             `json:"mime"`
	Size       int64              `json:"size"`
	SHA256     string             `json:"sha256"`
	Kind       string             `json:"kind"`
	Width      int                `json:"width"`
	Height     int                `json:"height"`
	Duration   float64            `json:"duration"`
	KeyWrapped []byte             `json:"key_wrapped"`
	KeyNonce   []byte             `json:"key_nonce"`
	CryptoVer  int                `json:"crypto_ver"`
	HasThumb   bool               `json:"has_thumb"`
	HasBThumb  bool               `json:"has_bthumb"`
	Naming     filenames.Metadata `json:"naming,omitzero"`
}

// CarrierChunk 分片描述。
type CarrierChunk struct {
	Kind   string `json:"kind"` // data|thumb|bthumb
	Idx    int    `json:"idx"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// iend PNG 数据块结束标记（IEND 类型 + CRC）。
var iendTail = []byte{0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xAE, 0x42, 0x60, 0x82}

// Writer 流式写出：png（模糊预览）+ 头 + 各分片密文。
type Writer struct {
	w           io.Writer
	wrote       int
	hdrOff      int
	err         error
	wroteHeader bool
}

// NewWriter 写入可见 PNG 部分。
func NewWriter(w io.Writer, png []byte) (*Writer, error) {
	if !bytes.Contains(png, iendTail) {
		return nil, errors.New("预览部分不是合法 PNG")
	}
	if _, err := w.Write(png); err != nil {
		return nil, err
	}
	return &Writer{w: w, wrote: len(png)}, nil
}

// WriteHeader 写入 magic + 头 JSON。
func (cw *Writer) WriteHeader(h *Header) error {
	if cw.err != nil {
		return cw.err
	}
	hb, err := json.Marshal(h)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(Magic)+4+len(hb))
	buf = append(buf, Magic...)
	var l [4]byte
	binary.LittleEndian.PutUint32(l[:], uint32(len(hb)))
	buf = append(buf, l[:]...)
	buf = append(buf, hb...)
	if _, err := cw.w.Write(buf); err != nil {
		cw.err = err
		return err
	}
	cw.wroteHeader = true
	cw.wrote += len(buf)
	return nil
}

// WriteChunk 顺序写入分片密文（调用方保证与 Header.Chunks 顺序一致）。
func (cw *Writer) WriteChunk(p []byte) (int, error) {
	if !cw.wroteHeader {
		return 0, errors.New("必须先写 Header")
	}
	n, err := cw.w.Write(p)
	cw.wrote += n
	return n, err
}

// Written 已写字节数。
func (cw *Writer) Written() int { return cw.wrote }

// Parse 从导出 PNG 文件字节中解析（小文件整体读入；导出用于单文件交换）。
func Parse(data []byte) (png []byte, h *Header, blob []byte, err error) {
	i := bytes.LastIndex(data, iendTail)
	if i < 0 {
		return nil, nil, nil, errors.New("不是 PNG 文件")
	}
	end := i + len(iendTail)
	if end+len(Magic)+4 > len(data) {
		return nil, nil, nil, errors.New("PNG 中没有 pimg 附加数据")
	}
	if !bytes.Equal(data[end:end+len(Magic)], Magic) {
		return nil, nil, nil, errors.New("附加数据标识不匹配（可能不是 pimg 导出文件）")
	}
	off := end + len(Magic)
	hl := binary.LittleEndian.Uint32(data[off : off+4])
	off += 4
	if int(hl) > len(data)-off {
		return nil, nil, nil, errors.New("附加数据头损坏")
	}
	h = &Header{}
	if err := json.Unmarshal(data[off:off+int(hl)], h); err != nil {
		return nil, nil, nil, fmt.Errorf("解析附加数据头: %w", err)
	}
	off += int(hl)
	return data[:end], h, data[off:], nil
}
