package tguser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"golang.org/x/crypto/pbkdf2"
)

// Build a server-side SRP verifier independently from the SDK's client proof,
// following https://core.telegram.org/api/srp. No real-account data is used.
func passwordServer(t *testing.T, password string) (*tg.AccountPassword, func(*tg.InputCheckPasswordSRP) bool) {
	t.Helper()
	pBytes, err := hex.DecodeString("C71CAEB9C6B1C9048E6C522F70F13F73980D40238E3E21C14934D037563D930F" +
		"48198A0AA7C14058229493D22530F4DBFA336F6E0AC925139543AED44CCE7C37" +
		"20FD51F69458705AC68CD4FE6B6B13ABDC9746512969328454F18FAF8C595F64" +
		"2477FE96BB2A941D5BCD1D4AC8CC49880708FA9B378E3C4F3A9060BEE67CF9A4" +
		"A4A695811051907E162753B56B0F6B410DBA74D8A84B2A14B3144E0EF1284754" +
		"FD17ED950D5965B4B9DD46582DB1178D169C6BC465B0D6FF9CA3928FEF5B9AE4" +
		"E418FC15E83EBEA0F87FA9FF5EED70050DED2849F47BF959D956850CE929851F" +
		"0D8115F635B105EE2E4E15D04B2454BF6F4FADF034B10403119CD8E3B92FCC5B")
	if err != nil {
		t.Fatal(err)
	}
	hash := func(parts ...[]byte) []byte {
		h := sha256.New()
		for _, part := range parts {
			_, _ = h.Write(part)
		}
		return h.Sum(nil)
	}
	pad := func(n *big.Int) []byte { return n.FillBytes(make([]byte, 256)) }
	p, g := new(big.Int).SetBytes(pBytes), big.NewInt(3)
	salt1, salt2 := []byte("first-fixture-salt"), []byte("second-fixture-salt")
	first := hash(salt1, []byte(password), salt1)
	second := hash(salt2, first, salt2)
	derived := pbkdf2.Key(second, salt1, 100000, 64, sha512.New)
	x := new(big.Int).SetBytes(hash(salt2, derived, salt2))
	v := new(big.Int).Exp(g, x, p)
	k := new(big.Int).SetBytes(hash(pBytes, pad(g)))
	b := new(big.Int).SetBytes(bytes.Repeat([]byte{0x42}, 256))
	B := new(big.Int).Add(new(big.Int).Mul(k, v), new(big.Int).Exp(g, b, p))
	B.Mod(B, p)
	parameters := &tg.AccountPassword{
		HasPassword: true, Hint: "offline fixture hint", SRPID: 123, SRPB: pad(B),
		SecureRandom: bytes.Repeat([]byte{0x31}, 256),
		CurrentAlgo:  &tg.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow{G: 3, P: pBytes, Salt1: salt1, Salt2: salt2},
	}
	return parameters, func(proof *tg.InputCheckPasswordSRP) bool {
		if proof.SRPID != parameters.SRPID || len(proof.A) != 256 {
			return false
		}
		A := new(big.Int).SetBytes(proof.A)
		u := new(big.Int).SetBytes(hash(pad(A), pad(B)))
		base := new(big.Int).Mul(A, new(big.Int).Exp(v, u, p))
		base.Mod(base, p)
		shared := new(big.Int).Exp(base, b, p)
		xor := hash(pBytes)
		for i, value := range hash(pad(g)) {
			xor[i] ^= value
		}
		expected := hash(xor, hash(salt1), hash(salt2), pad(A), pad(B), hash(pad(shared)))
		return bytes.Equal(proof.M1, expected)
	}
}

func TestUserPasswordProofPreservesWhitespaceAndUnicode(t *testing.T) {
	const actualPassword = "  fixture-密码-Пароль-🙂\t"
	for _, supplied := range []string{actualPassword, strings.TrimSpace(actualPassword)} {
		t.Run(map[bool]string{true: "exact", false: "trimmed-is-different"}[supplied == actualPassword], func(t *testing.T) {
			parameters, valid := passwordServer(t, actualPassword)
			var logs bytes.Buffer
			checks := 0
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch req := in.(type) {
				case *tg.AccountGetPasswordRequest:
					*out.(*tg.AccountPassword) = *parameters
				case *tg.AuthCheckPasswordRequest:
					checks++
					if !valid(req.Password.(*tg.InputCheckPasswordSRP)) {
						return tgerr.New(400, "PASSWORD_HASH_INVALID")
					}
					out.(*tg.AuthAuthorizationBox).Authorization = &tg.AuthAuthorization{User: &tg.User{ID: 77}}
				default:
					t.Fatalf("unexpected login RPC %T", in)
				}
				return nil
			})
			c.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			c.status = Status{State: "password", Configured: true}
			err := c.AuthPassword(context.Background(), supplied)
			if checks != 1 {
				t.Fatal("password was not checked exactly once")
			}
			if supplied == actualPassword {
				if err != nil || c.Status().State != "authorized" {
					t.Fatal("valid server-side proof rejected", err)
				}
			} else if err == nil || c.Status().State != "password" || c.Status().Error != "Telegram 两步验证密码不正确" || !strings.Contains(logs.String(), "PASSWORD_HASH_INVALID") {
				t.Fatal("wrong password must retain a safe failure for status refresh")
			}
			if strings.Contains(logs.String(), "fixture-") || strings.Contains(logs.String(), "offline fixture hint") || !strings.Contains(logs.String(), "account.getPassword") || !strings.Contains(logs.String(), "auth.checkPassword") {
				t.Fatal("authentication diagnostics leaked data or missed a stage")
			}
		})
	}
}

