package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"strconv"
	"strings"
	"sync"

	"pimg/internal/config"
	"pimg/internal/cryptox"
)

// dynamic 数据库设置可覆盖的运行参数（初始化自 config，可热更新）。
type dynamic struct {
	mu                 sync.RWMutex
	retentionHours     int
	presignMinutes     int
	maxSizeMB          int
	allowedExts        []string
	coldStorage        string
	hotStorage         string
	originalCacheHours int
	publicBaseURL      string
	publicMu           sync.RWMutex
}

func (s *Service) initDynamic() error {
	m, err := s.DB.AllSettings()
	if err != nil {
		return err
	}
	base := m["site.public_base_url"]
	maxMB, _ := strconv.Atoi(m["site.upload_max_mb"])
	extensions := config.DefaultAllowedExts()
	if raw := strings.TrimSpace(m["site.allowed_exts"]); raw != "" {
		extensions = strings.Split(raw, ",")
	}
	positive := func(key string, fallback int) int {
		n, _ := strconv.Atoi(m[key])
		if n > 0 {
			return n
		}
		return fallback
	}
	s.dyn = dynamic{
		publicBaseURL:      strings.TrimRight(base, "/"),
		retentionHours:     positive("site.retention_hours", 24),
		presignMinutes:     positive("site.presign_minutes", 30),
		maxSizeMB:          maxMB,
		allowedExts:        extensions,
		originalCacheHours: 24,
	}
	return nil
}

func (s *Service) PublicBaseURL() string {
	s.dyn.publicMu.RLock()
	defer s.dyn.publicMu.RUnlock()
	return s.dyn.publicBaseURL
}

func (s *Service) SetPublicBaseURL(base string) {
	s.dyn.publicMu.Lock()
	defer s.dyn.publicMu.Unlock()
	s.dyn.publicBaseURL = strings.TrimRight(base, "/")
}

// SetDynamic 热更新运行参数（-1/nil 表示不修改）。
func (s *Service) SetDynamic(retention, presign, maxMB int, exts []string) {
	s.dyn.mu.Lock()
	defer s.dyn.mu.Unlock()
	if retention > 0 {
		s.dyn.retentionHours = retention
	}
	if presign > 0 {
		s.dyn.presignMinutes = presign
	}
	if maxMB >= 0 {
		s.dyn.maxSizeMB = maxMB
	}
	if len(exts) > 0 {
		s.dyn.allowedExts = exts
	}
}

func (s *Service) RetentionHours() int {
	s.dyn.mu.RLock()
	defer s.dyn.mu.RUnlock()
	return s.dyn.retentionHours
}

func (s *Service) PresignMinutes() int {
	s.dyn.mu.RLock()
	defer s.dyn.mu.RUnlock()
	return s.dyn.presignMinutes
}

func (s *Service) MaxSizeMB() int {
	s.dyn.mu.RLock()
	defer s.dyn.mu.RUnlock()
	return s.dyn.maxSizeMB
}

func (s *Service) AllowedExts() []string {
	s.dyn.mu.RLock()
	defer s.dyn.mu.RUnlock()
	return s.dyn.allowedExts
}

func sha256New() hash.Hash { return sha256.New() }

func hexOf(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// teeReader 读出数据的同时喂给哈希。
func teeReader(r io.Reader, h hash.Hash) io.Reader { return &hashTee{r: r, h: h} }

type hashTee struct {
	r io.Reader
	h hash.Hash
}

func (t *hashTee) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.h.Write(p[:n])
	}
	return n, err
}

func cryptoxDecryptReader(r io.Reader, key []byte) (io.Reader, error) {
	return cryptox.DecryptReader(r, key)
}

// nopCloser 包装 bytes.Reader 成为 ReadCloser。
type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
