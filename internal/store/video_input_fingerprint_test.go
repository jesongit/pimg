package store

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"pimg/internal/dbx"
)

func TestVideoFingerprintSurvivesSourceRemovalAndStoragePlanChange(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	path := filepath.Join(t.TempDir(), "source.mp4")
	args := []string{"-nostdin", "-y", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24", "-t", "4", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "12", "-b:v", "8M", "-minrate", "8M", "-maxrate", "8M", "-bufsize", "1M", "-x264-params", "nal-hrd=cbr:force-cfr=1", path}
	if output, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, output)
	}
	plain, err := os.ReadFile(path)
	if err != nil || len(plain) < 2<<20 {
		t.Fatal("fixture must require multiple 1 MiB parts", len(plain), err)
	}

	s, _, _ := peerFixture(t)
	s.SetDynamic(-1, -1, -1, []string{"mp4"})
	policy := s.StoragePolicy()
	policy.Archives = []string{"local"}
	if err = s.SaveStoragePolicy(policy); err != nil {
		t.Fatal(err)
	}
	local, err := s.instance("local")
	if err != nil {
		t.Fatal(err)
	}
	config := local.config
	config.Encrypt = true
	config.SplitVideo = true
	config.VideoPartMB = 1
	if _, err = s.SaveStorageInstance("local", local.row.Name, "local", config, true); err != nil {
		t.Fatal(err)
	}
	ctx := WithPrivateUpload(context.Background())
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "original.mp4", "mp4", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunDirectArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchiveSync(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	replicas, err := s.DB.MediaReplicas(f.ID)
	if err != nil || len(replicas) != 1 || len(replicas[0].Parts) < 2 {
		t.Fatal("video was not really split", replicas, err)
	}
	for _, part := range replicas[0].Parts {
		if part.Original || !part.Verified {
			t.Fatal("test must retain only verified independent parts", part)
		}
	}
	t.Logf("source bytes=%d, original SHA256=%s, verified independent parts=%d", len(plain), sha256Hex(plain), len(replicas[0].Parts))
	if _, err = os.Stat(s.plainPath(f.ID)); !os.IsNotExist(err) {
		t.Fatal("full source still present in upload workspace", err)
	}
	var inputHash string
	var inputSize int64
	if err = s.DB.QueryRow(`SELECT sha256,size FROM input_groups WHERE id=?`, f.ID).Scan(&inputHash, &inputSize); err != nil || inputHash != sha256Hex(plain) || inputSize != int64(len(plain)) {
		t.Fatal("original fingerprint not durable", inputHash, inputSize, err)
	}

	// Reconnect to the persisted index. No whole source copy is required.
	var seq int
	var dbName, dbPath string
	if err = s.DB.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbPath); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	s.DB, err = dbx.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DB.Close() })
	config.SplitVideo = false
	config.VideoPartMB = 2
	if _, err = s.SaveStorageInstance("local", local.row.Name, "local", config, true); err != nil {
		t.Fatal(err)
	}
	counts := func() map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, table := range []string{"files", "input_groups", "media_replicas", "media_parts", "tasks"} {
			var n int
			if err := s.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[table] = n
		}
		return out
	}
	before := counts()
	duplicate, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "renamed-after-restart.mp4", "mp4", sql.NullInt64{}, nil, 0, true)
	if err != nil || duplicate.ID != f.ID || !duplicate.UploadDuplicate {
		t.Fatal("same original was accepted as a new video", duplicate, err)
	}
	if !reflect.DeepEqual(before, counts()) {
		t.Fatal("duplicate added files, plans, parts, or work", before, counts())
	}
	after, err := s.DB.MediaReplicas(f.ID)
	if err != nil || !reflect.DeepEqual(replicas, after) {
		t.Fatal("duplicate changed existing segmentation", err)
	}
	if duplicate.SHA256 != inputHash || duplicate.Size != inputSize {
		t.Fatal("logical source identity was replaced by a part fingerprint")
	}

	// A different privacy request remains distinct even for identical bytes.
	private, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "encrypted-variant.mp4", "mp4", sql.NullInt64{}, nil, 0, false)
	if err != nil || private.ID == f.ID || private.UploadDuplicate {
		t.Fatal("different privacy intent incorrectly reused plaintext", private, err)
	}
}