func TestUserRestartAuthClearsOnlyUnfinishedLogin(t *testing.T) {
	c := fakeClient(func(context.Context, bin.Encoder, bin.Decoder) error {
		t.Fatal("restart made a remote request")
		return nil
	})
	c.status = Status{State: "password", Configured: true, PasswordHint: "fixture", Error: "previous error"}
	c.phone, c.codeHash = "fixture-phone", "fixture-code-hash"
	if err := c.RestartAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Status().State != "phone" || c.Status().Error != "" || c.Status().PasswordHint != "" || c.phone != "" || c.codeHash != "" {
		t.Fatal("old login data survived restart")
	}
	if err := c.AuthCode(context.Background(), "old-code"); err == nil {
		t.Fatal("old code flow can still be used")
	}
	c.status.State, c.status.AccountID = "authorized", 77
	if err := c.RestartAuth(context.Background()); err == nil || c.Status().State != "authorized" {
		t.Fatal("restart reset an authorized account")
	}
}

func TestUserInitialUnauthorizedProbeIsNotSessionRevocation(t *testing.T) {
	c := fakeClient(func(context.Context, bin.Encoder, bin.Decoder) error {
		return tgerr.New(401, "AUTH_KEY_UNREGISTERED")
	})
	c.status = Status{State: "phone", Configured: true}
	if status, err := c.auth.Status(context.Background()); err != nil || status.Authorized || c.Status().State != "phone" || c.Status().Error != "" {
		t.Fatal("ordinary pre-login probe looked like a revoked account", err)
	}
	c.status = Status{State: "authorized", Configured: true, AccountID: 77}
	if _, err := c.auth.Status(context.Background()); err != nil || c.Status().State != "reauth_required" {
		t.Fatal("established account revocation was not surfaced", err)
	}
}

func TestUserSendCodeRestartsAuthorizationOnce(t *testing.T) {
	for _, restartType := range []string{"AUTH_RESTART", "AUTH_RESTART_42"} {
		t.Run(restartType, func(t *testing.T) {
			calls := 0
			var c *Client
			c = fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch req := in.(type) {
				case *tg.AuthSendCodeRequest:
					calls++
					if req.PhoneNumber != "+123456789" || c.phone != "" || c.codeHash != "" || c.Status().State != "phone" || c.Status().PasswordHint != "" {
						t.Fatal("new phone request retained the previous login flow")
					}
					if calls == 1 {
						return tgerr.New(500, restartType)
					}
					out.(*tg.AuthSentCodeBox).SentCode = &tg.AuthSentCode{PhoneCodeHash: "new-code-hash"}
				case *tg.AuthSignInRequest:
					if req.PhoneNumber != "+123456789" || req.PhoneCodeHash != "new-code-hash" {
						t.Fatal("sign-in did not use the restarted authorization")
					}
					out.(*tg.AuthAuthorizationBox).Authorization = &tg.AuthAuthorization{User: &tg.User{ID: 77}}
				default:
					t.Fatalf("unexpected RPC %T", in)
				}
				return nil
			})
			c.status = Status{State: "password", Configured: true, PasswordHint: "stale hint", Error: "stale error"}
			c.phone, c.codeHash = "previous-phone", "previous-code-hash"
			if err := c.AuthPhone(context.Background(), " +123456789 "); err != nil || calls != 2 || c.Status().State != "code" || c.Status().Error != "" {
				t.Fatal("sendCode restart did not recover", err, calls, c.Status())
			}
			if err := c.AuthCode(context.Background(), "12345"); err != nil || c.Status().State != "authorized" {
				t.Fatal("recovered code could not complete sign-in", err)
			}
		})
	}
}

func TestUserSendCodeRestartStopsAndPreservesSafeFailure(t *testing.T) {
	for _, kind := range []string{"AUTH_RESTART", "AUTH_RESTART_42", "PHONE_NUMBER_INVALID"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				if _, ok := in.(*tg.AuthSendCodeRequest); !ok {
					t.Fatalf("unexpected RPC %T", in)
				}
				calls++
				return tgerr.New(500, kind)
			})
			c.status = Status{State: "code", Configured: true}
			c.phone, c.codeHash = "old-phone", "old-hash"
			err := c.AuthPhone(context.Background(), "+123456789")
			wantCalls := 2
			if kind == "PHONE_NUMBER_INVALID" {
				wantCalls = 1
			}
			if err == nil || calls != wantCalls || c.Status().State != "phone" || c.Status().Error != err.Error() || c.phone != "" || c.codeHash != "" {
				t.Fatal("sendCode failure retried indefinitely or retained stale state", err, calls, c.Status())
			}
			if kind != "PHONE_NUMBER_INVALID" && !strings.Contains(err.Error(), "重新获取验证码") {
				t.Fatal("restart has no actionable message", err)
			}
			if err := c.AuthCode(context.Background(), "old-code"); err == nil || calls != wantCalls {
				t.Fatal("failed restart left the old code usable")
			}
		})
	}
}

func TestUserSendCodeRestartHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c := fakeClient(func(context.Context, bin.Encoder, bin.Decoder) error {
		calls++
		cancel()
		return tgerr.New(500, "AUTH_RESTART")
	})
	c.status = Status{State: "phone", Configured: true}
	if err := c.AuthPhone(ctx, "+123456789"); !errors.Is(err, context.Canceled) || calls != 1 || c.codeHash != "" || c.Status().State != "phone" {
		t.Fatal("canceled sendCode was retried", err, calls)
	}
}
