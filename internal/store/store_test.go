package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pimg/internal/carrier"
	"pimg/internal/config"
	"pimg/internal/cryptox"
	"pimg/internal/dbx"
	"pimg/internal/devmock"
	"pimg/internal/r2x"
	"pimg/internal/tgx"
)

// newTestEnv 启动内存版 TG/R2 假服务并搭好 Service。
func newTestEnv(t *testing.T) (*Service, *devmock.TGServer, *devmock.S3Server) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tgM, err := devmock.StartTG(log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tgM.Close() })
	s3M, err := devmock.StartS3()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s3M.Close() })

	dir := t.TempDir()
	cfg := &config.Config{
		Storage: config.Storage{Cold: "telegram", Hot: "r2"},
		Telegram: config.Telegram{
			APIBase: tgM.Base, BotToken: "t", ChannelID: "-100x", ChunkMB: 1, // 1MB 分片，制造多分片
		},
		R2: config.R2{
			Endpoint: s3M.Base, AccessKeyID: "k", SecretAccessKey: "s",
			TempBucket: "temp-bkt", PermBucket: "perm-bkt",
			PublicBaseURL:  s3M.Base + "/perm-bkt",
			RetentionHours: 1, PresignMinutes: 5,
		},
		Upload:  config.Upload{MaxSizeMB: 64, MaxPixels: 40_000_000},
		Paths:   config.Paths{DataDir: dir, TempDir: filepath.Join(dir, "tmp")},
		DevMock: true,
	}
	cfg.Upload.AllowedExts = []string{"jpg", "png", "mp4"}
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m, err := cryptox.NewMaster(cryptox.GenerateMasterKey())
	if err != nil {
		t.Fatal(err)
	}
	tgc := tgx.New(cfg.Telegram.APIBase, cfg.Telegram.BotToken, cfg.Telegram.ChannelID)
	r2c := r2x.New(cfg.R2.Endpoint, "k", "s", "temp-bkt", "perm-bkt", cfg.R2.PublicBaseURL, "", "")
	svc, err := New(cfg, db, m, tgc, r2c, log)
	if err != nil {
		t.Fatal(err)
	}
	return svc, tgM, s3M
}

