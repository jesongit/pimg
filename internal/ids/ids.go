// Package ids 生成时间有序的 ULID 风格标识（Crockford base32，26 字符）。
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// encodeBase32 把任意字节序列编码为 Crockford base32（大端、无填充）。
func encodeBase32(b []byte) string {
	var sb strings.Builder
	acc, bits := 0, 0
	for _, by := range b {
		acc = acc<<8 | int(by)
		bits += 8
		for bits >= 5 {
			bits -= 5
			sb.WriteByte(crockford[(acc>>bits)&31])
		}
	}
	if bits > 0 {
		sb.WriteByte(crockford[(acc<<(5-bits))&31])
	}
	return sb.String()
}

// New 返回 26 字符、毫秒时间有序、随机结尾的唯一 ID（48 位时间 + 80 位随机）。
func New() string {
	var b [16]byte
	binary.BigEndian.PutUint32(b[0:4], uint32(time.Now().UnixMilli()>>16))
	binary.BigEndian.PutUint16(b[4:6], uint16(time.Now().UnixMilli()&0xFFFF))
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err)
	}
	return encodeBase32(b[:])
}

// NewShort 返回 10 字符随机标识（用于临时对象路径等）。
func NewShort() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return encodeBase32(b[:])
}

// Valid 检查 ID 形态合法（仅允许 Crockford 字符）。
func Valid(s string) bool {
	if len(s) == 0 || len(s) > 26 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune(crockford, r) {
			return false
		}
	}
	return true
}
