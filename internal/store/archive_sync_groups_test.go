package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"testing"

	"pimg/internal/dbx"
)

func TestSingleReadyArchiveDoesNotCreateEmptySyncOperation(t *testing.T) {
	s, _, _ := newTestEnv(t)
	if err := s.SetStorage("local", "local"); err != nil {
		t.Fatal(err)
	}
	policy := s.StoragePolicy()
	policy.Archives = []string{"local"}
	if err := s.SaveStoragePolicy(policy); err != nil {
		t.Fatal(err)
	}
	plain := tinyMedia(t)
	f, err := s.ReceiveUpload(context.Background(), bytes.NewReader(plain), "single.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	var jobs, groups int
	if err = s.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM tasks WHERE type='archive_sync'),(SELECT COUNT(*) FROM task_operations WHERE kind='sync')`).Scan(&jobs, &groups); err != nil || jobs != 0 || groups != 0 {
		t.Fatal("ready first copy created an empty sync task", jobs, groups, err)
	}
	if _, err = os.Stat(s.plainPath(f.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed upload work space leaked", err)
	}
	f, err = s.DB.GetFile(f.ID)
	if err != nil || f.ArchiveState != "done" {
		t.Fatal("first archive not complete", f, err)
	}
	reader, err := s.SourceStream(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("skipping empty sync lost the verified original", err)
	}
}

func TestMissingArchiveReplicaStillQueuesIndependentSync(t *testing.T) {
	s, _, plain := peerFixture(t)
	f, err := s.ReceiveUpload(context.Background(), bytes.NewReader(plain), "replicate.png", "png", sql.NullInt64{}, nil, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RunArchive(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	job, err := s.DB.FindActiveTask(dbx.TaskArchiveSync, f.ID)
	if err != nil || job.State != "pending" {
		t.Fatal("missing replicas did not queue synchronization", job, err)
	}
	first, err := s.DB.GetFile(f.ID)
	if err != nil || first.ArchiveState != "done" {
		t.Fatal("replication held first archive open", first, err)
	}
}
