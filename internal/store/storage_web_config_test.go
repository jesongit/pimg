package store

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"pimg/internal/config"
	"pimg/internal/storage"
	"pimg/internal/tguser"
)

func TestWebStorageMigrationSurvivesRemovedDeploymentCredentials(t *testing.T) {
	s, _, _ := newTestEnv(t)
	s.Cfg.DevMock = false // Exercise independent persisted clients, not injected mocks.
	if err := s.initInstances(); err != nil {
		t.Fatal(err)
	}
	p := s.StoragePolicy()
	if err := s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	permanent, _ := s.instance("r2")
	c := permanent.config
	c.AccessKey = "rotated-access"
	c.SecretKey = "rotated-secret"
	c.CDNToken = "private-cdn-token"
	if _, err := s.SaveStorageInstance("r2", "可编辑的永久桶", "r2", c, true); err != nil {
		t.Fatal(err)
	}
	c.AccessKey = ""
	c.SecretKey = ""
	c.CDNToken = ""
	if _, err := s.SaveStorageInstance("r2", "再次修改名称", "r2", c, true); err != nil {
		t.Fatal(err)
	}
	conn := s.storageConnection()
	conn.APIID = 123
	conn.APIHash = strings.Repeat("b", 32)
	if err := s.persistStorageConnection(conn); err != nil {
		t.Fatal(err)
	}
	clean := &config.Config{Paths: s.Cfg.Paths, Server: s.Cfg.Server, Storage: config.Storage{Cold: "local", Hot: "local"}}
	resumed, err := New(clean, s.DB, s.Master, s.TG, nil, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	i, _ := resumed.instance("r2")
	if i.row.Name != "再次修改名称" || i.config.AccessKey != "rotated-access" || i.config.SecretKey != "rotated-secret" || i.config.CDNToken != "private-cdn-token" {
		t.Fatal("file/env replaced persisted credentials or name")
	}
	if !reflect.DeepEqual(resumed.StoragePolicy(), p) || resumed.storageConnection().APIID != 123 {
		t.Fatal("policy or TG credentials lost on restart")
	}
	for _, v := range resumed.StorageInstances() {
		if v.Config.AccessKey != "" || v.Config.SecretKey != "" || v.Config.CDNToken != "" {
			t.Fatal("secret in admin response")
		}
	}
	var raw []byte
	if err = s.DB.QueryRow("SELECT config FROM storage_instances WHERE id='r2'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("rotated-secret")) || bytes.Contains(raw, []byte("private-cdn-token")) {
		t.Fatal("unencrypted instance secret")
	}
	settings, _ := s.DB.AllSettings()
	if strings.Contains(settings[connectionKey], conn.APIHash) {
		t.Fatal("unencrypted TG secret")
	}
}

func TestWebConnectionReconnectWaitsForSessionAndRetainsIdentity(t *testing.T) {
	s, r := runtimeFixture(t)
	if err := s.initStorageConnection(); err != nil {
		t.Fatal(err)
	}
	r.chatID = 41
	s.StartTelegramRuntime(context.Background())
	t.Cleanup(func() { stopRuntime(t, r) })
	old := r.current().(*runtimeFixtureClient)
	waitRuntime(t, old.started)
	next := newRuntimeFixtureClient()
	var options tguser.Options
	r.newClient = func(o tguser.Options) telegramUserClient { options = o; return next }
	id := 11
	hash := strings.Repeat("d", 32)
	if err := s.SaveStorageConnection(context.Background(), nil, &id, &hash); err != nil {
		t.Fatal(err)
	}
	waitRuntime(t, next.started)
	if options.APIID != id || options.APIHash != hash || options.SessionFile != s.Cfg.Telegram.SessionFile || !bytes.Equal(options.SessionKey, s.Master.Raw()) || s.TGChatID() != 41 {
		t.Fatal("reconnect changed credentials/session/group incorrectly")
	}
	if s.UserTG.Status().AccountID != 77 {
		t.Fatal("account identity changed")
	}
	stopRuntime(t, r)
}

func TestWebGenericR2PublicTemporaryUsesItsOwnPrefix(t *testing.T) {
	s, _, mock := newTestEnv(t)
	s.Cfg.DevMock = false
	if err := s.initInstances(); err != nil {
		t.Fatal(err)
	}
	c := InstanceConfig{Endpoint: mock.Base, Bucket: s.Cfg.R2.TempBucket, Prefix: "separate-web-instance", AccessKey: "k", SecretKey: "s", TempPublicBase: "https://temporary.example.invalid"}
	id, err := s.SaveStorageInstance("", "带前缀临时桶", "r2", c, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	p := s.StoragePolicy()
	p.Temporary = id
	if err = s.SaveStoragePolicy(p); err != nil {
		t.Fatal(err)
	}
	f := archiveMedia(t, s, tinyMedia(t))
	ctx := context.Background()
	if err = s.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	obj, err := s.DB.LiveTempObject(f.ID, "original", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := s.presignTemp(ctx, obj)
	if err != nil || !strings.HasPrefix(u, c.TempPublicBase+"/"+c.Prefix+"/temp/tmp/v1/") {
		t.Fatal("wrong public prefix", err)
	}
	if err = s.deleteTemp(ctx, obj); err != nil {
		t.Fatal(err)
	}
	i, _ := s.instance(id)
	if _, err = i.hot.HeadSize(ctx, storage.Temporary, obj.ObjectKey); err == nil {
		t.Fatal("prefixed temporary object not deleted")
	}
}

func TestWebStorageEditsRetainOriginalAndLegacyTemporaryLocation(t *testing.T) {
	s, _, _ := newTestEnv(t)
	s.Cfg.DevMock = false
	if err := s.initInstances(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	f := archiveMedia(t, s, tinyMedia(t))
	local, _ := s.instance("local")
	if _, err := s.SaveStorageInstance("local", "已改名本地", "local", local.config, true); err != nil {
		t.Fatal(err)
	}
	c := local.config
	c.ColdDirectory = t.TempDir()
	if _, err := s.SaveStorageInstance("local", "错误目录", "local", c, true); err == nil {
		t.Fatal("referenced archive location changed")
	}
	r, err := s.SourceStream(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(b, tinyMedia(t)) {
		t.Fatal("original changed", err)
	}
	ctx := context.Background()
	key := "tmp/legacy-config-test"
	tmp, _ := s.instance(defaultR2Temporary)
	if err = tmp.hot.PutObject(ctx, storage.Temporary, key, strings.NewReader("legacy"), 6, "text/plain", ""); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.hot("r2")
	if err != nil {
		t.Fatal(err)
	}
	got, err := legacy.(storage.HotReader).OpenObject(ctx, storage.Temporary, key)
	if err != nil {
		t.Fatal(err)
	}
	b, err = io.ReadAll(got)
	got.Close()
	if err != nil || string(b) != "legacy" {
		t.Fatal("legacy temporary bucket changed", err)
	}
	if err = legacy.Delete(ctx, storage.Temporary, key); err != nil {
		t.Fatal(err)
	}
	if _, err = tmp.hot.HeadSize(ctx, storage.Temporary, key); err == nil {
		t.Fatal("legacy delete missed original bucket")
	}
}

func TestWebStorageConnectionSecretsAndValidation(t *testing.T) {
	s, _, _ := newTestEnv(t)
	proxy := "http://username:private-password@127.0.0.1:7890"
	if err := s.SaveStorageConnection(context.Background(), &proxy, nil, nil); err != nil {
		t.Fatal(err)
	}
	v := s.StorageConnectionView()
	if v["proxy_url"] != "" || v["has_proxy"] != true {
		t.Fatal("proxy credentials exposed")
	}
	id := 123
	hash := strings.Repeat("c", 32)
	if err := s.SaveStorageConnection(context.Background(), nil, &id, &hash); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if err := s.SaveStorageConnection(context.Background(), nil, &id, &empty); err != nil || s.storageConnection().APIHash != hash {
		t.Fatal("blank secret did not retain existing hash", err)
	}
	bad := "vless://private-secret@invalid"
	if err := s.SaveStorageConnection(context.Background(), &bad, nil, nil); err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("invalid proxy accepted or leaked", err)
	}
}
