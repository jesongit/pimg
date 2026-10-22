package cryptox

import (
	"bytes"
	"io"
	"testing"
)

func TestCiphertextSizeMatchesActualSegmentBoundaries(t *testing.T) {
	for _, size := range []int{0, 1, 1000, SegmentSize - 57, SegmentSize - 56, SegmentSize - 55, SegmentSize, 2*SegmentSize - 72, 2 * SegmentSize, 4*SegmentSize + 1} {
		ciphertext, err := EncryptBytes(make([]byte, size), GenerateFileKey())
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(ciphertext)) != CiphertextSize(int64(size)) {
			t.Fatalf("plain=%d actual=%d planned=%d", size, len(ciphertext), CiphertextSize(int64(size)))
		}
	}
}

func TestWrapUnwrap(t *testing.T) {
	mk := GenerateMasterKey()
	m, err := NewMaster(mk)
	if err != nil {
		t.Fatal(err)
	}
	fk := GenerateFileKey()
	w, n, err := m.WrapFileKey(fk)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.UnwrapFileKey(w, n)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fk) {
		t.Fatal("解封结果与原密钥不一致")
	}
	// 换主密钥必须失败
	m2, _ := NewMaster(GenerateMasterKey())
	if _, err := m2.UnwrapFileKey(w, n); err == nil {
		t.Fatal("错误的主密钥竟然能解封")
	}
}

func TestStreamingRoundtrip(t *testing.T) {
	plain := make([]byte, 5_500_000) // > 5 个 1MB 段
	for i := range plain {
		plain[i] = byte(i*31 + i/251)
	}
	fk := GenerateFileKey()
	er, err := EncryptReader(bytes.NewReader(plain), fk)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := io.ReadAll(er)
	if err != nil {
		t.Fatal(err)
	}
	if len(ct) <= len(plain) {
		t.Fatalf("密文应当更大: %d vs %d", len(ct), len(plain))
	}
	dr, err := DecryptReader(bytes.NewReader(ct), fk)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(dr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("往返不一致: %d bytes", len(got))
	}
	// 篡改密文必须认证失败
	ct[len(ct)-3] ^= 0xFF
	dr2, err := DecryptReader(bytes.NewReader(ct), fk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(dr2); err == nil {
		t.Fatal("篡改后的密文竟然解密成功")
	}
}

func TestBytesRoundtrip(t *testing.T) {
	plain := []byte("hello pimg 缩略图")
	fk := GenerateFileKey()
	ct, err := EncryptBytes(plain, fk)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecryptBytes(ct, fk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("小数据往返不一致")
	}
	if _, err := DecryptBytes(ct, GenerateFileKey()); err == nil {
		t.Fatal("错误密钥竟然解密成功")
	}
}
