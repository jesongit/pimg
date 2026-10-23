package tguser

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func fakeClient(invoke telegram.InvokeFunc) *Client {
	c := New(Options{APIID: 1, APIHash: "fixture"})
	c.api = tg.NewClient(c.middleware(invoke))
	c.auth = auth.NewClient(c.api, rand.Reader, 1, "fixture")
	c.connected = true
	c.status = Status{Configured: true, State: "authorized", AccountID: 77, UploadLimitBytes: defaultUploadLimit}
	c.channels[41] = &tg.Channel{ID: 41, Title: "fixture", AccessHash: 412, Forum: true, Megagroup: true, Creator: true}
	return c
}

func docMessage(chat int64, id, topic int, data []byte, reference string) *tg.Message {
	return &tg.Message{ID: id, PeerID: &tg.PeerChannel{ChannelID: chat}, Date: 1500, ReplyTo: &tg.MessageReplyHeader{ForumTopic: true, ReplyToTopID: topic}, Message: "app:pimg fixture", Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 81, AccessHash: 82, FileReference: []byte(reference), DCID: 2, MimeType: "image/png", Size: int64(len(data)), Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "fixture.pimg.png"}}}}}
}

func TestUserUploadPreservesDocumentBytesAndStableRandomID(t *testing.T) {
	payload := bytes.Repeat([]byte("png-visible-and-encrypted-trailer"), 10000)
	thumb := []byte("jpeg-blur-preview")
	parts := map[int64]map[int][]byte{}
	sends := 0
	events := 0
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.UploadSaveFilePartRequest:
			if parts[req.FileID] == nil {
				parts[req.FileID] = map[int][]byte{}
			}
			parts[req.FileID][req.FilePart] = append([]byte(nil), req.Bytes...)
			out.(*tg.BoolBox).Bool = &tg.BoolTrue{}
		case *tg.MessagesSendMediaRequest:
			sends++
			if req.RandomID != 123456 {
				t.Fatalf("stable operation id lost: %d", req.RandomID)
			}
			media, ok := req.Media.(*tg.InputMediaUploadedDocument)
			if !ok || !media.ForceFile || media.MimeType != "image/png" {
				t.Fatalf("document bytes may be transformed: %#v", req.Media)
			}
			if req.ReplyTo.(*tg.InputReplyToMessage).ReplyToMsgID != 7 {
				t.Fatal("wrong destination topic")
			}
			join := func(id int64) []byte {
				var indexes []int
				for i := range parts[id] {
					indexes = append(indexes, i)
				}
				sort.Ints(indexes)
				var data []byte
				for _, i := range indexes {
					data = append(data, parts[id][i]...)
				}
				return data
			}
			if !bytes.Equal(join(media.File.(*tg.InputFile).ID), payload) || !bytes.Equal(join(media.Thumb.(*tg.InputFile).ID), thumb) {
				t.Fatal("document or preview bytes changed")
			}
			out.(*tg.UpdatesBox).Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: docMessage(41, 9, 7, payload, "fresh")}}}
		default:
			t.Fatalf("unexpected RPC %T", in)
		}
		return nil
	})
	c.opts.OnMessage = func(m Message) {
		events++
		if m.Reference.MessageID != 9 || m.MIME != "image/png" {
			t.Fatal(m)
		}
	}
	for i := 0; i < 2; i++ {
		ref, err := c.Upload(context.Background(), 41, 7, "fixture.pimg.png", "image/png", payload, thumb, "app:pimg fixture", 123456)
		if err != nil || ref.AccountID != 77 || ref.ChatID != 41 || ref.MessageID != 9 || ref.TopicID != 7 || ref.Size != int64(len(payload)) {
			t.Fatalf("upload failed: %#v %v", ref, err)
		}
	}
	if sends != 2 || events != 2 {
		t.Fatalf("missing upload/normalized callback: %d %d", sends, events)
	}
}

