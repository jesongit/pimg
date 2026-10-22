package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"pimg/internal/dbx"
	"pimg/internal/totp"
	"rsc.io/qr"
)

func (s *Server) registerTwoFactorRoutes(mux *http.ServeMux) {
	for _, route := range []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{"GET", "", s.handleTwoFactorStatus}, {"POST", "/setup", s.handleTwoFactorSetup},
		{"POST", "/enable", s.handleTwoFactorChange}, {"POST", "/disable", s.handleTwoFactorChange},
		{"POST", "/recovery", s.handleTwoFactorChange},
	} {
		mux.HandleFunc(route.method+" /api/auth/2fa"+route.path, func(w http.ResponseWriter, r *http.Request) {
			// Never hold the shared auth lock while reading from the network.
			if r.Method == http.MethodPost {
				var req factorReq
				if err := readAuthBody(w, r, &req); err != nil {
					fail(w, 400, "请填写当前密码和验证码")
					return
				}
				body, err := json.Marshal(req)
				if err != nil {
					fail(w, 400, "验证信息无效")
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			s.factorMu.Lock()
			defer s.factorMu.Unlock()
			s.accountAuth(func(w http.ResponseWriter, r *http.Request) {
				// API credentials must never enroll, remove, or recover an interactive factor.
				if r.Header.Get("Authorization") != "" {
					fail(w, 403, "请使用网页登录会话管理两步验证")
					return
				}
				route.handler(w, r)
			})(w, r)
		})
	}
}

// Authentication payloads are tiny. Bound their read duration independently of
// large upload endpoints and clear the deadline for keep-alive connections.
func readAuthBody(w http.ResponseWriter, r *http.Request, dst any) error {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	return readJSONBody(r, dst, 4096)
}

func (s *Server) factorCipher() (cipher.AEAD, error) {
	h := hmac.New(sha256.New, s.Svc.Master.Raw())
	h.Write([]byte("pimg/two-factor/v1"))
	b, err := aes.NewCipher(h.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func (s *Server) factorSecret(owner string, f *dbx.TwoFactor) ([]byte, error) {
	g, err := s.factorCipher()
	if err != nil {
		return nil, err
	}
	if len(f.Nonce) != g.NonceSize() {
		return nil, errors.New("invalid nonce")
	}
	return g.Open(nil, f.Nonce, f.Secret, []byte(owner))
}

func recoveryHash(owner, code string) string {
	code = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	h := sha256.Sum256([]byte("pimg/recovery/v1\x00" + owner + "\x00" + code))
	return hex.EncodeToString(h[:])
}

func recoveryCodes(owner string) ([]string, []string, error) {
	codes, hashes := []string{}, []string{}
	for i := 0; i < 10; i++ {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			return nil, nil, err
		}
		h := hex.EncodeToString(b)
		code := h[:5] + "-" + h[5:10] + "-" + h[10:15] + "-" + h[15:]
		codes = append(codes, code)
		hashes = append(hashes, recoveryHash(owner, code))
	}
	return codes, hashes, nil
}

// No success path accepts the same TOTP time step or recovery code twice.
func (s *Server) verifyFactor(owner, code string, f *dbx.TwoFactor) bool {
	if f == nil || !f.Enabled {
		return false
	}
	if len(strings.TrimSpace(code)) == 6 {
		secret, err := s.factorSecret(owner, f)
		if err != nil {
			return false
		}
		step, ok := totp.Match(secret, code, time.Now())
		return ok && s.DB.ConsumeTwoFactorStep(owner, f.Nonce, step) == nil
	}
	return s.DB.ConsumeRecovery(owner, recoveryHash(owner, code)) == nil
}

type factorReq struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (s *Server) factorPassword(w http.ResponseWriter, r *http.Request) (factorReq, bool) {
	var req factorReq
	ip := clientIP(r)
	owner := dbx.ContextOwner(r.Context())
	if !s.guard.allow("factor-ip:"+ip) || !s.guard.allow("factor-owner:"+owner) {
		fail(w, 429, "验证次数过多，请 15 分钟后再试")
		return req, false
	}
	if readJSONBody(r, &req, 4096) != nil {
		fail(w, 400, "请填写当前密码和验证码")
		return req, false
	}
	var hash string
	var err error
	if owner == dbx.AdminOwner {
		_, hash, err = s.DB.GetAdmin()
	} else {
		err = s.DB.QueryRow(`SELECT password_hash FROM accounts WHERE id=? AND disabled=0`, owner).Scan(&hash)
	}
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		s.factorFailure(r, owner)
		fail(w, 400, "当前密码错误")
		return req, false
	}
	return req, true
}

func (s *Server) factorFailure(r *http.Request, owner string) {
	s.guard.fail("factor-ip:" + clientIP(r))
	s.guard.fail("factor-owner:" + owner)
}

func (s *Server) handleTwoFactorStatus(w http.ResponseWriter, r *http.Request) {
	owner := dbx.ContextOwner(r.Context())
	f, err := s.DB.TwoFactor(owner)
	if err != nil {
		fail(w, 503, "无法读取两步验证状态")
		return
	}
	n, err := s.DB.RecoveryCount(owner)
	if err != nil {
		fail(w, 503, "无法读取恢复码状态")
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": f != nil && f.Enabled, "recovery_remaining": n})
}

func (s *Server) handleTwoFactorSetup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.factorPassword(w, r); !ok {
		return
	}
	owner := dbx.ContextOwner(r.Context())
	f, err := s.DB.TwoFactor(owner)
	if err != nil {
		fail(w, 503, "无法读取两步验证状态")
		return
	}
	if f != nil && f.Enabled {
		fail(w, 409, "两步验证已开启")
		return
	}
	secret, err := totp.NewSecret()
	if err != nil {
		fail(w, 500, "无法生成绑定密钥")
		return
	}
	g, err := s.factorCipher()
	if err != nil {
		fail(w, 500, "无法保护绑定密钥")
		return
	}
	nonce := make([]byte, g.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		fail(w, 500, "无法生成绑定密钥")
		return
	}
	var username string
	if owner == dbx.AdminOwner {
		username, _, err = s.DB.GetAdmin()
	} else {
		err = s.DB.QueryRow(`SELECT username FROM accounts WHERE id=?`, owner).Scan(&username)
	}
	if err != nil {
		fail(w, 503, "无法读取账号")
		return
	}
	issuer := "PIMG (" + r.Host + ")"
	u := url.URL{Scheme: "otpauth", Host: "totp", Path: "/" + issuer + ":" + username}
	q := url.Values{"secret": {totp.SecretText(secret)}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	u.RawQuery = q.Encode()
	qrCode, err := qr.Encode(u.String(), qr.M)
	if err != nil {
		fail(w, 500, "无法生成绑定二维码")
		return
	}
	expires := time.Now().Add(10 * time.Minute).Unix()
	if err = s.DB.SetTwoFactorPending(owner, g.Seal(nil, nonce, secret, []byte(owner)), nonce, expires); err != nil {
		fail(w, 409, "绑定状态已改变，请刷新后重试")
		return
	}
	writeJSON(w, 200, map[string]any{"secret": totp.SecretText(secret), "qr_image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(qrCode.PNG()), "expires_at": expires})
}

func (s *Server) handleTwoFactorChange(w http.ResponseWriter, r *http.Request) {
	req, ok := s.factorPassword(w, r)
	if !ok {
		return
	}
	owner := dbx.ContextOwner(r.Context())
	f, err := s.DB.TwoFactor(owner)
	if err != nil {
		fail(w, 503, "无法读取两步验证状态")
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/api/auth/2fa/")
	var step int64
	if action == "enable" {
		if f == nil || f.Enabled || f.ExpiresAt <= time.Now().Unix() {
			fail(w, 409, "绑定已失效，请重新开始")
			return
		}
		secret, e := s.factorSecret(owner, f)
		if e == nil {
			step, ok = totp.Match(secret, req.Code, time.Now())
		} else {
			ok = false
		}
	} else {
		ok = s.verifyFactor(owner, req.Code, f)
	}
	if !ok {
		s.factorFailure(r, owner)
		fail(w, 400, "验证码无效或已使用，请等待新动态码，或使用未使用的恢复码")
		return
	}
	var codes, hashes []string
	if action != "disable" {
		codes, hashes, err = recoveryCodes(owner)
		if err != nil {
			fail(w, 500, "无法生成恢复码")
			return
		}
	}
	keep := ""
	if c, e := r.Cookie(sessionCookie); e == nil {
		sum := sha256.Sum256([]byte(c.Value))
		keep = hex.EncodeToString(sum[:])
	}
	if err = s.DB.FinishTwoFactor(owner, action, keep, f.Nonce, step, hashes); err != nil {
		fail(w, 409, "保存失败，请刷新后重试")
		return
	}
	s.guard.reset("factor-owner:" + owner)
	s.guard.reset("factor-ip:" + clientIP(r))
	writeJSON(w, 200, map[string]any{"ok": true, "enabled": action != "disable", "recovery_codes": codes})
}
