package tguser

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pimg/internal/proxytraffic"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

var (
	ErrNotAuthorized   = errors.New("Telegram 用户会话尚未授权")
	ErrAccountMismatch = errors.New("Telegram 归档所属账号与当前授权账号不同")
	ErrNotFound        = errors.New("Telegram 原消息或文件不存在")
)

// Client owns one independent user login. Constructing it does not open a
// connection; Run must remain active while authentication or storage RPCs run.
type Client struct {
	opts          Options
	mu            sync.RWMutex
	status        Status
	running       bool
	connected     bool
	readyNotified bool
	mt            mediaDatacenterClient
	api           *tg.Client
	auth          *auth.Client
	store         *encryptedSessionStorage
	channels      map[int64]*tg.Channel
	runCancel     context.CancelFunc
	runContext    context.Context
	restarting    bool
	proxySeen     atomic.Bool
	// The phone and code hash exist only during the interactive login flow.
	// Neither the code nor the password is retained by Client.
	authMu   sync.Mutex
	folderMu sync.Mutex // Serialize folder read/merge/write across account tasks.
	phone    string
	codeHash string
}

var _ Transport = (*Client)(nil)

func New(o Options) *Client {
	o.SessionKey = append([]byte(nil), o.SessionKey...)
	configured := o.APIID > 0 && strings.TrimSpace(o.APIHash) != ""
	state := "unconfigured"
	if configured {
		state = "connecting"
	}
	return &Client{opts: o, status: Status{State: state, Configured: configured}, channels: make(map[int64]*tg.Channel)}
}

func (c *Client) Status() Status { c.mu.RLock(); defer c.mu.RUnlock(); return c.status }

func (c *Client) setState(state, message string) {
	c.mu.Lock()
	c.status.State = state
	c.status.Error = message
	c.mu.Unlock()
}

func (c *Client) authorized(u *tg.User) error {
	if u == nil || u.ID == 0 || u.Bot {
		return errors.New("仅支持 Telegram 用户账号授权")
	}
	c.mu.Lock()
	notify := false
	defer func() {
		c.mu.Unlock()
		if notify && c.opts.OnReady != nil {
			c.opts.OnReady()
		}
	}()
	if c.status.AccountID != 0 && c.status.AccountID != u.ID {
		c.status.State = "reauth_required"
		c.status.Error = ErrAccountMismatch.Error()
		return ErrAccountMismatch
	}
	c.status.AccountID = u.ID
	if c.status.State != "authorized" || c.status.UploadLimitBytes <= 0 {
		c.status.UploadLimitBytes = defaultUploadLimit
	}
	c.status.Name = strings.TrimSpace(u.FirstName + " " + u.LastName)
	c.status.State = "authorized"
	c.status.Error = ""
	c.status.PasswordHint = ""
	notify = !c.readyNotified
	c.readyNotified = true
	if c.runContext != nil && c.api != nil {
		go c.refreshUploadLimit(c.runContext, c.api, u.ID, u.Premium)
	}
	return nil
}

func (c *Client) connectionState(state telegram.ConnectionState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = state == telegram.ConnectionStateReady
	if state != telegram.ConnectionStateReady {
		c.readyNotified = false
		if c.status.State != "reauth_required" {
			c.status.State = "connecting"
		}
	}
}

func (c *Client) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("Telegram 用户客户端已运行")
	}
	if !c.status.Configured {
		c.mu.Unlock()
		return errors.New("请配置 Telegram 用户 API ID 和 API Hash")
	}
	c.running = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.running = false; c.mu.Unlock() }()
	for {
		err := c.runConnection(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.mu.Lock()
		restart := c.restarting
		c.restarting = false
		c.mu.Unlock()
		if !restart {
			return err
		}
	}
}

