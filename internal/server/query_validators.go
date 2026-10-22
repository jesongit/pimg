package server

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"pimg/internal/dbx"
)

type queryValidator struct {
	version, tag string
	until        time.Time
	expiry       int64
	body         []byte
}
type queryValidators struct {
	mu      sync.Mutex
	entries map[[32]byte]queryValidator
}

func matchesETag(header, tag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == tag {
			return true
		}
	}
	return false
}

// Run only inside authentication. Retain bounded JSON views, never credentials.
// Pure database views can reuse JSON or return 304 before grouping history when
// no transaction has committed. Filesystem/provider-dependent views are excluded.
func (s *Server) fastConditional(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pure := r.URL.Path == "/api/admin/logs" || r.URL.Path == "/api/admin/tasks/summary" || r.URL.Path == "/api/admin/files/timeline" || r.URL.Path == "/api/account/files/timeline"
		out, buffered := w.(*pageResponse)
		if r.Method != "GET" || !pure || !buffered {
			next(w, r)
			return
		}
		version, err := s.DB.ChangeVersion(r.Context())
		if err != nil {
			next(w, r)
			return
		}
		key := sha256.Sum256([]byte(dbx.ContextOwner(r.Context()) + "\x00" + r.URL.Path + "?" + r.URL.RawQuery))
		now := time.Now()
		cache := &s.queryValidators
		cache.mu.Lock()
		cached, ok := cache.entries[key]
		cache.mu.Unlock()
		if ok && cached.version == version && now.Before(cached.until) && (cached.expiry == 0 || now.Unix() < cached.expiry) {
			w.Header().Set("ETag", cached.tag)
			w.Header().Set("Cache-Control", "private, no-store")
			w.Header().Add("Vary", "Cookie, Authorization, X-Pimg-Owner")
			if matchesETag(r.Header.Get("If-None-Match"), cached.tag) {
				w.WriteHeader(http.StatusNotModified)
			} else if len(cached.body) > 0 {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Write(cached.body)
			} else {
				next(w, r)
			}
			return
		}
		next(w, r)
		if out.streaming || out.code != 200 || !strings.HasPrefix(out.header.Get("Content-Type"), "application/json") {
			return
		}
		after, err := s.DB.ChangeVersion(r.Context())
		if err != nil || after != version {
			return
		}
		expiry, err := s.DB.NextOperationExpiry()
		if err != nil || expiry != 0 && expiry <= time.Now().Unix() {
			return
		}
		tag := fmt.Sprintf(`"%x"`, sha256.Sum256(out.body.Bytes()))
		cache.mu.Lock()
		defer cache.mu.Unlock()
		// Retain a small number of bounded authorized DB views. Cache lookup is
		// inside authentication; the DB version and expiring operations still
		// invalidate it. Reloads without an ETag can reuse the same JSON body.
		if cache.entries == nil || len(cache.entries) >= 64 {
			cache.entries = make(map[[32]byte]queryValidator)
		}
		var body []byte
		if out.body.Len() <= 128<<10 {
			body = append([]byte(nil), out.body.Bytes()...)
		}
		cache.entries[key] = queryValidator{version: version, tag: tag, body: body, expiry: expiry, until: now.Add(5 * time.Minute)}
	}
}
