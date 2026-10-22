package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pimg/internal/dbx"
)

// The family is made of small independently archived media fixtures. Codec
// splitting is tested by the upload layer; these tests exercise the receipts.
func workflowSourceFamily(t *testing.T) (*Service, *workflowTG, int64, int, *dbx.File, *dbx.File) {
	t.Helper()
	s, tg := workflowEnv(t)
	album, topic := workflowBound(t, s, tg)
	destination, err := s.EnsureTelegramAlbum(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	a := workflowArchivedFile(t, s, destination.AlbumID)
	b, err := s.receiveLegacyTelegramFixture(context.Background(), append(workflowPNG(t), 2), "part-2.png", "image/png", destination.AlbumID, "")
	if err != nil {
		t.Fatal(err)
	}
	tg.add(topic, 80, 1, "source.mp4", "video/mp4", "", []byte("original source"))
	ref, _ := json.Marshal(tg.messages[80].Reference)
	if _, err = s.DB.Exec(`UPDATE files SET telegram_source='11:77:80' WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`INSERT INTO telegram_source_parts(source_key,part_index,file_id) VALUES('11:77:80',0,?),('11:77:80',1,?)`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.UpsertTelegramMessage(&dbx.TelegramMessage{AccountID: 11, ChatID: 77, TopicID: topic, MessageID: 80, Date: 1, Reference: string(ref), Name: "source.mp4", MIME: "video/mp4", State: "done", FileID: a.ID}); err != nil {
		t.Fatal(err)
	}
	return s, tg, album, topic, a, b
}

func TestTelegramDeleteChecksEverySourcePartBeforeRemovingTopic(t *testing.T) {
	s, tg, album, _, _, second := workflowSourceFamily(t)
	if _, err := s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err == nil || !strings.Contains(err.Error(), second.ID) || !strings.Contains(err.Error(), "尚未归档") {
		t.Fatal("ready first part hid pending second part", err)
	}
	if tg.deleteTopicCalls != 0 {
		t.Fatal("complete source deleted before all independent parts verified")
	}
	if err := s.RunArchive(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if tg.deleteTopicCalls != 1 {
		t.Fatal("verified family did not finish deletion")
	}
}

func TestTelegramDeleteRetiredFirstPartStillChecksRemainingFamily(t *testing.T) {
	s, tg, album, _, first, second := workflowSourceFamily(t)
	cancelFixtureReferences(t, s, first.ID)
	if err := s.RunPurgeFile(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	message, _ := s.DB.GetTelegramMessage(11, 77, 80)
	if message.State != "retired" {
		t.Fatal("fixture source was not retired")
	}
	if _, err := s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err == nil || !strings.Contains(err.Error(), second.ID) {
		t.Fatal("retired first part hid remaining pending part", err)
	}
	if tg.deleteTopicCalls != 0 {
		t.Fatal("remaining part lost original source")
	}
	if err := s.RunArchive(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal("explicitly purged first part should not prevent remaining verified family", err)
	}
}

func TestTelegramDeleteFollowsSourcePartConversionChainAndWaitsForStaging(t *testing.T) {
	s, tg, album, _, first, second := workflowSourceFamily(t)
	if err := s.RunArchive(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	c, err := s.QueueConversion(context.Background(), first.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.RunConversion(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plain != c.TargetID {
		t.Fatal("unexpected conversion target")
	}
	c2, err := s.QueueConversion(context.Background(), plain, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if err = s.RunDeleteLinkedAlbum(context.Background(), album); err == nil {
		t.Fatal("unfinished conversion did not delay source deletion")
	}
	if tg.deleteTopicCalls != 0 {
		t.Fatal("staging conversion lost source")
	}
	if _, err = s.RunConversion(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	// Simulate an older receipt pointing to the original ID rather than the
	// current one, to exercise more than one completed conversion hop.
	if _, err = s.DB.Exec(`UPDATE telegram_source_parts SET file_id=? WHERE source_key='11:77:80' AND part_index=0`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.resolveTelegramSourceFile(first.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RunDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal("completed chain could not be verified", err)
	}
	target, _ := s.DB.GetFile(c2.TargetID)
	if target.Hidden {
		t.Fatal("resolved target unexpectedly hidden")
	}
}

func TestTelegramDeleteMissingUnpurgedPartRetainsSource(t *testing.T) {
	s, tg, album, _, _, second := workflowSourceFamily(t)
	// An unexplained missing row is not equivalent to the user purging a part.
	if _, err := s.DB.Exec(`DELETE FROM files WHERE id=?`, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueueDeleteLinkedAlbum(context.Background(), album); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDeleteLinkedAlbum(context.Background(), album); err == nil || !strings.Contains(err.Error(), second.ID) {
		t.Fatal("missing part was treated as an intentional purge", err)
	}
	if tg.deleteTopicCalls != 0 {
		t.Fatal("unexplained missing part lost source")
	}
}
