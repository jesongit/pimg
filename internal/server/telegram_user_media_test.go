package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"

	"pimg/internal/carrier"
	"pimg/internal/dbx"
	"pimg/internal/tguser"
)

type rawUserTG struct {
	tguser.Transport
	data  []byte
	opens int
}

func (f *rawUserTG) Status() tguser.Status {
	return tguser.Status{State: "authorized", Configured: true, AccountID: 11}
}
func (f *rawUserTG) OpenStream(context.Context, tguser.Reference) (io.ReadCloser, error) {
	f.opens++
	return io.NopCloser(bytes.NewReader(f.data)), nil
}
func (f *rawUserTG) UploadStream(context.Context, int64, int, string, string, io.Reader, int64, []byte, string, int64, *tguser.VideoMetadata) (tguser.Reference, error) {
	panic("raw download attempted upload")
}

func TestTelegramUserRawPNGTokenHeadAndRange(t *testing.T) {
	s := telegramLinkServer(t)
	f := &dbx.File{ID: "user-png", Name: "private", OrigName: "private.png", Ext: "png", Mime: "image/png", Kind: "image", Size: 123, SHA256: "filehash", ArchiveState: "done", ColdBackend: "telegram", KeyWrapped: []byte{1}, KeyNonce: []byte{2}, CryptoVer: 1}
	if err := s.DB.InsertFile(f); err != nil {
		t.Fatal(err)
	}
	wire, err := carrier.EncodePart(interactionPNG(t), carrier.PartHeader{FileID: f.ID, Kind: "data", Index: 0}, []byte("stored opaque ciphertext"))
	if err != nil {
		t.Fatal(err)
	}
	raw := &rawUserTG{data: wire}
	s.Svc.UserTG = raw
	s.Cfg.Telegram.ChatID = 77
	metadata, _ := json.Marshal(map[string]any{"topic_id": 10, "message_id": 55, "document_id": 99, "size": len(wire), "carrier": true, "wire_sha256": fmt.Sprintf("%x", sha256.Sum256(wire))})
	if err := s.DB.UpsertChunk(&dbx.Chunk{FileID: f.ID, Kind: "data", Idx: 0, ObjectKey: "tguser:" + string(metadata), Size: 24, SHA256: "cipherhash"}); err != nil {
		t.Fatal(err)
	}
	// Exact stored access must work without a media decryption key.
	s.Svc.Master = nil
	handler := s.Handler()
	call := func(method, rangeValue string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/media/tg/user-png/original", nil)
		if auth {
			r.Header.Set("Authorization", "Bearer telegram-links-auth")
		}
		if rangeValue != "" {
			r.Header.Set("Range", rangeValue)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := call("GET", "", false); w.Code != 401 || raw.opens != 0 {
		t.Fatalf("token bypass %d", w.Code)
	}
	if w := call("HEAD", "", true); w.Code != 200 || w.Header().Get("Content-Length") != fmt.Sprint(len(wire)) || raw.opens != 0 {
		t.Fatalf("HEAD fetched file or wrong size: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "", true); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), wire) || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("raw bytes transformed: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "bytes=8-31", true); w.Code != 206 || !bytes.Equal(w.Body.Bytes(), wire[8:32]) {
		t.Fatalf("range invalid: %d %s", w.Code, w.Body.String())
	}
	if _, err := s.DB.Exec(`UPDATE files SET hidden=1 WHERE id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "", true); w.Code != 404 {
		t.Fatalf("retired archive accessible %d", w.Code)
	}
}
