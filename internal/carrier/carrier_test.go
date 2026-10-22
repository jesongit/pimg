package carrier

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	// 1x1 红 PNG
	b64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCarrierRoundtrip(t *testing.T) {
	png := tinyPNG(t)
	var buf bytes.Buffer
	w, err := NewWriter(&buf, png)
	if err != nil {
		t.Fatal(err)
	}
	hdr := &Header{
		V: 1,
		File: CarrierFile{
			Name: "测试", Ext: "jpg", Mime: "image/jpeg", Size: 123,
			SHA256: "abc", KeyWrapped: []byte{1, 2}, KeyNonce: []byte{3},
		},
		Chunks: []CarrierChunk{
			{Kind: "data", Idx: 0, Size: 10, SHA256: "d0"},
			{Kind: "thumb", Idx: 0, Size: 4, SHA256: "t0"},
		},
	}
	if err := w.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteChunk([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteChunk([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	gotPNG, gotHdr, blob, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPNG, png) {
		t.Fatal("PNG 部分不一致")
	}
	if gotHdr.File.Name != "测试" || len(gotHdr.Chunks) != 2 {
		t.Fatalf("头解析错误: %+v", gotHdr)
	}
	if string(blob) != "0123456789abcd" {
		t.Fatalf("blob 不一致: %q", blob)
	}
}

func TestParseRejectsPlainPNG(t *testing.T) {
	if _, _, _, err := Parse(tinyPNG(t)); err == nil {
		t.Fatal("普通 PNG 不应被识别为导出载体")
	}
}
