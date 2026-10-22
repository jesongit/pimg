// pimg — 隐私优先的个人图床：可选冷热存储，默认 Telegram 用户账号 + R2。
//
// 子命令：
//
//	pimg            启动服务
//	pimg genkey     生成 32 字节主密钥（base64）
//	pimg verify <fileID>    从冷存储恢复并校验某文件（不落盘）
//	pimg restore <fileID> <输出路径>   从冷存储恢复文件到本地
//	pimg passwd <新密码>    重置管理员密码
package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/datalayout"
	"pimg/internal/dbx"
	"pimg/internal/devmock"
	"pimg/internal/diagnostics"
	"pimg/internal/migration"
	"pimg/internal/runtimeguard"
	"pimg/internal/server"
	"pimg/internal/store"
	"pimg/internal/tasks"
	"pimg/internal/upgrade"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "recover-file":
			if err := recoverFile(args[1:], os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, "恢复失败:", err)
				os.Exit(1)
			}
			return
		case "genkey":
			fmt.Println(base64.StdEncoding.EncodeToString(cryptox.GenerateMasterKey()))
			fmt.Fprintln(os.Stderr, "请将此密钥保存在安全位置（环境变量或密钥文件）；丢失后所有已归档文件将无法解密。")
			return
		case "verify":
			runOffline(args[0], args[1:])
			return
		case "restore":
			runOffline(args[0], args[1:])
			return
		case "passwd":
			runOffline(args[0], args[1:])
			return
		case "version", "-v", "--version":
			fmt.Println("pimg", server.Version)
			return
		}
	}
	for {
		err := runServer()
		if errors.Is(err, migration.ErrRestart) {
			continue
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "启动失败:", err)
			os.Exit(1)
		}
		return
	}
}

