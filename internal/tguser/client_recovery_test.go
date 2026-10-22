package tguser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func recoveryCaption(random int64) string {
	op := ""
	if random != 0 {
		op = strconv.FormatInt(random, 16)
	}
	b, _ := json.Marshal(pimgCaption{App: "pimg", V: 1, File: "file-recovery", Kind: "data", Index: 0, SHA256: strings.Repeat("ab", 32), Op: op})
	return string(b)
}

func recoveryDocument(id, topic int, caption string, size, documentID int64) *tg.Message {
	m := docMessage(41, id, topic, nil, "fresh-recovered-reference")
	m.Message = caption
	doc := m.Media.(*tg.MessageMediaDocument).Document.(*tg.Document)
	doc.Size, doc.ID = size, documentID
	return m
}

func recoverySearchResult(out bin.Decoder, messages ...tg.MessageClass) {
	out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: messages}
}

func checkRecoverySearch(t *testing.T, req *tg.MessagesSearchRequest, topic int, query string) {
	t.Helper()
	peer, ok := req.Peer.(*tg.InputPeerChannel)
	if !ok || peer.ChannelID != 41 || req.TopMsgID != topic || req.Q != query || req.Limit != 100 {
		t.Fatalf("recovery search escaped channel/topic/operation: %#v", req)
	}
	if _, ok := req.Filter.(*tg.InputMessagesFilterEmpty); !ok {
		t.Fatal("recovery search would exclude playable video documents")
	}
}

func TestUserUploadRecoveryLostACKDuplicateAndRestart(t *testing.T) {
	const random int64 = 0x12345678
	caption := recoveryCaption(random)
	payload := bytes.Repeat([]byte("complete-wire-carrier"), 40000)
	var committed *tg.Message
	searches, saves, sends := 0, 0, 0
	visible := false
	invoke := func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.MessagesSearchRequest:
			searches++
			checkRecoverySearch(t, req, 7, "12345678")
			if visible {
				recoverySearchResult(out, committed)
			} else {
				recoverySearchResult(out)
			}
		case *tg.UploadSaveFilePartRequest:
			saves++
			out.(*tg.BoolBox).Bool = &tg.BoolTrue{}
		case *tg.MessagesSendMediaRequest:
			sends++
			if req.RandomID != random || req.Message != caption {
				t.Fatal("recovery changed stable operation identity")
			}
			if committed == nil {
				committed = recoveryDocument(900, 7, caption, int64(len(payload)), 901)
				return errors.New("lost ACK after commit")
			}
			visible = true
			return tgerr.New(500, "RANDOM_ID_DUPLICATE")
		default:
			t.Fatalf("unexpected recovery RPC %T", in)
		}
		return nil
	}

	// First process loses the response before search indexing sees the message.
	c := fakeClient(invoke)
	if _, err := c.Upload(context.Background(), 41, 7, "carrier.png", "image/png", payload, nil, caption, random); err == nil {
		t.Fatal("lost ACK reported success without a durable message location")
	}
	if sends != 1 || searches != 2 || committed == nil {
		t.Fatal("first attempt did not preserve committed operation", sends, searches)
	}

	// A fresh client has no in-memory Updates mapping. Telegram deduplicates but
	// returns an error; search recovers the original message instead of resending.
	c = fakeClient(invoke)
	ref, err := c.Upload(context.Background(), 41, 7, "carrier.png", "image/png", payload, nil, caption, random)
	if err != nil || ref.MessageID != 900 || ref.DocumentID != 901 || sends != 2 || searches != 4 {
		t.Fatal("duplicate did not recover prior committed message", ref, err, sends, searches)
	}

	// Once visible, a third process avoids all protocol upload parts but must
	// consume the complete caller reader so root's wire SHA remains correct.
	beforeSaves := saves
	hash := sha256.New()
	c = fakeClient(invoke)
	events := 0
	c.opts.OnMessage = func(m Message) {
		events++
		if m.Reference.MessageID != 900 || m.Caption != caption {
			t.Fatal("recovered message not normalized", m)
		}
	}
	ref, err = c.UploadStream(context.Background(), 41, 7, "carrier.png", "image/png", io.TeeReader(bytes.NewReader(payload), hash), int64(len(payload)), nil, caption, random, nil)
	wantHash := sha256.Sum256(payload)
	if err != nil || ref.MessageID != 900 || sends != 2 || saves != beforeSaves || events != 1 || !bytes.Equal(hash.Sum(nil), wantHash[:]) {
		t.Fatal("preflight replay uploaded again or skipped complete wire hash", ref, err, sends, saves, events)
	}
}

