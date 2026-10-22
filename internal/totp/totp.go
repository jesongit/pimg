// Package totp implements the RFC 6238 SHA-1, six-digit, 30-second profile.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

func NewSecret() ([]byte, error) {
	b := make([]byte, 20)
	_, err := rand.Read(b)
	return b, err
}

func SecretText(secret []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
}

func Code(secret []byte, step int64) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	h := hmac.New(sha1.New, secret)
	h.Write(counter[:])
	sum := h.Sum(nil)
	offset := sum[len(sum)-1] & 15
	n := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1000000)
}

// Match allows at most one time step of clock skew. Callers must persist the
// returned step and atomically reject reuse, including after a process restart.
func Match(secret []byte, code string, at time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != 6 || strings.IndexFunc(code, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, false
	}
	step := at.Unix() / 30
	for _, delta := range []int64{0, -1, 1} {
		if subtle.ConstantTimeCompare([]byte(Code(secret, step+delta)), []byte(code)) == 1 {
			return step + delta, true
		}
	}
	return 0, false
}