func (c *Client) runConnection(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.runCancel = cancel
	c.runContext = ctx
	c.channels = make(map[int64]*tg.Channel)
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		c.connected = false
		c.api = nil
		c.auth = nil
		c.mt = nil
		c.runCancel = nil
		c.runContext = nil
		c.mu.Unlock()
	}()
	store, err := newSessionStorage(c.opts.SessionFile, c.opts.SessionKey)
	if err != nil {
		c.setState("error", err.Error())
		return err
	}
	resolver := dcs.DefaultResolver()
	dial, err := clientProxyDial(c.opts.ProxyURL, c.opts.ProxyTraffic)
	if err != nil {
		c.setState("error", err.Error())
		return err
	}
	if dial != nil {
		configuredDial := dial
		dial = func(ctx context.Context, network, target string) (net.Conn, error) {
			if proxySelected(c.opts.ProxyURL, target) {
				c.proxySeen.Store(true)
			}
			return configuredDial(ctx, network, target)
		}
		resolver = dcs.Plain(dcs.PlainOptions{Dial: dial})
	}
	mt := telegram.NewClient(c.opts.APIID, c.opts.APIHash, telegram.Options{
		SessionStorage: store, Resolver: resolver,
		Device:            telegram.DeviceConfig{DeviceModel: "PIMG", SystemVersion: "PIMG archive", AppVersion: "1.0", LangCode: "en", SystemLangCode: "en"},
		UpdateHandler:     telegram.UpdateHandlerFunc(c.handleUpdates),
		Middlewares:       []telegram.Middleware{telegram.MiddlewareFunc(c.middleware)},
		OnConnectionState: c.connectionState,
		OnSelfSuccess:     func(u *tg.User) { _ = c.authorized(u) },
		OnSelfError: func(_ context.Context, err error) error {
			if auth.IsUnauthorized(err) {
				c.mu.Lock()
				if c.status.AccountID != 0 {
					c.status.State = "reauth_required"
					c.status.Error = "Telegram 会话已失效，请重新授权"
				}
				c.mu.Unlock()
			}
			return nil
		},
	})
	c.mu.Lock()
	c.mt = mt
	c.api = mt.API()
	c.auth = mt.Auth()
	c.store = store
	c.mu.Unlock()
	c.setState("connecting", "")
	err = mt.Run(ctx, func(runCtx context.Context) error {
		s, err := mt.Auth().Status(runCtx)
		if err != nil {
			return c.safeError(err)
		}
		if s.Authorized {
			if err := c.authorized(s.User); err != nil {
				return err
			}
		} else if c.Status().State != "reauth_required" {
			c.setState("phone", "")
		}
		<-runCtx.Done()
		return runCtx.Err()
	})
	if ctx.Err() != nil {
		c.setState("connecting", "")
		return ctx.Err()
	}
	if err != nil {
		err = c.safeError(err)
		if c.Status().State != "reauth_required" {
			c.setState("error", err.Error())
		}
	}
	return err
}

// middleware waits only for Telegram's explicit flood delay. It never logs RPC
// inputs, and cancellation interrupts even a long server-specified delay.
func (c *Client) middleware(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
		if err := c.proxyTrafficError(); err != nil {
			return err
		}
		if c.proxySeen.Load() || c.opts.ProxyURL != "" {
			if err := proxytraffic.Mark(ctx, c.opts.ProxyTraffic); err != nil {
				return err
			}
		}
		for {
			started := time.Now()
			err := next.Invoke(ctx, in, out)
			c.logAuthRPC(in, err, time.Since(started))
			if durable, _ := ctx.Value(durableProvisionContextKey{}).(bool); durable {
				if delay, ok := tgerr.AsFloodWait(err); ok {
					return &RetryAfterError{Wait: delay}
				}
				return err
			}
			if _, provisioning := in.(*tg.ChannelsCreateChannelRequest); provisioning {
				return err
			}
			if delay, ok := tgerr.AsFloodWait(err); ok {
				if delay < time.Second {
					delay = time.Second
				}
				t := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					t.Stop()
					return ctx.Err()
				case <-t.C:
					continue
				}
			}
			// Pre-login authorization probes normally return 401. Interactive
			// auth handles those challenges; an established account is different.
			if auth.IsUnauthorized(err) && c.Status().AccountID != 0 {
				c.setState("reauth_required", "Telegram 会话已失效，请重新授权")
			}
			return err
		}
	}
}

func (c *Client) safeError(err error) error {
	if err != nil {
		if quotaErr := c.proxyTrafficError(); quotaErr != nil {
			return quotaErr
		}
	}
	var retry *RetryAfterError
	if errors.As(err, &retry) {
		return err
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, ErrAccountMismatch) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotAuthorized) {
		return err
	}
	if auth.IsUnauthorized(err) {
		c.setState("reauth_required", "Telegram 会话已失效，请重新授权")
		return ErrNotAuthorized
	}
	if rpc, ok := tgerr.As(err); ok {
		if rpc.IsType("AUTH_RESTART") {
			return errors.New("Telegram 要求重新开始登录，请稍后重新获取验证码（AUTH_RESTART）")
		}
		// Telegram error types are protocol identifiers. Do not return arbitrary
		// upstream text, phone numbers, proxy credentials, or transport URLs.
		for _, r := range rpc.Type {
			if !(r >= 'A' && r <= 'Z' || r == '_') {
				return errors.New("Telegram 请求失败")
			}
		}
		if len(rpc.Type) > 80 || rpc.Type == "" {
			return errors.New("Telegram 请求失败")
		}
		return fmt.Errorf("Telegram 请求失败：%s", rpc.Type)
	}
	return errors.New("Telegram 连接或请求失败，请稍后重试")
}

