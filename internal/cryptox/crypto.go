// Package cryptox 提供服务端透明加密：
//   - 主密钥（32 字节，来自安全配置）用 AES-256-GCM 封装每个文件的独立随机密钥；
//   - 文件内容用 Tink 的 AESGCMHKDF 流式认证加密（AES-256-GCM 段式流，成熟实现，非自拼协议），
//     加解密均以流式进行，任意大小文件都不整份驻留内存。
//
// 密文格式版本 1：tink aes256gcm-hkdf 1MB 段。AAD 为常量 "pimg/media/v1"，
// 使导出/导入到不同记录 ID 时仍可解密（每个文件密钥本身独立随机）。
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	tinksubtle "github.com/tink-crypto/tink-go/v2/streamingaead/subtle"
)

// AADMedia 流式加密的关联数据常量。
const AADMedia = "pimg/media/v1"

const aadKeyWrap = "pimg/keywrap/v1"

// SegmentSize 流式加密的密文段大小（与 Tink AES256GCMHKDF1MB 模板一致）。
const SegmentSize = 1024 * 1024

// CryptoVer 加密格式版本。
const CryptoVer = 1

// CiphertextSize is exact for the current Tink template, including the header
// and each authentication tag. Size planning must include these bytes.
func CiphertextSize(plain int64) int64 {
	const header, tag = int64(40), int64(16)
	segments := (plain + header + SegmentSize - tag - 1) / (SegmentSize - tag)
	return plain + header + segments*tag
}

// Master 持有主密钥的 AES-GCM 实例。
type Master struct {
	gcm cipher.AEAD
	raw []byte
}

// NewMaster 用 32 字节主密钥构造。
func NewMaster(key []byte) (*Master, error) {
	if len(key) != 32 {
		return nil, errors.New("主密钥长度必须为 32 字节")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Master{gcm: gcm, raw: key}, nil
}

// GenerateMasterKey 生成随机 32 字节主密钥。
func GenerateMasterKey() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// GenerateFileKey 生成随机 32 字节文件密钥。
func GenerateFileKey() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// WrapFileKey 用主密钥封装文件密钥，返回 (密文, nonce)。
func (m *Master) WrapFileKey(fileKey []byte) (wrapped, nonce []byte, err error) {
	nonce = make([]byte, m.gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	wrapped = m.gcm.Seal(nil, nonce, fileKey, []byte(aadKeyWrap))
	return wrapped, nonce, nil
}

// UnwrapFileKey 解封装文件密钥。
func (m *Master) UnwrapFileKey(wrapped, nonce []byte) ([]byte, error) {
	if len(nonce) != m.gcm.NonceSize() {
		return nil, errors.New("密钥封装 nonce 长度错误")
	}
	fk, err := m.gcm.Open(nil, nonce, wrapped, []byte(aadKeyWrap))
	if err != nil {
		return nil, fmt.Errorf("解封装文件密钥失败（主密钥不匹配或数据损坏）: %w", err)
	}
	return fk, nil
}

// Raw 返回主密钥字节（仅供内部派生用途，绝不写日志）。
func (m *Master) Raw() []byte { return m.raw }

func newStreaming(fileKey []byte) (*tinksubtle.AESGCMHKDF, error) {
	return tinksubtle.NewAESGCMHKDF(fileKey, "SHA256", 32, SegmentSize, 0)
}

// EncryptReader 返回一个流式加密 Reader：读取 plain 的同时产出密文流。
// 构造必须在消费协程内进行（NewEncryptingWriter 会同步写头块到管道）。
func EncryptReader(plain io.Reader, fileKey []byte) (io.Reader, error) {
	sa, err := newStreaming(fileKey)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		w, err := sa.NewEncryptingWriter(pw, []byte(AADMedia))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		_, err = io.Copy(w, plain)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		pw.CloseWithError(err) // nil 时正常关闭
	}()
	return pr, nil
}

// DecryptReader 返回一个流式解密 Reader；认证失败在读取过程中报告。
func DecryptReader(cipherText io.Reader, fileKey []byte) (io.Reader, error) {
	sa, err := newStreaming(fileKey)
	if err != nil {
		return nil, err
	}
	return sa.NewDecryptingReader(cipherText, []byte(AADMedia))
}

// EncryptBytes 小数据（缩略图等）一次性加密。
func EncryptBytes(plain []byte, fileKey []byte) ([]byte, error) {
	sa, err := newStreaming(fileKey)
	if err != nil {
		return nil, err
	}
	var buf zeroBuffer
	w, err := sa.NewEncryptingWriter(&buf, []byte(AADMedia))
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.b, nil
}

// DecryptBytes 小数据一次性解密。
func DecryptBytes(cipherText []byte, fileKey []byte) ([]byte, error) {
	sa, err := newStreaming(fileKey)
	if err != nil {
		return nil, err
	}
	r, err := sa.NewDecryptingReader(newSliceReader(cipherText), []byte(AADMedia))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

type zeroBuffer struct{ b []byte }

func (z *zeroBuffer) Write(p []byte) (int, error) {
	z.b = append(z.b, p...)
	return len(p), nil
}

type sliceReader struct {
	b []byte
	i int
}

func newSliceReader(b []byte) *sliceReader { return &sliceReader{b: b} }

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.i >= len(s.b) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.i:])
	s.i += n
	return n, nil
}
