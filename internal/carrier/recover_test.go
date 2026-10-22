package carrier

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
	"io"
	"testing"

	"pimg/internal/cryptox"
)

func recoveryFixture(t *testing.T) ([]byte, []byte, *cryptox.Master) {
	t.Helper()
	master, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	key := cryptox.GenerateFileKey()
	wrapped, nonce, _ := master.WrapFileKey(key)
	plain := bytes.Repeat([]byte("independent database-free recovery\x00"), 70000)
	encrypted, err := cryptox.EncryptBytes(plain, key)
	if err != nil {
		t.Fatal(err)
	}
	var cover bytes.Buffer
	_ = png.Encode(&cover, image.NewNRGBA(image.Rect(0, 0, 4, 4)))
	sum := sha256.Sum256(plain)
	wire, err := EncodePart(cover.Bytes(), PartHeader{FileID: "standalone", Kind: "data", File: CarrierFile{OrigName: "photo.jpg", Size: int64(len(plain)), SHA256: hex.EncodeToString(sum[:]), KeyWrapped: wrapped, KeyNonce: nonce, CryptoVer: cryptox.CryptoVer}}, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	return wire, plain, master
}

func TestRecoverWithoutDatabaseOrExternalManifest(t *testing.T) {
	wire, plain, master := recoveryFixture(t)
	var recovered bytes.Buffer
	meta, err := Recover(bytes.NewReader(wire), &recovered, master)
	if err != nil || !bytes.Equal(plain, recovered.Bytes()) || meta.OrigName != "photo.jpg" {
		t.Fatal("standalone recovery", err)
	}
}

func TestRecoverRejectsWrongKeyCorruptionTruncationAndExtraData(t *testing.T) {
	wire, _, master := recoveryFixture(t)
	wrong, _ := cryptox.NewMaster(cryptox.GenerateMasterKey())
	if _, err := Recover(bytes.NewReader(wire), io.Discard, wrong); err == nil {
		t.Fatal("wrong master accepted")
	}
	corrupt := append([]byte(nil), wire...)
	corrupt[len(corrupt)-25] ^= 1
	for name, data := range map[string][]byte{"corrupt": corrupt, "truncated": wire[:len(wire)-1], "trailing": append(append([]byte(nil), wire...), 1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := Recover(bytes.NewReader(data), io.Discard, master); err == nil {
				t.Fatal("damaged archive accepted")
			}
		})
	}
}