func loadConfigForCLI() (*config.Config, error) {
	cfgPath := envOr("PIMG_CONFIG", "config.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		cfgPath = ""
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	if cfg.DevMock {
		// CLI 离线命令不支持 mock（无真实 TG 可恢复）
		cfg.DevMock = false
	}
	if err := migration.ApplyRuntime(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// runOffline verify/restore/passwd。
func runOffline(cmd string, args []string) {
	cfg, err := loadConfigForCLI()
	if err != nil {
		die(err)
	}
	releaseLayout, err := datalayout.PrepareWithUpgrade(cfg, func(path string, guard *sql.DB) error { return upgrade.BeforeLayout(cfg, path, guard) })
	if err != nil {
		die(err)
	}
	defer releaseLayout()
	if err = upgrade.RequireExistingKey(cfg); err != nil {
		die(err)
	}
	switch cmd {
	case "passwd":
		if len(args) != 1 || len(args[0]) < 8 {
			die(fmt.Errorf("用法: pimg passwd <新密码(至少8位)>"))
		}
		db := mustDB(cfg)
		h, err := bcrypt.GenerateFromPassword([]byte(args[0]), bcrypt.DefaultCost)
		if err != nil {
			die(err)
		}
		if _, _, err := db.GetAdmin(); err != nil {
			die(fmt.Errorf("管理员尚未初始化，请先启动服务完成引导"))
		}
		if err := db.UpdateAdminCredentials(firstNonEmpty(envOr("PIMG_ADMIN_USERNAME", cfg.Admin.Username), "admin"), string(h)); err != nil {
			die(err)
		}
		fmt.Println("管理员密码已重置")
	case "verify":
		if len(args) != 1 {
			die(fmt.Errorf("用法: pimg verify <fileID>"))
		}
		if err := runArchiveCLI(cfg, cmd, args); err != nil {
			die(err)
		}
	case "restore":
		if len(args) != 2 {
			die(fmt.Errorf("用法: pimg restore <fileID> <输出路径>"))
		}
		if err := runArchiveCLI(cfg, cmd, args); err != nil {
			die(err)
		}
	}
}

// runArchiveCLI releases the independent Telegram session before runOffline
// reports an error via os.Exit. Interactive authentication belongs to the UI.
func runArchiveCLI(cfg *config.Config, command string, args []string) error {
	svc := mustService(cfg)
	defer svc.DB.Close()
	defer svc.StopMediaLoads()
	f, err := svc.DB.MutationFile(args[0])
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	needsTelegram := f.ColdBackend == "telegram"
	locations, err := svc.DB.ArchiveObjects(f.ID, "")
	if err != nil {
		return err
	}
	for _, instance := range svc.StorageInstances() {
		if instance.ID == f.ColdBackend && instance.Driver == "telegram" {
			needsTelegram = true
		}
		if instance.Driver == "telegram" {
			for _, location := range locations {
				if location.InstanceID == instance.ID {
					needsTelegram = true
				}
			}
		}
	}
	var sessionErr error
	if needsTelegram && svc.UserTG != nil {
		closeSession, err := svc.ConnectTelegramSession(ctx)
		if err != nil {
			sessionErr = err
		} else {
			defer closeSession()
		}
	}
	if command == "verify" {
		result, err := svc.Verify(ctx, f.ID)
		fmt.Println(result)
		if err != nil {
			return fmt.Errorf("校验失败: %w", errors.Join(sessionErr, err))
		}
		fmt.Println("校验通过：冷存储原件可完整恢复为与上传内容一致的文件")
		return nil
	}
	rc, err := svc.ViewOriginal(ctx, f)
	if err != nil {
		return errors.Join(sessionErr, err)
	}
	defer rc.Close()
	out, err := os.Create(args[1])
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(out, rc)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Printf("已恢复 %d 字节到 %s\n", n, args[1])
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func mustDB(cfg *config.Config) *dbx.DB {
	path := cfg.Paths.DatabaseFile
	if path == "" {
		path = filepath.Join(cfg.Paths.DataDir, "pimg.db")
	}
	db, err := dbx.Open(path)
	if err != nil {
		die(err)
	}
	return db
}

func mustService(cfg *config.Config) *store.Service {
	master, err := cfg.ResolveMasterKey()
	if err != nil {
		die(err)
	}
	m, err := cryptox.NewMaster(master)
	if err != nil {
		die(err)
	}
	db := mustDB(cfg)
	tg, r2c, err := storageClients(cfg)
	if err != nil {
		die(err)
	}
	svc, err := store.New(cfg, db, m, tg, r2c, newLogger())
	if err != nil {
		die(err)
	}
	return svc
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}

func newLogger(buffers ...*diagnostics.Buffer) *slog.Logger {
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	if len(buffers) > 0 {
		handler = diagnostics.NewHandler(handler, buffers[0])
	}
	return slog.New(handler)
}

// ---------- 服务器 ----------

func runServer() error {
	cfgPath := envOr("PIMG_CONFIG", "config.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		cfgPath = ""
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := migration.ApplyPending(cfg); err != nil {
		return err
	}
	if err := migration.ApplyRuntime(cfg); err != nil {
		return err
	}
	releaseLayout, err := datalayout.PrepareWithUpgrade(cfg, func(path string, guard *sql.DB) error { return upgrade.BeforeLayout(cfg, path, guard) })
	if err != nil {
		return err
	}
	defer releaseLayout()
	if err = upgrade.RequireExistingKey(cfg); err != nil {
		return err
	}
	runtimeLogs := diagnostics.NewBuffer(cfg.MasterKey, cfg.Admin.Password, cfg.Telegram.APIHash, cfg.Telegram.BotToken, cfg.R2.SecretAccessKey, cfg.R2.AccessKeyID, cfg.R2.CDNAPIToken)
	log := newLogger(runtimeLogs)
	profile, profileErr := runtimeguard.Configure(cfg.Paths.RuntimeRoot())
	defer runtimeguard.Close()
	log.Info("运行时内存策略", "go", profile.GoVersion, "gc", profile.GC, "memory_limit_bytes", profile.MemoryLimit, "default_limit", profile.MemoryDefaulted)
	if profileErr != nil {
		log.Warn("无法保存独立运行时崩溃日志", "err", profileErr)
	} else {
		log.Info("运行时崩溃日志", "path", profile.CrashLog)
	}

	if cfg.DevMock {
		tgM, err := devmock.StartTG(log)
		if err != nil {
			return err
		}
		s3M, err := devmock.StartS3()
		if err != nil {
			return err
		}
		cfg.Telegram.APIBase = tgM.Base
		cfg.Telegram.BotToken = "mock-token"
		cfg.Telegram.ChannelID = "-100mock"
		cfg.R2.Endpoint = s3M.Base
		cfg.R2.AccessKeyID = "mock"
		cfg.R2.SecretAccessKey = "mock"
		cfg.R2.TempBucket = "pimg-temp"
		cfg.R2.PermBucket = "pimg-perm"
		cfg.R2.PublicBaseURL = s3M.Base + "/pimg-perm"
		if cfg.Server.PublicBaseURL == "" {
			cfg.Server.PublicBaseURL = "http://127.0.0.1:8787"
		}
		if cfg.MasterKey == "" && cfg.MasterKeyFile == "" {
			cfg.MasterKeyFile = filepath.Join(cfg.Paths.DataDir, "dev-master.key")
		}
		log.Info("DEV MOCK 模式：TG/R2 为进程内模拟，模拟远端重启后丢失；本地存储仍持久化")
	}

	master, err := cfg.ResolveMasterKey()
	if err != nil {
		return err
	}
	masterObj, err := cryptox.NewMaster(master)
	if err != nil {
		return err
	}
	db := mustDB(cfg)

	// 管理员引导
	if _, _, err := db.GetAdmin(); err != nil {
		username := cfg.Admin.Username
		if username == "" {
			username = "admin"
		}
		password := cfg.Admin.Password
		setupRequired := password == "" || password == "admin"
		if password == "" {
			if cfg.DevMock {
				password = "admin123"
				setupRequired = false
			} else {
				password = "admin"
			}
		}
		h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if err := db.EnsureAdminWithSetup(username, string(h), setupRequired); err != nil {
			return err
		}
		log.Info("已创建管理员账户", "username", username)
	}

	tg, r2c, err := storageClients(cfg)
	if err != nil {
		return err
	}
	svc, err := store.New(cfg, db, masterObj, tg, r2c, log)
	if err != nil {
		return err
	}
	if !cfg.DevMock {
		if err = config.SaveStartup(cfg, master); err != nil {
			return err
		}
	}
	eng := tasks.New(db, svc, cfg.Workers, log)
	if err := eng.Start(); err != nil {
		return err
	}
	srv, err := server.New(cfg, db, svc, eng, log)
	if err != nil {
		return err
	}
	srv.Diagnostics = runtimeLogs

	httpSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		log.Info("pimg 启动", "listen", cfg.Server.Listen, "version", server.Version)
		if err := httpSrv.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
			log.Error("HTTP 服务退出", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	restart := false
	select {
	case <-sig:
	case <-srv.Restart:
		restart = true
	}
	log.Info("正在停止…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
	eng.Stop()
	if err := svc.StopTelegramRuntime(ctx); err != nil {
		log.Warn("等待 Telegram 会话停止失败", "err", err)
	}
	db.Close()
	if restart {
		return migration.ErrRestart
	}
	return nil
}