func TestUserUploadRecoveryHandlesLostAndEmptyResponse(t *testing.T) {
	for _, emptyResponse := range []bool{false, true} {
		t.Run(fmt.Sprint(emptyResponse), func(t *testing.T) {
			caption := recoveryCaption(71)
			var committed *tg.Message
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch in.(type) {
				case *tg.MessagesSearchRequest:
					if committed == nil {
						recoverySearchResult(out)
					} else {
						recoverySearchResult(out, committed)
					}
				case *tg.UploadSaveFilePartRequest:
					out.(*tg.BoolBox).Bool = &tg.BoolTrue{}
				case *tg.MessagesSendMediaRequest:
					committed = recoveryDocument(90, 7, caption, 4, 91)
					if !emptyResponse {
						return errors.New("lost ACK")
					}
					out.(*tg.UpdatesBox).Updates = &tg.Updates{}
				default:
					t.Fatalf("unexpected RPC %T", in)
				}
				return nil
			})
			ref, err := c.Upload(context.Background(), 41, 7, "carrier.png", "image/png", []byte("wire"), nil, caption, 71)
			if err != nil || ref.MessageID != 90 {
				t.Fatal("lost/empty response did not recover", ref, err)
			}
		})
	}
}

func TestUserUploadRecoveryRejectsWrongIdentity(t *testing.T) {
	const random int64 = 71
	caption := recoveryCaption(random)
	for _, field := range []string{"topic", "size", "op", "file", "kind", "idx", "sha256", "channel"} {
		t.Run(field, func(t *testing.T) {
			wrong := recoveryDocument(90, 7, caption, 4, 91)
			identity, _ := parsePimgCaption(caption)
			switch field {
			case "topic":
				wrong.ReplyTo.(*tg.MessageReplyHeader).ReplyToTopID = 8
			case "size":
				wrong.Media.(*tg.MessageMediaDocument).Document.(*tg.Document).Size++
			case "op":
				identity.Op = "deadbeef"
			case "file":
				identity.File = "other-file"
			case "kind":
				identity.Kind = "thumb"
			case "idx":
				identity.Index++
			case "sha256":
				identity.SHA256 = strings.Repeat("cd", 32)
			case "channel":
				wrong.PeerID = &tg.PeerChannel{ChannelID: 42}
			}
			b, _ := json.Marshal(identity)
			wrong.Message = string(b)
			sends := 0
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch in.(type) {
				case *tg.MessagesSearchRequest:
					recoverySearchResult(out, wrong)
				case *tg.UploadSaveFilePartRequest:
					out.(*tg.BoolBox).Bool = &tg.BoolTrue{}
				case *tg.MessagesSendMediaRequest:
					sends++
					return tgerr.New(500, "RANDOM_ID_DUPLICATE")
				default:
					t.Fatalf("unexpected RPC %T", in)
				}
				return nil
			})
			ref, err := c.Upload(context.Background(), 41, 7, "carrier.png", "image/png", []byte("wire"), nil, caption, random)
			if err == nil || ref.MessageID != 0 || sends != 1 {
				t.Fatal("wrong identity recovered as current operation", ref, err, sends)
			}
		})
	}
}

func TestUserUploadRecoveryStillRejectsWrongInputLength(t *testing.T) {
	for _, actual := range []string{"123", "12345"} {
		t.Run(actual, func(t *testing.T) {
			caption := recoveryCaption(71)
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				if _, ok := in.(*tg.MessagesSearchRequest); !ok {
					t.Fatalf("preflight recovery uploaded to Telegram: %T", in)
				}
				recoverySearchResult(out, recoveryDocument(90, 7, caption, 4, 91))
				return nil
			})
			if _, err := c.UploadStream(context.Background(), 41, 7, "carrier.png", "image/png", strings.NewReader(actual), 4, nil, caption, 71, nil); !errors.Is(err, errStreamLength) {
				t.Fatal("recovery accepted a short or long local reader", err)
			}
		})
	}
}

func TestUserRecoveryPreflightFailureCannotBlindlySend(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint(ambiguous), func(t *testing.T) {
			caption := recoveryCaption(71)
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				if _, ok := in.(*tg.MessagesSearchRequest); !ok {
					t.Fatalf("unsafe resend after uncertain search: %T", in)
				}
				if !ambiguous {
					return errors.New("http://user:secret@proxy.invalid lost search ACK")
				}
				recoverySearchResult(out, recoveryDocument(90, 7, caption, 4, 91), recoveryDocument(92, 7, caption, 4, 91))
				return nil
			})
			if _, err := c.Upload(context.Background(), 41, 7, "carrier.png", "image/png", []byte("wire"), nil, caption, 71); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("uncertain search accepted or leaked upstream credentials", err)
			}
		})
	}
	c := fakeClient(func(context.Context, bin.Encoder, bin.Decoder) error {
		t.Fatal("mismatched caption op reached RPC")
		return nil
	})
	if _, err := c.Upload(context.Background(), 41, 7, "carrier.png", "image/png", []byte("wire"), nil, recoveryCaption(71), 72); err == nil {
		t.Fatal("mismatched caption operation ID accepted")
	}
}

