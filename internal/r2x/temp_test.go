package r2x

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"pimg/internal/tempcdn"
)

type tempTransport func(*http.Request) (*http.Response, error)

func (f tempTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPublicTempJournalAndRevocation(t *testing.T) {
	for _, failure := range []string{"none", "journal", "upload", "purge"} {
		t.Run(failure, func(t *testing.T) {
			var mu sync.Mutex
			objects := map[string][]byte{}
			var events []string
			var cache string
			purgeFailed := failure == "purge"
			expires := time.Now().Unix() + 3600
			key := fmt.Sprintf("tmp/v1/%013d/file/copy.png", expires)
			journal := "/control/" + tempcdn.JobKey(key, expires)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, r.Method+" "+r.URL.Path)
				if strings.HasSuffix(r.URL.Path, "/purge_cache") {
					var body struct {
						Files []string `json:"files"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Files) != 1 || body.Files[0] != "https://temp.example.com/"+key || !strings.Contains(r.URL.Path, "/zones/temp-zone/") {
						t.Error("incorrect purge target", body)
					}
					if _, exists := objects["/temp/"+key]; exists {
						t.Error("purge ran before source deletion")
					}
					if purgeFailed {
						w.WriteHeader(429)
						fmt.Fprint(w, `{"success":false}`)
						return
					}
					fmt.Fprint(w, `{"success":true}`)
					return
				}
				switch r.Method {
				case "PUT":
					if failure == "journal" && strings.HasPrefix(r.URL.Path, "/control/") || failure == "upload" && strings.HasPrefix(r.URL.Path, "/temp/") {
						w.WriteHeader(403)
						fmt.Fprint(w, `<Error><Code>AccessDenied</Code></Error>`)
						return
					}
					body, _ := io.ReadAll(r.Body)
					if r.URL.Path == "/temp/"+key {
						if _, exists := objects[journal]; !exists {
							t.Error("plaintext uploaded before durable journal")
						}
						cache = r.Header.Get("Cache-Control")
					} else if r.Header.Get("X-Amz-Meta-Object_key") != key || r.Header.Get("X-Amz-Meta-Version") != "1" {
						t.Error("journal missing Worker metadata")
					}
					objects[r.URL.Path] = body
				case "DELETE":
					delete(objects, r.URL.Path)
					w.WriteHeader(204)
				default:
					w.WriteHeader(405)
				}
			}))
			defer srv.Close()
			transport := tempTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "api.cloudflare.com" {
					r = r.Clone(r.Context())
					u := *r.URL
					r.URL = &u
					r.URL.Scheme = "http"
					r.URL.Host = strings.TrimPrefix(srv.URL, "http://")
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			c := New(srv.URL, "key", "secret", "temp", "perm", "https://perm.example.com", "perm-zone", "token", WithPublicTemp("https://temp.example.com", "control", "temp-zone", 600), WithHTTPTransport(transport))
			plain := []byte("hash-identical plaintext")
			err := c.PutPublicTemp(context.Background(), key, bytes.NewReader(plain), int64(len(plain)), "image/png")
			if failure == "journal" || failure == "upload" {
				if err == nil {
					t.Fatal("expected upload failure")
				}
				if failure == "journal" && len(events) != 1 {
					t.Fatal("failed journal must prevent payload transfer", events)
				}
				if failure == "upload" && objects[journal] == nil {
					t.Fatal("ambiguous upload lost cleanup intent")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(objects["/temp/"+key], plain) || cache != "public, max-age=0, s-maxage=600, must-revalidate" {
				t.Fatal("plaintext/cache policy changed", cache)
			}
			err = c.DeletePublicTemp(context.Background(), key)
			if purgeFailed {
				if err == nil || objects[journal] == nil {
					t.Fatal("failed purge lost durable retry")
				}
				purgeFailed = false
				err = c.DeletePublicTemp(context.Background(), key)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(objects) != 0 {
				t.Fatal("source or journal survived successful revocation", objects)
			}
		})
	}
}
