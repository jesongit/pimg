package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"image"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMissingPreviewRepairsMetadataAndAuthorization(t *testing.T) {
	s := importServer(t)
	plain := interactionPNG(t)
	f, err := s.Svc.ReceiveUpload(t.Context(), bytes.NewReader(plain), "repair.png", "png", sql.NullInt64{}, nil, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	archiveImportFile(t, s, f)
	request := carrierRequest(t, s)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(f.ID+"\x00"+f.SHA256)))
	previewPath := filepath.Join(s.Cfg.Paths.RuntimeRoot(), "previews", key+"-v1.jpg")
	for _, cached := range []bool{false, true} {
		if !cached {
			if err = os.Remove(previewPath); err != nil {
				t.Fatal(err)
			}
		}
		s.DB.SetThumbInfo(f.ID, false, 0, 0, "", 0, true)
		w := request("GET", "/api/admin/files/previews?ids="+f.ID, nil, "")
		if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("kind=thumb")) {
			t.Fatal("repair URL missing", w.Code, w.Body.String())
		}
		w = request("GET", privateContentURL(f.ID, "thumb"), nil, "")
		cfg, format, decodeErr := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
		if w.Code != 200 || decodeErr != nil || format != "jpeg" {
			t.Fatal("repair failed", w.Code, decodeErr)
		}
		got, _ := s.DB.GetFile(f.ID)
		if !got.HasThumb || got.ThumbW != cfg.Width || got.ThumbH != cfg.Height || !got.HasBThumb {
			t.Fatal("repair metadata missing", got)
		}
		w = request("GET", "/api/admin/files/"+f.ID+"/download", nil, "")
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), plain) {
			t.Fatal("original changed")
		}
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", privateContentURL(f.ID, "thumb"), nil))
	if w.Code != 401 {
		t.Fatal("unauthenticated preview exposed", w.Code)
	}
	s.DB.SetPurgeState(f.ID, "pending")
	if w := request("GET", privateContentURL(f.ID, "thumb"), nil, ""); w.Code < 400 {
		t.Fatal("purging preview exposed")
	}
	if w := request("GET", "/api/admin/files/previews?ids="+f.ID, nil, ""); bytes.Contains(w.Body.Bytes(), []byte("kind=thumb")) {
		t.Fatal("purging repair URL exposed")
	}
}
