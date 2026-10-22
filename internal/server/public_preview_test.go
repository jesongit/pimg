package server

import (
	"bytes"
	"context"
	"database/sql"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicPreviewRequiresCurrentPublicationAndUsesLocalData(t *testing.T) {
	s := interactionServer(t)
	ctx := context.Background()
	for _, direct := range []bool{false, true} {
		f, err := s.Svc.ReceiveUpload(ctx, bytes.NewReader(interactionPNG(t)), "preview.png", "png", sql.NullInt64{}, nil, 0, direct)
		if err != nil {
			t.Fatal(err)
		}
		if direct {
			err = s.Svc.RunDirectArchive(ctx, f.ID)
		} else {
			err = s.Svc.RunArchive(ctx, f.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
		call := func() *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/previews/"+f.ID, nil))
			return w
		}
		w := call()
		if !direct {
			if w.Code != 404 {
				t.Fatal("private preview exposed", w.Code)
			}
			continue
		}
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "image/jpeg") || w.Body.Len() == 0 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("public preview unavailable", w.Code)
		}
		if err = s.Svc.RunUnpublish(ctx, f.ID); err != nil {
			t.Fatal(err)
		}
		if w = call(); w.Code != 404 {
			t.Fatal("revoked preview remains public", w.Code)
		}
	}
}
