package totp

import (
	"testing"
	"time"
)

func TestRFC6238SHA1Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, v := range []struct {
		at   int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"}} {
		if got := Code(secret, v.at/30); got != v.want {
			t.Fatalf("%d: %s != %s", v.at, got, v.want)
		}
		if step, ok := Match(secret, v.want, time.Unix(v.at, 0)); !ok || step != v.at/30 {
			t.Fatal("match failed", v)
		}
	}
}

func TestBoundedClockSkewAndInvalidInput(t *testing.T) {
	secret := []byte("12345678901234567890")
	at := time.Unix(1234567890, 0)
	for _, delta := range []int64{-1, 0, 1} {
		if _, ok := Match(secret, Code(secret, at.Unix()/30+delta), at); !ok {
			t.Fatal(delta)
		}
	}
	for _, code := range []string{"", "12345", "1234567", "１２３４５６", Code(secret, at.Unix()/30-2), Code(secret, at.Unix()/30+2)} {
		if _, ok := Match(secret, code, at); ok {
			t.Fatal("accepted", code)
		}
	}
}