func (c *Client) ready() (*tg.Client, int64, error) {
	if err := c.proxyTrafficError(); err != nil {
		return nil, 0, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.api == nil || !c.connected || c.status.State != "authorized" || c.status.AccountID == 0 {
		return nil, 0, ErrNotAuthorized
	}
	return c.api, c.status.AccountID, nil
}

func (c *Client) authClient() (*auth.Client, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.auth == nil || !c.connected {
		return nil, errors.New("Telegram 尚未连接，请稍后重试")
	}
	return c.auth, nil
}

func (c *Client) AuthPhone(ctx context.Context, phone string) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.Status().State == "authorized" {
		return errors.New("请先退出当前 Telegram 账号")
	}
	a, err := c.authClient()
	if err != nil {
		return err
	}
	phone = strings.TrimSpace(phone)
	if len(phone) < 5 || len(phone) > 32 {
		return errors.New("请输入有效的 Telegram 手机号码")
	}
	// A new sendCode starts a fresh flow; an old code hash or password challenge
	// must not survive a failed request. Telegram may explicitly request a
	// restart (including AUTH_RESTART_<debug code>); retry once, never indefinitely.
	c.phone, c.codeHash = "", ""
	c.mu.Lock()
	c.status.State, c.status.Error, c.status.PasswordHint = "phone", "", ""
	c.mu.Unlock()
	var res tg.AuthSentCodeClass
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err = a.SendCode(ctx, phone, auth.SendCodeOptions{})
		if !tgerr.Is(err, "AUTH_RESTART") {
			break
		}
	}
	if err != nil {
		err = c.safeError(err)
		c.mu.Lock()
		c.status.Error = err.Error()
		c.mu.Unlock()
		return err
	}
	switch v := res.(type) {
	case *tg.AuthSentCode:
		c.mu.Lock()
		c.channels = make(map[int64]*tg.Channel)
		c.status.PasswordHint = ""
		c.mu.Unlock()
		c.phone = phone
		c.codeHash = v.PhoneCodeHash
		c.setState("code", "")
		return nil
	case *tg.AuthSentCodeSuccess:
		if v, ok := v.Authorization.(*tg.AuthAuthorization); ok {
			if u, ok := v.User.(*tg.User); ok {
				return c.authorized(u)
			}
		}
	}
	return errors.New("Telegram 返回了暂不支持的登录流程")
}

func (c *Client) AuthCode(ctx context.Context, code string) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	a, err := c.authClient()
	if err != nil {
		return err
	}
	if c.phone == "" || c.codeHash == "" || c.Status().State != "code" {
		return errors.New("请先请求 Telegram 验证码")
	}
	res, err := a.SignIn(ctx, c.phone, strings.TrimSpace(code), c.codeHash)
	if errors.Is(err, auth.ErrPasswordAuthNeeded) {
		c.setState("password", "")
		c.mu.RLock()
		api := c.api
		c.mu.RUnlock()
		if api != nil {
			if parameters, hintErr := api.AccountGetPassword(ctx); hintErr == nil {
				c.mu.Lock()
				c.status.PasswordHint = parameters.Hint
				c.mu.Unlock()
			}
		}
		return nil
	}
	if err != nil {
		return c.safeError(err)
	}
	c.phone = ""
	c.codeHash = ""
	u, ok := res.User.(*tg.User)
	if !ok {
		return errors.New("Telegram 用户授权响应无效")
	}
	return c.authorized(u)
}

