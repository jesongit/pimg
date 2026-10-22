package mediax

import (
	"encoding/binary"
	"os"
)

const maxExifBytes = 2 << 20

type metadataBox struct {
	kind   string
	data   []byte
	offset int
}

// Only metadata boxes are read. Large mdat image payloads are skipped, including
// when meta follows the payload. Offsets and allocations are bounded by file size.
func heifEXIF(f *os.File, size int64) []byte {
	for off, count := int64(0), 0; off < size && count < 4096; count++ {
		var h [16]byte
		if size-off < 8 {
			return nil
		}
		if _, err := f.ReadAt(h[:8], off); err != nil {
			return nil
		}
		n, header := int64(binary.BigEndian.Uint32(h[:4])), int64(8)
		if n == 1 {
			if size-off < 16 {
				return nil
			}
			if _, err := f.ReadAt(h[8:], off+8); err != nil {
				return nil
			}
			u := binary.BigEndian.Uint64(h[8:])
			if u > uint64(size-off) {
				return nil
			}
			n, header = int64(u), 16
		} else if n == 0 {
			n = size - off
		}
		if n < header || n > size-off {
			return nil
		}
		if string(h[4:8]) == "meta" {
			if n-header < 4 || n-header > maxExifBytes {
				return nil
			}
			data := make([]byte, int(n-header))
			if _, err := f.ReadAt(data, off+header); err != nil {
				return nil
			}
			return heifExifItem(f, size, data)
		}
		off += n
	}
	return nil
}

func metadataBoxes(data []byte, start int) []metadataBox {
	var result []metadataBox
	for off, count := start, 0; off < len(data) && count < 4096; count++ {
		if len(data)-off < 8 {
			return nil
		}
		n, header := uint64(binary.BigEndian.Uint32(data[off:])), 8
		if n == 1 {
			if len(data)-off < 16 {
				return nil
			}
			n, header = binary.BigEndian.Uint64(data[off+8:]), 16
		} else if n == 0 {
			n = uint64(len(data) - off)
		}
		if n < uint64(header) || n > uint64(len(data)-off) {
			return nil
		}
		end := off + int(n)
		result = append(result, metadataBox{string(data[off+4 : off+8]), data[off+header : end], off + header})
		off = end
	}
	return result
}

type metadataCursor struct {
	data []byte
	off  int
	bad  bool
}

func (c *metadataCursor) number(n int) uint64 {
	if c.bad || n < 0 || n > 8 || c.off+n > len(c.data) {
		c.bad = true
		return 0
	}
	var value uint64
	for _, b := range c.data[c.off : c.off+n] {
		value = value<<8 | uint64(b)
	}
	c.off += n
	return value
}

func heifExifItem(f *os.File, size int64, meta []byte) []byte {
	if len(meta) < 4 || meta[0] != 0 {
		return nil
	}
	var iloc, idat []byte
	exifIDs := map[uint64]bool{}
	for _, box := range metadataBoxes(meta, 4) {
		switch box.kind {
		case "iloc":
			iloc = box.data
		case "idat":
			idat = box.data
		case "iinf":
			if len(box.data) < 6 {
				return nil
			}
			start := 6
			if box.data[0] > 0 {
				start = 8
			}
			for _, entry := range metadataBoxes(box.data, start) {
				if entry.kind != "infe" || len(entry.data) < 12 {
					continue
				}
				c := metadataCursor{data: entry.data, off: 4}
				version := entry.data[0]
				idBytes := 2
				if version == 3 {
					idBytes = 4
				}
				id := c.number(idBytes)
				c.number(2)
				if (version == 2 || version == 3) && c.number(4) == 0x45786966 && !c.bad {
					exifIDs[id] = true
				}
			}
		}
	}
	if len(exifIDs) == 0 || len(iloc) < 8 || iloc[0] > 2 {
		return nil
	}
	version := iloc[0]
	c := metadataCursor{data: iloc, off: 4}
	sizes := c.number(2)
	offsetSize, lengthSize, baseSize, indexSize := int(sizes>>12&15), int(sizes>>8&15), int(sizes>>4&15), 0
	if version > 0 {
		indexSize = int(sizes & 15)
	}
	if offsetSize > 8 || lengthSize > 8 || baseSize > 8 || indexSize > 8 {
		return nil
	}
	idSize := 2
	if version == 2 {
		idSize = 4
	}
	count := c.number(idSize)
	if count > 4096 {
		return nil
	}
	for i := uint64(0); i < count; i++ {
		id := c.number(idSize)
		method := uint64(0)
		if version > 0 {
			method = c.number(2) & 15
		}
		reference := c.number(2)
		base := c.number(baseSize)
		extents := c.number(2)
		if c.bad || extents > 4096 {
			return nil
		}
		var result []byte
		for j := uint64(0); j < extents; j++ {
			c.number(indexSize)
			offset, length := c.number(offsetSize), c.number(lengthSize)
			if c.bad {
				return nil
			}
			if !exifIDs[id] {
				continue
			}
			if reference != 0 || method > 1 || length == 0 || length > maxExifBytes-uint64(len(result)) || offset > ^uint64(0)-base {
				return nil
			}
			position := base + offset
			if method == 1 {
				if position > uint64(len(idat)) || length > uint64(len(idat))-position {
					return nil
				}
				result = append(result, idat[position:position+length]...)
			} else {
				if position > uint64(size) || length > uint64(size)-position {
					return nil
				}
				chunk := make([]byte, int(length))
				if _, err := f.ReadAt(chunk, int64(position)); err != nil {
					return nil
				}
				result = append(result, chunk...)
			}
		}
		if exifIDs[id] && len(result) >= 4 {
			offset := uint64(binary.BigEndian.Uint32(result[:4])) + 4
			if offset <= uint64(len(result)) {
				return result[offset:]
			}
		}
	}
	return nil
}
