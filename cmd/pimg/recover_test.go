package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"

	"pimg/internal/carrier"
	"pimg/internal/cryptox"
)

func TestRecoverFileDoesNotInitializeConfigDatabaseOrTG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIMG_CONFIG", filepath.Join(dir, "absent.yaml"))
	t.Setenv("PIMG_DATA_DIR", filepath.Join(dir, "must-not-exist"))
	masterBytes := cryptox.GenerateMasterKey()
	master, _ := cryptox.NewMaster(masterBytes)
	key := cryptox.GenerateFileKey()
	wrapped, nonce, _ := master.WrapFileKey(key)
	plain := []byte("standalone recovery without server database")
	encrypted, _ := cryptox.EncryptBytes(plain, key)
	sum := sha256.Sum256(plain)
	var cover bytes.Buffer
	_ = png.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	wire, _ := carrier.EncodePart(cover.Bytes(), carrier.PartHeader{FileID: "cli", Kind: "data", File: carrier.CarrierFile{Size: int64(len(plain)), SHA256: hex.EncodeToString(sum[:]), KeyWrapped: wrapped, KeyNonce: nonce, CryptoVer: cryptox.CryptoVer}}, encrypted)
	keyPath, input, output := filepath.Join(dir, "master.key"), filepath.Join(dir, "renamed.png"), filepath.Join(dir, "restored.jpg")
	_ = os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(masterBytes)), 0600)
	_ = os.WriteFile(input, wire, 0600)
	args := []string{"--key-file", keyPath, input, output}
	if err := recoverFile(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(output)
	if !bytes.Equal(got, plain) {
		t.Fatal("hash mismatch")
	}
	if _, err := os.Stat(filepath.Join(dir, "must-not-exist")); !os.IsNotExist(err) {
		t.Fatal("database/service initialized")
	}
	if err := recoverFile(args, io.Discard); err == nil {
		t.Fatal("existing output overwritten")
	}
	_ = os.Remove(output)
	wire[len(wire)-1] ^= 1
	_ = os.WriteFile(input, wire, 0600)
	if err := recoverFile(args, io.Discard); err == nil {
		t.Fatal("corruption accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("partial recovered output retained")
	}
}
