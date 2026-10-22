package carrier

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"testing"
)

func TestPartStreamAndBytesRoundTrip(t *testing.T) {
	var cover bytes.Buffer
	_ = png.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 16, 16)))
	payload := append([]byte("opaque ciphertext"), []byte("\x00\x00\x00\x00IEND\xaeB`\x82")...)
	encoded, err := EncodePart(cover.Bytes(), PartHeader{FileID: "example", Kind: "data", Index: 0}, payload)
	if err != nil {
		t.Fatal(err)
	}
	_, header, decoded, err := DecodePart(encoded)
	if err != nil || !bytes.Equal(decoded, payload) {
		t.Fatalf("roundtrip: %+v %v", header, err)
	}
	reader := bytes.NewReader(encoded)
	streamHeader, err := OpenPart(reader)
	if err != nil || streamHeader.SHA256 != header.SHA256 {
		t.Fatal(err)
	}
	stream, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(stream, payload) {
		t.Fatal("stream consumed ciphertext")
	}
	encoded[len(encoded)-1] ^= 1
	if _, _, _, err := DecodePart(encoded); err == nil {
		t.Fatal("corrupt payload accepted")
	}
	for _, n := range []int{0, 7, 20, len(cover.Bytes()) + 10, len(encoded) - 1} {
		if _, _, _, err := DecodePart(encoded[:n]); err == nil {
			t.Fatalf("truncation %d accepted", n)
		}
	}
}