func TestUserCopyRecoveryLostACKDuplicateAndRestart(t *testing.T) {
	for _, withOp := range []bool{false, true} {
		t.Run(fmt.Sprint(withOp), func(t *testing.T) {
			caption := recoveryCaption(0)
			query := "file-recovery"
			if withOp {
				caption, query = recoveryCaption(71), "47"
			}
			original := recoveryDocument(9, 7, caption, 4, 81)
			var committed *tg.Message
			forwards, searches := 0, 0
			visible := false
			invoke := func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch req := in.(type) {
				case *tg.ChannelsGetMessagesRequest:
					out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{original}}
				case *tg.MessagesSearchRequest:
					searches++
					checkRecoverySearch(t, req, 8, query)
					if visible {
						recoverySearchResult(out, committed)
					} else {
						recoverySearchResult(out)
					}
				case *tg.MessagesForwardMessagesRequest:
					forwards++
					if req.RandomID[0] != 991 || req.ID[0] != 9 || req.TopMsgID != 8 {
						t.Fatal("copy changed durable operation ID or source")
					}
					if committed == nil {
						committed = recoveryDocument(900, 8, caption, 4, 81)
						return errors.New("lost forward ACK")
					}
					visible = true
					return tgerr.New(500, "RANDOM_ID_DUPLICATE")
				default:
					t.Fatalf("unexpected copy recovery RPC %T", in)
				}
				return nil
			}
			source := Reference{AccountID: 77, ChatID: 41, TopicID: 7, MessageID: 9, DocumentID: 81, Size: 4}
			if _, err := fakeClient(invoke).Copy(context.Background(), source, 41, 8, 991); err == nil {
				t.Fatal("lost copy ACK reported success without location")
			}
			ref, err := fakeClient(invoke).Copy(context.Background(), source, 41, 8, 991)
			if err != nil || ref.MessageID != 900 || ref.DocumentID != 81 || forwards != 2 || searches != 4 {
				t.Fatal("duplicate forward did not recover original copy", ref, err, forwards, searches)
			}
			ref, err = fakeClient(invoke).Copy(context.Background(), source, 41, 8, 991)
			if err != nil || ref.MessageID != 900 || forwards != 2 {
				t.Fatal("preflight copy recovery forwarded again", ref, err, forwards)
			}
		})
	}
}

func TestUserCopyRecoveryRejectsWrongDocumentSizeTopicAndCaption(t *testing.T) {
	for _, field := range []string{"document", "size", "topic", "caption"} {
		t.Run(field, func(t *testing.T) {
			caption := recoveryCaption(71)
			wrong := recoveryDocument(90, 8, caption, 4, 81)
			switch field {
			case "document":
				wrong.Media.(*tg.MessageMediaDocument).Document.(*tg.Document).ID++
			case "size":
				wrong.Media.(*tg.MessageMediaDocument).Document.(*tg.Document).Size++
			case "topic":
				wrong.ReplyTo.(*tg.MessageReplyHeader).ReplyToTopID = 7
			case "caption":
				wrong.Message = strings.TrimSuffix(caption, "}") + `,"extra":"changed"}`
			}
			forwards := 0
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				switch in.(type) {
				case *tg.ChannelsGetMessagesRequest:
					recoverySearchResult(out, recoveryDocument(9, 7, caption, 4, 81))
				case *tg.MessagesSearchRequest:
					recoverySearchResult(out, wrong)
				case *tg.MessagesForwardMessagesRequest:
					forwards++
					return tgerr.New(500, "RANDOM_ID_DUPLICATE")
				default:
					t.Fatalf("unexpected RPC %T", in)
				}
				return nil
			})
			ref, err := c.Copy(context.Background(), Reference{AccountID: 77, ChatID: 41, TopicID: 7, MessageID: 9, DocumentID: 81, Size: 4}, 41, 8, 991)
			if err == nil || ref.MessageID != 0 || forwards != 1 {
				t.Fatal("wrong copy recovered as source document", ref, err, forwards)
			}
		})
	}
}

func TestUserRecoverySearchPaginatesAndFiltersGeneralTopic(t *testing.T) {
	caption := recoveryCaption(71)
	operation, _ := parsePimgCaption(caption)
	for _, topic := range []int{1, 7} {
		t.Run(strconv.Itoa(topic), func(t *testing.T) {
			calls := 0
			c := fakeClient(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				req := in.(*tg.MessagesSearchRequest)
				calls++
				if topic == 1 && req.TopMsgID != 0 || topic != 1 && req.TopMsgID != topic {
					t.Fatal("search scoped to incorrect thread", req.TopMsgID)
				}
				var messages []tg.MessageClass
				switch calls {
				case 1:
					messages = []tg.MessageClass{recoveryDocument(300, 8, caption, 4, 81)}
				case 2:
					if req.OffsetID != 300 {
						t.Fatal("search cursor not advanced", req.OffsetID)
					}
					messages = []tg.MessageClass{recoveryDocument(200, topic, caption, 4, 81)}
				case 3:
					if req.OffsetID != 200 {
						t.Fatal("search cursor not advanced", req.OffsetID)
					}
				default:
					t.Fatal("search did not terminate")
				}
				out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{Count: 2, Messages: messages}
				return nil
			})
			m, found, err := c.findPimgMessage(context.Background(), c.channels[41], topic, operation, 4, 0, "")
			if err != nil || !found || m.Reference.MessageID != 200 || calls != 3 {
				t.Fatal("search pagination lost exact target", m, found, err, calls)
			}
		})
	}
}