func (c *Client) AuthPassword(ctx context.Context, password string) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	a, err := c.authClient()
	if err != nil {
		return err
	}
	if c.Status().State != "password" {
		return errors.New("当前登录流程不需要两步验证密码")
	}
	c.setState("password", "")
	secret := []byte(password)
	defer func() { clear(secret) }()
	compute := auth.PasswordHashFor(secret)
	res, err := a.PasswordWith(ctx, func(ctx context.Context, parameters *tg.AccountPassword) (*tg.InputCheckPasswordSRP, error) {
		if c.opts.Logger != nil {
			_, supported := parameters.CurrentAlgo.(*tg.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow)
			c.opts.Logger.Info("telegram_auth_parameters", "algorithm_supported", supported, "srp_ready", parameters.SRPID != 0 && len(parameters.SRPB) > 0, "server_random_present", len(parameters.SecureRandom) > 0)
		}
		return compute(ctx, parameters)
	})
	if err != nil {
		if errors.Is(err, auth.ErrPasswordInvalid) {
			err = errors.New("Telegram 两步验证密码不正确")
			c.setState("password", err.Error())
			return err
		}
		err = c.safeError(err)
		c.mu.Lock()
		c.status.Error = err.Error()
		c.mu.Unlock()
		return err
	}
	c.phone = ""
	c.codeHash = ""
	u, ok := res.User.(*tg.User)
	if !ok {
		return errors.New("Telegram 用户授权响应无效")
	}
	return c.authorized(u)
}

// RestartAuth clears only an unfinished interactive flow, without logging out
// other clients or deleting the stored MTProto connection session.
func (c *Client) RestartAuth(ctx context.Context) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := c.authClient(); err != nil {
		return err
	}
	state := c.Status().State
	if state != "phone" && state != "code" && state != "password" {
		return errors.New("当前状态不支持重新开始登录，请刷新状态")
	}
	c.phone, c.codeHash = "", ""
	c.mu.Lock()
	c.status.State, c.status.Error, c.status.PasswordHint = "phone", "", ""
	c.mu.Unlock()
	return nil
}

// Only protocol identifiers and timing are logged, never RPC inputs, password
// hashes, salts, phone numbers, codes or raw upstream errors.
func (c *Client) logAuthRPC(in bin.Encoder, err error, elapsed time.Duration) {
	if c.opts.Logger == nil {
		return
	}
	method := ""
	switch in.(type) {
	case *tg.AccountGetPasswordRequest:
		method = "account.getPassword"
	case *tg.AuthCheckPasswordRequest:
		method = "auth.checkPassword"
	case *tg.AuthSignInRequest:
		method = "auth.signIn"
	case *tg.AuthSendCodeRequest:
		method = "auth.sendCode"
	default:
		return
	}
	result, kind := "ok", ""
	if err != nil {
		result, kind = "failed", "TRANSPORT_ERROR"
		if rpc, ok := tgerr.As(err); ok && len(rpc.Type) > 0 && len(rpc.Type) <= 80 {
			valid := true
			for _, r := range rpc.Type {
				if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
					valid = false
				}
			}
			if valid {
				kind = rpc.Type
			}
		}
	}
	c.opts.Logger.Info("telegram_auth_rpc", "method", method, "result", result, "error_type", kind, "duration_ms", elapsed.Milliseconds())
}

func (c *Client) Logout(ctx context.Context) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	c.mu.RLock()
	api := c.api
	connected := c.connected
	store := c.store
	c.mu.RUnlock()
	if api == nil || !connected {
		return errors.New("Telegram 尚未连接，请稍后重试")
	}
	// A revoked or mismatched login must still be removable. Logout only
	// terminates this client auth key; it never resets other device sessions.
	if _, err := api.AuthLogOut(ctx); err != nil && !auth.IsUnauthorized(err) {
		return c.safeError(err)
	}
	if store != nil {
		if err := store.Clear(ctx); err != nil {
			c.setState("reauth_required", "已退出 Telegram，但清理本地会话失败")
			return err
		}
	}
	c.phone = ""
	c.codeHash = ""
	c.mu.Lock()
	c.channels = make(map[int64]*tg.Channel)
	c.status.AccountID = 0
	c.status.UploadLimitBytes = defaultUploadLimit
	c.status.Name = ""
	c.status.State = "phone"
	c.status.Error = ""
	c.restarting = true
	cancel := c.runCancel
	c.mu.Unlock()
	// A fresh connection/storage generation is needed after logout. The old
	// storage remains disabled, so an in-flight save cannot resurrect its key.
	if cancel != nil {
		cancel()
	}
	return nil
}

func randomID() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, errors.New("无法生成 Telegram 操作编号")
	}
	var n int64
	for _, v := range b {
		n = n<<8 | int64(v)
	}
	if n == 0 {
		n = 1
	}
	return n, nil
}