func TestUserVideoUploadAddsPlaybackMetadataWithoutChangingOriginal(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			payload := bytes.Repeat([]byte("original-video-container-bytes"), 21000)
			thumb := []byte("jpeg-blurred-video-cover")
			parts := map[int64]map[int][]byte{}
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch req := in.(type) {
				case *tg.UploadSaveFilePartRequest:
					if parts[req.FileID] == nil {
						parts[req.FileID] = map[int][]byte{}
					}
					parts[req.FileID][req.FilePart] = append([]byte(nil), req.Bytes...)
					out.(*tg.BoolBox).Bool = &tg.BoolTrue{}
				case *tg.MessagesSendMediaRequest:
					media, ok := req.Media.(*tg.InputMediaUploadedDocument)
					if !ok || media.ForceFile || !media.NosoundVideo || media.MimeType != "video/mp4" || req.RandomID != 8899 {
						t.Fatalf("video display/operation id lost: %#v", req)
					}
					var attr *tg.DocumentAttributeVideo
					var filename string
					for _, a := range media.Attributes {
						switch a := a.(type) {
						case *tg.DocumentAttributeVideo:
							attr = a
						case *tg.DocumentAttributeFilename:
							filename = a.FileName
						}
					}
					if attr == nil || attr.Duration != 12.375 || attr.W != 1920 || attr.H != 1080 || attr.SupportsStreaming != streaming || filename != "original-part-001.mp4" {
						t.Fatalf("actual segment metadata lost: %#v %q", attr, filename)
					}
					// Check optional protocol flags survive the generated wire encoder,
					// including the false streaming case and optional thumbnail.
					var encoded bin.Buffer
					if err := media.Encode(&encoded); err != nil {
						t.Fatal(err)
					}
					var decoded tg.InputMediaUploadedDocument
					if err := decoded.Decode(&encoded); err != nil {
						t.Fatal(err)
					}
					if decoded.ForceFile || !decoded.NosoundVideo || decoded.Attributes[1].(*tg.DocumentAttributeVideo).SupportsStreaming != streaming || decoded.Thumb == nil {
						t.Fatal("video metadata changed on wire")
					}
					join := func(id int64) []byte {
						var indexes []int
						for i := range parts[id] {
							indexes = append(indexes, i)
						}
						sort.Ints(indexes)
						var data []byte
						for _, i := range indexes {
							data = append(data, parts[id][i]...)
						}
						return data
					}
					if !bytes.Equal(join(media.File.(*tg.InputFile).ID), payload) || !bytes.Equal(join(media.Thumb.(*tg.InputFile).ID), thumb) {
						t.Fatal("video or cover was re-encoded")
					}
					message := docMessage(41, 9, 7, payload, "r")
					message.Media.(*tg.MessageMediaDocument).Document.(*tg.Document).MimeType = "video/mp4"
					out.(*tg.UpdatesBox).Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: message}}}
				default:
					t.Fatalf("unexpected RPC %T", in)
				}
				return nil
			})
			ref, err := c.UploadVideo(context.Background(), 41, 7, "original-part-001.mp4", "video/mp4", payload, thumb, "app:pimg video", 8899, VideoMetadata{Duration: 12.375, Width: 1920, Height: 1080, SupportsStreaming: streaming})
			if err != nil || ref.MessageID != 9 || ref.Size != int64(len(payload)) {
				t.Fatal(ref, err)
			}
		})
	}
}

func TestUserVideoUploadRejectsMissingMetadataBeforeProviderCall(t *testing.T) {
	calls := 0
	c := fakeClient(func(context.Context, bin.Encoder, bin.Decoder) error { calls++; return errors.New("must not call") })
	valid := VideoMetadata{Duration: 1, Width: 640, Height: 360, SupportsStreaming: true}
	for _, meta := range []VideoMetadata{{}, {Duration: math.NaN(), Width: 640, Height: 360}, {Duration: math.Inf(1), Width: 640, Height: 360}, {Duration: -1, Width: 640, Height: 360}, {Duration: 1, Width: 0, Height: 360}, {Duration: 1, Width: 640, Height: -1}} {
		if _, err := c.UploadVideo(context.Background(), 41, 7, "v.mp4", "video/mp4", []byte("video"), nil, "", 1, meta); err == nil {
			t.Fatal("invalid video metadata accepted", meta)
		}
	}
	for _, mime := range []string{"image/png", "application/octet-stream", "video/"} {
		if _, err := c.UploadVideo(context.Background(), 41, 7, "v.mp4", mime, []byte("video"), nil, "", 1, valid); err == nil {
			t.Fatal("wrong MIME accepted", mime)
		}
	}
	if calls != 0 {
		t.Fatal("invalid metadata uploaded bytes", calls)
	}
}

