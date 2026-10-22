package carrier

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var ErrNotPNG = errors.New("归档载体缺少 PNG 签名")

// PartHeader describes one independently transported archive part. The visible
// PNG is always blurred; all clear thumbnails remain inside the ciphertext.
type PartHeader struct {
	Version int         `json:"v"`
	FileID  string      `json:"file_id"`
	File    CarrierFile `json:"file"`
	Kind    string      `json:"kind"`
	Index   int         `json:"index"`
	Size    int64       `json:"size"`
	SHA256  string      `json:"sha256"`
}

var partMagic = []byte("PIMGPART2")

// pngEnd walks PNG chunks, rather than searching ciphertext for an IEND byte
// sequence which may itself occur in the encrypted payload.
func pngEnd(data []byte) (int, error) {
	if len(data) < 8 || !bytes.Equal(data[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return 0, errors.New("载体缺少 PNG 签名")
	}
	for off := 8; off+12 <= len(data); {
		length := int64(binary.BigEndian.Uint32(data[off : off+4]))
		end := int64(off) + 12 + length
		if end > int64(len(data)) {
			return 0, errors.New("PNG 数据块截断")
		}
		if string(data[off+4:off+8]) == "IEND" {
			if length != 0 {
				return 0, errors.New("PNG IEND 损坏")
			}
			return int(end), nil
		}
		off = int(end)
	}
	return 0, errors.New("PNG 缺少 IEND")
}

func EncodePart(preview []byte, h PartHeader, payload []byte) ([]byte, error) {
	sum := sha256.Sum256(payload)
	prefix, err := PartPrefix(preview, h, int64(len(payload)), hex.EncodeToString(sum[:]))
	if err != nil {
		return nil, err
	}
	return append(prefix, payload...), nil
}

// PartPrefix permits uploading a PNG plus ciphertext as a single streamed file.
func PartPrefix(preview []byte, h PartHeader, payloadSize int64, payloadSHA string) ([]byte, error) {
	end, err := pngEnd(preview)
	if err != nil {
		return nil, err
	}
	h.Version = 2
	h.Size = payloadSize
	h.SHA256 = payloadSHA
	if h.FileID == "" || h.Index < 0 || (h.Kind != "data" && h.Kind != "thumb" && h.Kind != "bthumb") {
		return nil, errors.New("无效的载体分片信息")
	}
	header, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if len(header) > 1<<20 || payloadSize < 0 || len(payloadSHA) != 64 {
		return nil, errors.New("载体分片大小或头信息无效")
	}
	out.Grow(end + len(partMagic) + 4 + len(header))
	out.Write(preview[:end])
	out.Write(partMagic)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(header)))
	out.Write(size[:])
	out.Write(header)
	return out.Bytes(), nil
}

// OpenPart consumes only the bounded PNG preview/header, leaving the original
// stream at the ciphertext. The caller verifies the streamed payload hash.
func OpenPart(r io.Reader) (PartHeader, error) {
	var h PartHeader
	var signature [8]byte
	if _, err := io.ReadFull(r, signature[:]); err != nil {
		return h, fmt.Errorf("读取归档载体签名: %w", err)
	}
	if !bytes.Equal(signature[:], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return h, ErrNotPNG
	}
	var total int64
	for {
		var block [8]byte
		if _, err := io.ReadFull(r, block[:]); err != nil {
			return h, err
		}
		n := int64(binary.BigEndian.Uint32(block[:4]))
		total += n + 12
		if total > 4<<20 {
			return h, errors.New("载体预览超出大小限制")
		}
		if _, err := io.CopyN(io.Discard, r, n+4); err != nil {
			return h, err
		}
		if string(block[4:]) == "IEND" {
			if n != 0 {
				return h, errors.New("PNG IEND 损坏")
			}
			break
		}
	}
	magic := make([]byte, len(partMagic)+4)
	if _, err := io.ReadFull(r, magic); err != nil || !bytes.Equal(magic[:len(partMagic)], partMagic) {
		return h, errors.New("不是 Pimg 归档 PNG")
	}
	n := binary.LittleEndian.Uint32(magic[len(partMagic):])
	if n == 0 || n > 1<<20 {
		return h, errors.New("载体头长度无效")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return h, err
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return h, err
	}
	if h.Version != 2 || h.FileID == "" || h.Index < 0 || h.Size < 0 || len(h.SHA256) != 64 || (h.Kind != "data" && h.Kind != "thumb" && h.Kind != "bthumb") {
		return h, errors.New("载体头信息无效")
	}
	return h, nil
}

func DecodePart(data []byte) (preview []byte, h PartHeader, payload []byte, err error) {
	end, err := pngEnd(data)
	if err != nil {
		return nil, h, nil, err
	}
	if len(data) < end+len(partMagic)+4 || !bytes.Equal(data[end:end+len(partMagic)], partMagic) {
		return nil, h, nil, errors.New("不是 Pimg 归档分片 PNG")
	}
	off := end + len(partMagic)
	size := int64(binary.LittleEndian.Uint32(data[off : off+4]))
	off += 4
	if size <= 0 || size > 1<<20 || size > int64(len(data)-off) {
		return nil, h, nil, errors.New("载体头长度无效")
	}
	if err = json.Unmarshal(data[off:off+int(size)], &h); err != nil {
		return nil, h, nil, err
	}
	off += int(size)
	payload = data[off:]
	sum := sha256.Sum256(payload)
	if h.Version != 2 || h.FileID == "" || h.Index < 0 || h.Size != int64(len(payload)) || h.SHA256 != hex.EncodeToString(sum[:]) {
		return nil, h, nil, fmt.Errorf("载体分片校验失败")
	}
	if h.Kind != "data" && h.Kind != "thumb" && h.Kind != "bthumb" {
		return nil, h, nil, errors.New("载体分片类型无效")
	}
	return data[:end], h, payload, nil
}
