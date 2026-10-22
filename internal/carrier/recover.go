package carrier

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"pimg/internal/cryptox"
)

// Recover restores one complete encrypted original using only its carrier and
// master key. It never opens a database or connects to a storage provider.
// Callers must discard partial output when authentication/validation fails.
func Recover(input io.Reader, output io.Writer, master *cryptox.Master) (CarrierFile, error) {
	h, err := OpenPart(input)
	if err != nil {
		return CarrierFile{}, err
	}
	f := h.File
	if h.Kind != "data" || h.Index != 0 || f.CryptoVer != cryptox.CryptoVer || f.Size <= 0 || len(f.SHA256) != 64 || h.Size != cryptox.CiphertextSize(f.Size) {
		return f, errors.New("载体不是完整的独立加密原件")
	}
	key, err := master.UnwrapFileKey(f.KeyWrapped, f.KeyNonce)
	if err != nil {
		return f, err
	}
	defer clear(key)
	limited := &io.LimitedReader{R: input, N: h.Size}
	cipherHash := sha256.New()
	plain, err := cryptox.DecryptReader(io.TeeReader(limited, cipherHash), key)
	if err != nil {
		return f, err
	}
	plainHash := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, plainHash), io.LimitReader(plain, f.Size+1))
	if err != nil {
		return f, err
	}
	if n != f.Size || limited.N != 0 || hex.EncodeToString(plainHash.Sum(nil)) != f.SHA256 || hex.EncodeToString(cipherHash.Sum(nil)) != h.SHA256 {
		return f, errors.New("原件恢复校验失败，文件不完整或已损坏")
	}
	var tail [1]byte
	if n, err := input.Read(tail[:]); n != 0 || err != io.EOF {
		return f, errors.New("载体尾部存在额外数据")
	}
	return f, nil
}