// bigPNG 生成 ~2.5MB 的噪点 PNG（Go 可解码，多分片归档）。
func bigPNG(t *testing.T) []byte {
	t.Helper()
	const w, h = 1100, 1500
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := 1
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = (seed*1103515245 + 12345) & 0x7FFFFFFF
			img.Set(x, y, color.RGBA{uint8(seed), uint8(seed >> 7), uint8(seed >> 13), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// unpublishedPlainArchive uses a real plaintext archive, then removes its
// automatic publication so subsequent tests exercise an actual publish write.
func unpublishedPlainArchive(t *testing.T, svc *Service, plain []byte) *dbx.File {
	t.Helper()
	ctx := context.Background()
	f, err := svc.receiveLegacyUploadFixture(ctx, bytes.NewReader(plain), "public.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RunDirectArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.RunUnpublish(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	f, err = svc.DB.GetFile(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFullLifecycle(t *testing.T) {
	svc, _, s3M := newTestEnv(t)
	ctx := context.Background()
	plain := bigPNG(t)

	// 1. 上传（流式接收 + 索引）
	f, err := svc.receiveLegacyUploadFixture(ctx, bytes.NewReader(plain), "旅行照片.png", "png", nullInt(0), nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.Size != int64(len(plain)) {
		t.Fatalf("记录大小 %d != 实际 %d", f.Size, len(plain))
	}
	if f.ArchiveState != "pending" {
		t.Fatalf("初始状态应为 pending: %s", f.ArchiveState)
	}

	// 2. 归档（加密 + TG 分片 + 缩略图）
	if err := svc.RunArchive(ctx, f.ID); err != nil {
		t.Fatal("归档失败:", err)
	}
	f2, _ := svc.DB.GetFile(f.ID)
	if f2.ArchiveState != "done" {
		t.Fatalf("归档状态: %s err=%s", f2.ArchiveState, f2.ArchiveError.String)
	}
	if !f2.HasThumb || f2.HasBThumb {
		t.Fatal("应只有本地缩略图，不生成独立模糊图")
	}
	if _, err := os.Stat(svc.plainPath(f.ID)); !os.IsNotExist(err) {
		t.Fatal("归档完成后明文临时文件必须删除")
	}
	chunks, _ := svc.DB.ListChunks(f.ID, "data")
	if len(chunks) != 1 {
		t.Fatalf("图片应保持一个完整归档文件, got %d", len(chunks))
	}
	for _, c := range chunks {
		if c.MessageID.Int64 == 0 || !c.TgFileID.Valid {
			t.Fatal("分片缺少 TG 定位信息")
		}
	}

	// 3. TG 恢复校验（不落盘，哈希须与原件一致）
	res, err := svc.Verify(ctx, f.ID)
	if err != nil {
		t.Fatal("恢复校验失败:", err, res)
	}

	// 4. 临时物化 + 预签名可访问
	if err := svc.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
		t.Fatal("临时物化失败:", err)
	}
	tv, err := svc.GetTempView(ctx, f2, "original")
	if err != nil || tv.Status != "ready" {
		t.Fatalf("临时查看: %+v err=%v", tv, err)
	}
	resp, err := http.Get(tv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, plain) {
		t.Fatalf("临时链接内容不一致 status=%d len=%d want=%d", resp.StatusCode, len(body), len(plain))
	}
	if resp.Header.Get("Cache-Control") != "private, max-age=0, no-store" {
		t.Fatalf("临时响应必须禁止持久缓存, got %q", resp.Header.Get("Cache-Control"))
	}
	if _, err := svc.GetTempView(ctx, f2, "thumb"); err == nil {
		t.Fatal("缩略图不应创建临时存储副本")
	}
	if preview, err := svc.PreviewThumbnail(ctx, f2); err != nil || len(preview) == 0 {
		t.Fatal("本地缩略图不可用", err)
	}

	// 5. 加密原件只能临时查看；永久发布使用独立的明文归档。
	if err := svc.RunPublish(ctx, f.ID, false); !errors.Is(err, dbx.ErrPermanentEncrypted) {
		t.Fatal("加密归档不得永久发布:", err)
	}
	publicFile := unpublishedPlainArchive(t, svc, plain)
	publicChunks, err := svc.DB.ListChunks(publicFile.ID, "data")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RunPublish(ctx, publicFile.ID, false); err != nil {
		t.Fatal("发布失败:", err)
	}
	publicFile, _ = svc.DB.GetFile(publicFile.ID)
	pub := svc.PublicURL(publicFile)
	resp2, err := http.Get(pub)
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || !bytes.Equal(body2, plain) {
		t.Fatalf("永久链接内容不一致 status=%d", resp2.StatusCode)
	}
	if preview, err := svc.PreviewThumbnail(ctx, publicFile); err != nil || len(preview) == 0 {
		t.Fatal("公开图片的本地封面不可用", err)
	}

	// 6. 撤销发布：永久对象删除；TG 原件仍在
	if err := svc.RunUnpublish(ctx, publicFile.ID); err != nil {
		t.Fatal("撤销失败:", err)
	}
	if r, _ := http.Get(pub); r.StatusCode != 404 {
		t.Fatalf("撤销后永久对象应 404, got %d", r.StatusCode)
	}
	chunksAfter, _ := svc.DB.ListChunks(publicFile.ID, "data")
	if len(chunksAfter) != len(publicChunks) {
		t.Fatal("撤销发布不得删除 TG 原件")
	}

	// 7. 导出 → 导入闭环
	var exp bytes.Buffer
	if err := svc.WriteExport(ctx, f2, &exp); err != nil {
		t.Fatal("导出失败:", err)
	}
	nf, err := svc.ImportPNG(exp.Bytes(), nullInt(0))
	if err != nil {
		t.Fatal("导入失败:", err)
	}
	hdr := parseImportPayload(t, svc, nf.ID)
	if err := svc.RunImport(ctx, nf.ID, hdr); err != nil {
		t.Fatal("导入归档失败:", err)
	}
	if _, err := svc.Verify(ctx, nf.ID); err != nil {
		t.Fatal("导入文件恢复校验失败:", err)
	}

	// 8. 彻底删除：记录消失、公开对象消失
	cancelFixtureReferences(t, svc, f.ID)
	if err := svc.RunPurgeFile(ctx, f.ID); err != nil {
		t.Fatal("彻底删除失败:", err)
	}
	if _, err := svc.DB.GetFile(f.ID); err == nil {
		t.Fatal("记录应已删除")
	}
	cancelFixtureReferences(t, svc, publicFile.ID)
	if err := svc.RunPurgeFile(ctx, publicFile.ID); err != nil {
		t.Fatal("明文归档删除失败:", err)
	}
	_ = s3M
}

func TestTempExpiryGate(t *testing.T) {
	// 验收要求：临时副本过期后不能再获得有效访问授权。
	svc, _, _ := newTestEnv(t)
	ctx := context.Background()
	f, err := svc.receiveLegacyUploadFixture(ctx, bytes.NewReader(bigPNG(t)), "x.png", "png", nullInt(0), nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	tv, err := svc.GetTempView(ctx, f, "original")
	if err != nil || tv.Status != "ready" {
		t.Fatalf("%+v %v", tv, err)
	}
	// 直接把副本行改成已过期（模拟时间流逝）
	svc.DB.Exec(`UPDATE temp_objects SET expires_at=? WHERE file_id=?`, time.Now().Add(-time.Minute).Unix(), f.ID)
	tv2, err := svc.GetTempView(ctx, f, "original")
	if err != nil {
		t.Fatal(err)
	}
	if tv2.Status != "pending" {
		t.Fatalf("过期后应重新物化而非复用旧链接: %+v", tv2)
	}
	// 旧 URL 签名期 ≤ 剩余保留期，且新副本必须使用新路径
	if err := svc.RunTempMaterialize(ctx, f.ID, "original"); err != nil {
		t.Fatal(err)
	}
	tv3, _ := svc.GetTempView(ctx, f, "original")
	if tv3.URL == tv.URL {
		t.Fatal("重新物化必须使用新的对象路径")
	}
}

func TestPublishRequiresArchive(t *testing.T) {
	svc, _, _ := newTestEnv(t)
	ctx := context.Background()
	plain := bigPNG(t)
	f, err := svc.receiveLegacyUploadFixture(ctx, bytes.NewReader(plain), "y.png", "png", nullInt(0), nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RunPublish(ctx, f.ID, false); err == nil {
		t.Fatal("未归档完成的文件不允许发布")
	}
}

func parseImportPayload(t *testing.T, svc *Service, fileID string) *carrier.Header {
	t.Helper()
	tk, err := svc.DB.FindActiveTask("import", fileID)
	if err != nil {
		t.Fatal(err)
	}
	h := &carrier.Header{}
	if err := json.Unmarshal([]byte(tk.Payload), h); err != nil {
		t.Fatal(err)
	}
	return h
}

func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v > 0} }

func TestHomeAlbumDirect(t *testing.T) {
	svc, _, _ := newTestEnv(t)
	ctx := context.Background()
	homeID, err := svc.DB.EnsureHomeAlbum()
	if err != nil || homeID == 0 {
		t.Fatalf("首页相册创建失败: %v", err)
	}
	plain := bigPNG(t)

	// 1. 直发上传：进首页相册 = 不加密、TG 明文存底、自动发布上首页
	f, err := svc.receiveLegacyUploadFixture(ctx, bytes.NewReader(plain), "公开直发.png", "png",
		sql.NullInt64{Int64: homeID, Valid: true}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if f.Storage != "direct" {
		t.Fatalf("应为 direct 存储: %s", f.Storage)
	}
	if err := svc.RunDirectArchive(ctx, f.ID); err != nil {
		t.Fatal("直发归档失败:", err)
	}
	f2, _ := svc.DB.GetFile(f.ID)
	if f2.ArchiveState != "done" || !f2.Published {
		t.Fatalf("直发后应归档完成+已发布: %+v", f2)
	}
	// 公开 URL 直读 == 原件
	resp, err := http.Get(svc.PublicURL(f2))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, plain) {
		t.Fatalf("直发公开对象不一致 status=%d len=%d", resp.StatusCode, len(body))
	}
	// 明文存底：分片存在且母本流 == 原件（无加密层）
	chunks, _ := svc.DB.ListChunks(f.ID, "data")
	if len(chunks) == 0 {
		t.Fatal("直发文件必须有 TG 分片（明文）")
	}
	rc, err := svc.SourceStream(ctx, f2)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, plain) {
		t.Fatal("TG 明文母本与原件不一致")
	}
	if _, err := svc.Verify(ctx, f.ID); err != nil {
		t.Fatal("直发文件校验失败:", err)
	}
	// 首页可见（首页=游客相册中已发布的文件）
	home, _, _ := svc.DB.ListHome(homeID, 1, 20)
	found := false
	for _, h := range home {
		if h.ID == f.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("直发文件应出现在游客首页")
	}

	// 2. 移出游客相册：不在首页，但外链仍有效；TG 存底保留
	svc.DB.UpdateFileMeta(f.ID, "", sql.NullInt64{Valid: true}, nil)
	home2, _, _ := svc.DB.ListHome(homeID, 1, 20)
	for _, h := range home2 {
		if h.ID == f.ID {
			t.Fatal("移出游客相册后不应出现在首页")
		}
	}
	if r, _ := http.Get(svc.PublicURL(f2)); r.StatusCode != 200 {
		t.Fatalf("移出游客相册后外链应仍有效, got %d", r.StatusCode)
	}

	// 3. 撤销发布：公开对象删除，TG 存底保留
	if err := svc.RunUnpublish(ctx, f.ID); err != nil {
		t.Fatal("撤销失败:", err)
	}
	if r, _ := http.Get(svc.PublicURL(f2)); r.StatusCode != 404 {
		t.Fatalf("撤销后公开对象应 404, got %d", r.StatusCode)
	}
	chunksAfter, _ := svc.DB.ListChunks(f.ID, "data")
	if len(chunksAfter) != len(chunks) {
		t.Fatal("撤销公开不得删除 TG 存底")
	}

	// 4. 明文归档：单独发布后移入游客相册才上首页。
	pf := unpublishedPlainArchive(t, svc, plain)
	if err := svc.RunPublish(ctx, pf.ID, false); err != nil {
		t.Fatal("发布失败:", err)
	}
	// 发布但不在游客相册 → 不上首页
	home3, _, _ := svc.DB.ListHome(homeID, 1, 20)
	for _, h := range home3 {
		if h.ID == pf.ID {
			t.Fatal("未加入游客相册的已发布文件不应上首页")
		}
	}
	// 移入游客相册 → 上首页
	svc.DB.UpdateFileMeta(pf.ID, "", sql.NullInt64{Int64: homeID, Valid: true}, nil)
	home4, _, _ := svc.DB.ListHome(homeID, 1, 20)
	onHome := false
	for _, h := range home4 {
		if h.ID == pf.ID {
			onHome = true
		}
	}
	if !onHome {
		t.Fatal("移入游客相册后应上首页")
	}
	pf2, _ := svc.DB.GetFile(pf.ID)
	if !pf2.Published {
		t.Fatal("应已发布")
	}
	if pf2.Storage != "direct" {
		t.Fatal("移入游客相册不得改变 TG 明文存底")
	}
	if _, err := svc.Verify(ctx, pf.ID); err != nil {
		t.Fatal("移入游客相册后校验失败:", err)
	}
}