func TestUserDownloadRefreshesSourceAndExpiredFileReference(t *testing.T) {
	payload := bytes.Repeat([]byte("raw-PNG-carrier-cipher"), 64000)
	reads, downloads := 0, 0
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.ChannelsGetMessagesRequest:
			reads++
			if req.Channel.(*tg.InputChannel).ChannelID != 41 || req.ID[0].(*tg.InputMessageID).ID != 9 {
				t.Fatal("source message lost")
			}
			out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{docMessage(41, 9, 7, payload, fmt.Sprintf("fresh-%d", reads))}}
		case *tg.UploadGetFileRequest:
			downloads++
			loc := req.Location.(*tg.InputDocumentFileLocation)
			if bytes.Equal(loc.FileReference, []byte("stale-database-reference")) {
				t.Fatal("stale persisted reference reused")
			}
			if reads == 1 {
				return tgerr.New(400, "FILE_REFERENCE_EXPIRED")
			}
			if string(loc.FileReference) != "fresh-2" {
				t.Fatal("expired source was not refreshed")
			}
			start := int(req.Offset)
			end := start + req.Limit
			if start > len(payload) {
				start = len(payload)
			}
			if end > len(payload) {
				end = len(payload)
			}
			out.(*tg.UploadFileBox).File = &tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: payload[start:end]}
		default:
			t.Fatalf("unexpected RPC %T", in)
		}
		return nil
	})
	ref := Reference{AccountID: 77, ChatID: 41, TopicID: 7, MessageID: 9, DocumentID: 81, FileReference: []byte("stale-database-reference"), Size: int64(len(payload))}
	data, err := c.Download(context.Background(), ref)
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("download bytes differ: %d %v", len(data), err)
	}
	if reads != 2 || downloads < 3 {
		t.Fatalf("refresh/multiple transfer chunks not exercised: %d %d", reads, downloads)
	}
	ref.AccountID = 999
	if _, err := c.Download(context.Background(), ref); !errors.Is(err, ErrAccountMismatch) {
		t.Fatal("wrong account allowed", err)
	}
	if err := c.Delete(context.Background(), ref); !errors.Is(err, ErrAccountMismatch) {
		t.Fatal("wrong account delete allowed", err)
	}
	if reads != 2 {
		t.Fatal("wrong account contacted provider")
	}
}

func TestUserDownloadRejectsReplacedOrMissingSource(t *testing.T) {
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		if _, ok := in.(*tg.ChannelsGetMessagesRequest); !ok {
			t.Fatalf("unexpected download RPC %T", in)
		}
		out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{docMessage(41, 9, 7, []byte("content"), "current")}}
		return nil
	})
	_, err := c.Download(context.Background(), Reference{AccountID: 77, ChatID: 41, MessageID: 9, DocumentID: 999})
	if err == nil {
		t.Fatal("edited source accepted")
	}
	_, err = c.Download(context.Background(), Reference{AccountID: 77, ChatID: 41, MessageID: 100})
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("missing source accepted", err)
	}
}

func TestUserHistoryKeepsUnmanagedTextServicesAndDates(t *testing.T) {
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		req := in.(*tg.MessagesGetRepliesRequest)
		if req.MsgID != 7 || req.OffsetDate != 2001 || req.Limit != 100 {
			t.Fatalf("incorrect scan bounds: %#v", req)
		}
		image := docMessage(41, 18, 7, []byte("png"), "r")
		text := docMessage(41, 19, 7, nil, "")
		text.Media = nil
		text.Message = "unmanaged ordinary text"
		text.Date = 1900
		old := docMessage(41, 16, 7, nil, "")
		old.Date = 999
		future := docMessage(41, 20, 7, nil, "")
		future.Date = 2200
		service := &tg.MessageService{ID: 17, Date: 1200, PeerID: &tg.PeerChannel{ChannelID: 41}, ReplyTo: &tg.MessageReplyHeader{ForumTopic: true, ReplyToTopID: 7}, Action: &tg.MessageActionTopicEdit{Title: "changed"}}
		out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{future, text, image, service, old}}
		return nil
	})
	page, err := c.History(context.Background(), 41, 7, time.Unix(1000, 0), time.Unix(2000, 0), 0, 500)
	if err != nil || !page.Done || page.NextOffset != 16 || len(page.Messages) != 3 {
		t.Fatalf("bad page: %#v %v", page, err)
	}
	if page.Messages[0].MIME != "" || page.Messages[0].Caption != "unmanaged ordinary text" || !page.Messages[2].IsService {
		t.Fatal("nonmedia deleted from scan", page.Messages)
	}
}

