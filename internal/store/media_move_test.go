package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"pimg/internal/storage"
	"testing"
)

func TestMediaTelegramMoveIncludesReplicaWhenPrimaryIsLocal(t *testing.T) {
	s, tg := workflowEnv(t)
	ctx := context.Background()
	album, _ := workflowBound(t, s, tg)
	i, _ := s.instance("local")
	c := i.config
	c.Priority = 20
	if _, err := s.SaveStorageInstance("local", i.row.Name, "local", c, true); err != nil {
		t.Fatal(err)
	}
	policy := s.StoragePolicy()
	policy.Archives = []string{"local", "telegram"}
	if err := s.SaveStoragePolicy(policy); err != nil {
		t.Fatal(err)
	}
	plain := workflowPNG(t)
	f, err := s.ReceiveUpload(ctx, bytes.NewReader(plain), "source.png", "png", sql.NullInt64{Int64: album, Valid: true}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchiveSync(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	f, _ = s.DB.GetFile(f.ID)
	if f.ColdBackend != "local" || !s.HasTelegramMedia(f) {
		t.Fatal("fixture did not retain TG alongside local")
	}
	target, err := s.EnsureTelegramAlbum(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.QueueTelegramMove(ctx, f.ID, target.AlbumID)
	if err != nil || task == 0 {
		t.Fatal("secondary TG was skipped", task, err)
	}
	if err = s.MoveTelegramFile(ctx, f.ID, target.AlbumID); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.FinishTask(task, "")
	f, _ = s.DB.GetFile(f.ID)
	if f.AlbumID.Int64 != target.AlbumID {
		t.Fatal("album not switched")
	}
	all, err := s.DB.MediaReplicas(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.InstanceID != "telegram" {
			continue
		}
		for _, p := range r.Parts {
			var ref storage.Reference
			if err = json.Unmarshal([]byte(p.Reference), &ref); err != nil {
				t.Fatal(err)
			}
			obj, err := s.telegramObject(ref.Key)
			if err != nil || obj.TopicID != target.TopicID {
				t.Fatal("secondary reference not switched", err)
			}
			input, err := s.openMediaPart(ctx, f, r, p)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(input)
			input.Close()
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatal("moved replica failed recovery", err)
			}
		}
	}
}
