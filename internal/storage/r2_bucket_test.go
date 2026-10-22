package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pimg/internal/r2x"
)

func TestSingleBucketR2CheckDoesNotRequireTheOtherBucket(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/permanent/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	client := r2x.New(server.URL, "fixture", "fixture", "temporary", "permanent", "https://permanent.example", "", "")
	permanent := R2{Client: client, Bucket: "permanent", PublicBase: client.PublicBase}
	temporary := R2{Client: client, Bucket: "temporary"}
	if err := permanent.Check(context.Background()); err != nil {
		t.Fatal("a working permanent instance depended on the inaccessible temporary bucket", err)
	}
	if err := temporary.Check(context.Background()); err == nil {
		t.Fatal("the inaccessible temporary bucket was reported as healthy")
	}
	if temporary.PublicURL("file.png") != "" || permanent.PublicURL("file.png") != "https://permanent.example/file.png" {
		t.Fatal("temporary instance inherited the permanent domain")
	}
}