func TestUserCopyDeleteUseRecordedSourceAndStableRandom(t *testing.T) {
	copies, deletes := 0, 0
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.ChannelsGetMessagesRequest:
			out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{docMessage(41, 9, 7, []byte("png"), "r")}}
		case *tg.MessagesForwardMessagesRequest:
			copies++
			if req.RandomID[0] != 4455 || req.ID[0] != 9 || req.TopMsgID != 8 || !req.DropAuthor {
				t.Fatalf("copy lost destination/idempotency: %#v", req)
			}
			out.(*tg.UpdatesBox).Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: docMessage(41, 99, 8, []byte("png"), "new")}}}
		case *tg.ChannelsDeleteMessagesRequest:
			deletes++
			if req.Channel.(*tg.InputChannel).ChannelID != 41 || len(req.ID) != 1 || req.ID[0] != 9 {
				t.Fatal("deleted wrong source")
			}
		default:
			t.Fatalf("unexpected RPC %T", in)
		}
		return nil
	})
	ref := Reference{AccountID: 77, ChatID: 41, TopicID: 7, MessageID: 9, DocumentID: 81, Size: 3}
	copy, err := c.Copy(context.Background(), ref, 41, 8, 4455)
	if err != nil || copy.MessageID != 99 || copy.TopicID != 8 {
		t.Fatal(copy, err)
	}
	if err := c.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if copies != 1 || deletes != 1 {
		t.Fatal(copies, deletes)
	}
}

func TestUserAuthFlowKeepsCodeAndPasswordOutOfSessionState(t *testing.T) {
	passwordRequired := false
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.AuthSendCodeRequest:
			if req.PhoneNumber != "+123456789" {
				t.Fatal("wrong phone")
			}
			out.(*tg.AuthSentCodeBox).SentCode = &tg.AuthSentCode{PhoneCodeHash: "hash-only-in-memory"}
		case *tg.AuthSignInRequest:
			if req.PhoneCode != "12345" || req.PhoneCodeHash != "hash-only-in-memory" {
				t.Fatal("wrong login flow")
			}
			if passwordRequired {
				return tgerr.New(401, "SESSION_PASSWORD_NEEDED")
			}
			out.(*tg.AuthAuthorizationBox).Authorization = &tg.AuthAuthorization{User: &tg.User{ID: 77, FirstName: "Fixture"}}
		case *tg.AccountGetPasswordRequest:
			out.(*tg.AccountPassword).Hint = "fixture hint"
		default:
			t.Fatalf("unexpected login RPC %T", in)
		}
		return nil
	})
	c.status.State = "phone"
	c.status.AccountID = 0
	if err := c.AuthPhone(context.Background(), "+123456789"); err != nil || c.Status().State != "code" {
		t.Fatal(c.Status(), err)
	}
	if err := c.AuthCode(context.Background(), "12345"); err != nil || c.Status().State != "authorized" || c.phone != "" || c.codeHash != "" {
		t.Fatal(c.Status(), err)
	}
	c.status.State = "phone"
	passwordRequired = true
	if err := c.AuthPhone(context.Background(), "+123456789"); err != nil {
		t.Fatal(err)
	}
	if err := c.AuthCode(context.Background(), "12345"); err != nil || c.Status().State != "password" || c.Status().PasswordHint != "fixture hint" {
		t.Fatal(c.Status(), err)
	}
}

