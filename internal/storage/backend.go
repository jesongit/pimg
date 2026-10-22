// Package storage defines provider contracts for durable archives and serving copies.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"pimg/internal/r2x"
	"pimg/internal/tgx"
)

const (
	Temporary = "temp"
	Permanent = "permanent"
)

// Reference supports both object stores and message based archives.
type Reference struct {
	Key       string
	MessageID int64
	FileID    string
}

type Cold interface {
	Put(context.Context, string, []byte, string) (Reference, error)
	Open(context.Context, Reference) (io.ReadCloser, error)
	Delete(context.Context, Reference) error
	Check(context.Context) error
}

type Hot interface {
	PutObject(context.Context, string, string, io.Reader, int64, string, string) error
	HeadSize(context.Context, string, string) (int64, error)
	Delete(context.Context, string, string) error
	PresignGet(context.Context, string, string, time.Duration, string) (string, error)
	PublicURL(string) string
	PurgeCDN(context.Context, []string) error
	Check(context.Context) error
}

type Telegram struct{ Client *tgx.Client }

func (t Telegram) Put(ctx context.Context, key string, data []byte, caption string) (Reference, error) {
	m, f, _, err := t.Client.SendDocument(ctx, strings.ReplaceAll(key, "/", "-"), data, caption)
	meta, _ := json.Marshal(map[string]any{"size": len(data), "sha256": fmt.Sprintf("%x", sha256.Sum256(data))})
	return Reference{Key: "tgwire:" + string(meta), MessageID: m, FileID: f}, err
}
func (t Telegram) Open(ctx context.Context, ref Reference) (io.ReadCloser, error) {
	return t.Client.Download(ctx, ref.FileID)
}
func (t Telegram) Delete(ctx context.Context, ref Reference) error {
	if ref.MessageID == 0 {
		return nil
	}
	return t.Client.DeleteMessage(ctx, ref.MessageID)
}
func (t Telegram) Check(ctx context.Context) error { return t.Client.TestConnectivity(ctx) }

type R2 struct {
	Client *r2x.Client
	// An empty Bucket is reserved for historical records that selected a bucket
	// by scope. New default instances always bind exactly one physical bucket.
	Bucket     string
	PublicBase string
}

// PublicTemporary is optional; local and legacy private R2 copies retain signed URLs.
type PublicTemporary interface {
	PutPublicTemp(context.Context, string, io.Reader, int64, string) error
	TempPublicURL(string) string
	DeletePublicTemp(context.Context, string) error
}

func (r R2) PutPublicTemp(ctx context.Context, key string, body io.Reader, size int64, mime string) error {
	if r.Bucket != "" && r.Bucket != r.Client.TempBucket {
		return fmt.Errorf("此 R2 实例不支持公开临时副本")
	}
	return r.Client.PutPublicTemp(ctx, key, body, size, mime)
}
func (r R2) TempPublicURL(key string) string {
	if r.Bucket != "" && r.Bucket != r.Client.TempBucket {
		return ""
	}
	return r.Client.TempPublicURL(key)
}
func (r R2) DeletePublicTemp(ctx context.Context, key string) error {
	if r.Bucket != "" && r.Bucket != r.Client.TempBucket {
		return fmt.Errorf("此 R2 实例不支持公开临时副本")
	}
	return r.Client.DeletePublicTemp(ctx, key)
}

func (r R2) bucket(scope string) string {
	if r.Bucket != "" {
		return r.Bucket
	}
	if scope == Temporary {
		return r.Client.TempBucket
	}
	return r.Client.PermBucket
}
func (r R2) PutObject(ctx context.Context, scope, key string, body io.Reader, size int64, mime, cache string) error {
	return r.Client.PutObject(ctx, r.bucket(scope), key, body, size, mime, cache)
}
func (r R2) HeadSize(ctx context.Context, scope, key string) (int64, error) {
	return r.Client.HeadSize(ctx, r.bucket(scope), key)
}
func (r R2) Delete(ctx context.Context, scope, key string) error {
	return r.Client.Delete(ctx, r.bucket(scope), key)
}
func (r R2) PresignGet(ctx context.Context, scope, key string, ttl time.Duration, cache string) (string, error) {
	return r.Client.PresignGet(ctx, r.bucket(scope), key, ttl, cache)
}
func (r R2) PublicURL(key string) string {
	if r.Bucket != "" {
		if r.PublicBase == "" {
			return ""
		}
		return strings.TrimRight(r.PublicBase, "/") + "/" + key
	}
	return r.Client.PublicURL(key)
}
func (r R2) PurgeCDN(ctx context.Context, urls []string) error { return r.Client.PurgeCDN(ctx, urls) }
func (r R2) Check(ctx context.Context) error {
	scopes := []string{Temporary, Permanent}
	if r.Bucket != "" {
		scopes = []string{Permanent}
	}
	for _, scope := range scopes {
		if _, err := r.HeadSize(ctx, scope, "pimg/.healthcheck"); err != nil && !strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "NoSuchKey") {
			return fmt.Errorf("%s: %w", scope, err)
		}
	}
	return nil
}

func (r R2) DeleteManagedTemp(ctx context.Context, key string) error {
	return r.Client.DeleteManagedTemp(ctx, key)
}