func TestUserUploadLimitFromAccountAppConfig(t *testing.T) {
	for _, premium := range []bool{false, true} {
		t.Run(fmt.Sprint(premium), func(t *testing.T) {
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				if _, ok := in.(*tg.HelpGetAppConfigRequest); !ok {
					t.Fatalf("unexpected RPC %T", in)
				}
				out.(*tg.HelpAppConfigBox).AppConfig = &tg.HelpAppConfig{Config: &tg.JSONObject{Value: []tg.JSONObjectValue{{Key: "upload_max_fileparts_default", Value: &tg.JSONNumber{Value: 4000}}, {Key: "upload_max_fileparts_premium", Value: &tg.JSONNumber{Value: 8000}}}}}
				return nil
			})
			c.refreshUploadLimit(context.Background(), c.api, 77, premium)
			want := int64(4000 * 512 * 1024)
			if premium {
				want *= 2
			}
			if c.Status().UploadLimitBytes != want {
				t.Fatal(c.Status())
			}
			c.status.UploadLimitBytes = 2
			if _, err := c.Upload(context.Background(), 41, 7, "f.png", "image/png", []byte("123"), nil, "", 1); err == nil || !strings.Contains(err.Error(), "3 字节") || !strings.Contains(err.Error(), "2 字节") {
				t.Fatal("wire payload limit not enforced", err)
			}
		})
	}
}

func TestUserLogoutClearsOnlyIndependentSession(t *testing.T) {
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		if _, ok := in.(*tg.AuthLogOutRequest); !ok {
			t.Fatalf("unexpected RPC %T", in)
		}
		return nil
	})
	path := filepath.Join(t.TempDir(), "user.session")
	s, err := newSessionStorage(path, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	c.store = s
	if err := s.StoreSession(context.Background(), []byte("private-session")); err != nil {
		t.Fatal(err)
	}
	if err := c.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Status().State != "phone" || c.Status().AccountID != 0 {
		t.Fatal(c.Status())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("session survived logout", err)
	}
	if err := s.StoreSession(context.Background(), []byte("late-save")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("late save revived logout", err)
	}
}

func TestUserFloodWaitCancelAndSanitizedErrors(t *testing.T) {
	c := New(Options{})
	calls := 0
	run := c.middleware(telegram.InvokeFunc(func(context.Context, bin.Encoder, bin.Decoder) error {
		calls++
		return tgerr.New(420, "FLOOD_WAIT_3600")
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := run(ctx, &tg.HelpGetConfigRequest{}, &tg.Config{}); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second || calls != 1 {
		t.Fatal("flood wait not cancellable", err, calls)
	}
	err := c.safeError(errors.New("socks5://user:secret@proxy.invalid +123456789 code=12345"))
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "12345") {
		t.Fatal("upstream secret leaked", err)
	}
	c.status.State = "authorized"
	c.safeError(tgerr.New(401, "SESSION_REVOKED"))
	if c.Status().State != "reauth_required" {
		t.Fatal("session revocation not surfaced")
	}
}

func TestUserMissingConfigDoesNotConnect(t *testing.T) {
	c := New(Options{})
	if c.Status().State != "unconfigured" {
		t.Fatal(c.Status())
	}
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("unconfigured client started")
	}
	c = New(Options{APIID: 1, APIHash: "fixture"})
	if err := c.Run(context.Background()); err == nil || c.Status().State != "error" {
		t.Fatal("invalid session storage started a connection", err)
	}
	for _, u := range []string{"ftp://user:secret@proxy", "http://", "http://proxy/path", "http://proxy?secret=1"} {
		if _, err := proxyDial(u); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid or leaking proxy", u, err)
		}
	}
}

func TestUserGroupDiscoveryFiltersPermissionsAndSupportsArchivedFolder(t *testing.T) {
	folders := []int{}
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		req := in.(*tg.MessagesGetDialogsRequest)
		folders = append(folders, req.FolderID)
		chats := []tg.ChatClass{&tg.Channel{ID: 41, Title: "Allowed", AccessHash: 412, Megagroup: true, Forum: true, AdminRights: tg.ChatAdminRights{ManageTopics: true, DeleteMessages: true}}, &tg.Channel{ID: 42, Title: "Read only", AccessHash: 422, Megagroup: true, Forum: true}, &tg.Channel{ID: 43, Title: "Broadcast channel", AccessHash: 432, Broadcast: true, Creator: true}}
		if req.FolderID == 1 {
			chats = []tg.ChatClass{&tg.Channel{ID: 44, Title: "Archived forum", AccessHash: 442, Megagroup: true, Forum: true, Creator: true}}
		}
		out.(*tg.MessagesDialogsBox).Dialogs = &tg.MessagesDialogs{Chats: chats}
		return nil
	})
	groups, err := c.Groups(context.Background())
	if err != nil || len(groups) != 2 || len(folders) != 2 || folders[0] != 0 || folders[1] != 1 {
		t.Fatal(groups, folders, err)
	}
	if _, err := c.channel(context.Background(), 42); err == nil {
		t.Fatal("read-only forum selected for archive")
	}
}

func TestUserTopicMutationsUseExplicitFieldsAndDeleteContinuation(t *testing.T) {
	deletes := 0
	edits := 0
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.MessagesGetForumTopicsRequest:
			if req.OffsetTopic == 0 {
				out.(*tg.MessagesForumTopics).Count = 2
				out.(*tg.MessagesForumTopics).Topics = []tg.ForumTopicClass{&tg.ForumTopic{ID: 7, Title: "First", TopMessage: 70, Date: 700}}
			} else {
				if req.OffsetTopic != 7 || req.OffsetID != 70 {
					t.Fatal(req)
				}
				out.(*tg.MessagesForumTopics).Count = 2
				out.(*tg.MessagesForumTopics).Topics = []tg.ForumTopicClass{&tg.ForumTopic{ID: 8, Title: "Second", Closed: true}}
			}
		case *tg.MessagesCreateForumTopicRequest:
			if req.Title != "New" || req.RandomID == 0 {
				t.Fatal(req)
			}
			out.(*tg.UpdatesBox).Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: &tg.MessageService{ID: 9, Action: &tg.MessageActionTopicCreate{Title: "New"}}}}}
		case *tg.MessagesEditForumTopicRequest:
			edits++
			if edits == 1 {
				if title, ok := req.GetTitle(); !ok || title != "Renamed" {
					t.Fatal("rename flag missing")
				}
			} else {
				if closed, ok := req.GetClosed(); !ok || closed {
					t.Fatal("false closed flag was dropped")
				}
			}
		case *tg.MessagesDeleteTopicHistoryRequest:
			deletes++
			if req.TopMsgID != 7 {
				t.Fatal(req)
			}
			if deletes == 1 {
				out.(*tg.MessagesAffectedHistory).Offset = 1
			}
		default:
			t.Fatalf("unexpected RPC %T", in)
		}
		return nil
	})
	topics, err := c.Topics(context.Background(), 41)
	if err != nil || len(topics) != 2 || !topics[1].Closed {
		t.Fatal(topics, err)
	}
	topic, err := c.CreateTopic(context.Background(), 41, "New")
	if err != nil || topic.ID != 9 {
		t.Fatal(topic, err)
	}
	if err := c.RenameTopic(context.Background(), 41, 7, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseTopic(context.Background(), 41, 7, false); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteTopic(context.Background(), 41, 7); err != nil || deletes != 2 {
		t.Fatal(deletes, err)
	}
	if err := c.DeleteTopic(context.Background(), 41, 1); err == nil {
		t.Fatal("General topic delete allowed")
	}
}

func TestUserAccountChangeRequiresExplicitLogoutAndDropsSessionScopedPeers(t *testing.T) {
	c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		if _, ok := in.(*tg.AuthLogOutRequest); !ok {
			t.Fatalf("unexpected RPC %T", in)
		}
		return tgerr.New(401, "SESSION_REVOKED")
	})
	if err := c.authorized(&tg.User{ID: 999, FirstName: "Other"}); !errors.Is(err, ErrAccountMismatch) || c.Status().State != "reauth_required" || c.Status().AccountID != 77 {
		t.Fatal("account silently replaced", c.Status(), err)
	}
	if _, _, err := c.ready(); err == nil {
		t.Fatal("mismatched account still served old archives")
	}
	if err := c.Logout(context.Background()); err != nil || c.Status().State != "phone" || len(c.channels) != 0 {
		t.Fatal("revoked/mismatched session could not be cleared", c.Status(), err)
	}
}
