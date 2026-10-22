package storage

import (
	"bytes"
	"context"
	"io"
	"pimg/internal/r2x"
	"strings"
	"time"
)

// StreamCold is optional. Replication uses it to keep large objects off the heap.
type StreamCold interface {
	PutReader(context.Context, string, io.Reader, int64, string) (Reference, error)
}
type PlannedCold interface{ PlanReference(string) Reference }

func (l LocalCold) PlanReference(key string) Reference { return Reference{Key: key} }
func (o ObjectStore) PlanReference(key string) Reference {
	return Reference{Key: o.key("archives", key)}
}

type HotReader interface {
	OpenObject(context.Context, string, string) (io.ReadCloser, error)
}

// ObjectStore gives one R2 instance disjoint archive, permanent and temporary namespaces.
type ObjectStore struct {
	Client                     *r2x.Client
	Bucket, Prefix, PublicBase string
}

func (o ObjectStore) key(scope, key string) string {
	p := strings.Trim(o.Prefix, "/")
	if p != "" {
		p += "/"
	}
	return p + scope + "/" + key
}
func (o ObjectStore) Put(ctx context.Context, k string, b []byte, c string) (Reference, error) {
	return o.PutReader(ctx, k, bytes.NewReader(b), int64(len(b)), c)
}
func (o ObjectStore) PutReader(ctx context.Context, k string, r io.Reader, n int64, c string) (Reference, error) {
	key := o.key("archives", k)
	e := o.Client.PutObject(ctx, o.Bucket, key, r, n, "application/octet-stream", "private, no-store")
	return Reference{Key: key}, e
}
func (o ObjectStore) Open(ctx context.Context, r Reference) (io.ReadCloser, error) {
	return o.Client.Get(ctx, o.Bucket, r.Key)
}
func (o ObjectStore) Delete(ctx context.Context, r Reference) error {
	return o.Client.Delete(ctx, o.Bucket, r.Key)
}

// DisplayObjectStore wraps the archive adapter because Go does not overload Delete.
type DisplayObjectStore struct{ ObjectStore }

func (o DisplayObjectStore) PutObject(ctx context.Context, scope, key string, r io.Reader, n int64, mime, cache string) error {
	return o.Client.PutObject(ctx, o.Bucket, o.key(scope, key), r, n, mime, cache)
}
func (o DisplayObjectStore) HeadSize(ctx context.Context, scope, key string) (int64, error) {
	return o.Client.HeadSize(ctx, o.Bucket, o.key(scope, key))
}
func (o DisplayObjectStore) Delete(ctx context.Context, scope, key string) error {
	return o.Client.Delete(ctx, o.Bucket, o.key(scope, key))
}
func (o DisplayObjectStore) PresignGet(ctx context.Context, scope, key string, ttl time.Duration, cache string) (string, error) {
	return o.Client.PresignGet(ctx, o.Bucket, o.key(scope, key), ttl, cache)
}
func (o DisplayObjectStore) PublicURL(key string) string {
	if o.PublicBase == "" {
		return ""
	}
	return strings.TrimRight(o.PublicBase, "/") + "/" + o.key(Permanent, key)
}
func (o DisplayObjectStore) PurgeCDN(ctx context.Context, urls []string) error {
	return o.Client.PurgeCDN(ctx, urls)
}
func (o DisplayObjectStore) TempPublicURL(key string) string {
	return o.Client.TempPublicURL(o.key(Temporary, key))
}
func (o DisplayObjectStore) PutPublicTemp(ctx context.Context, key string, r io.Reader, n int64, mime string) error {
	return o.PutObject(ctx, Temporary, key, r, n, mime, "public, max-age=60")
}
func (o DisplayObjectStore) DeletePublicTemp(ctx context.Context, key string) error {
	return o.DeleteManagedTemp(ctx, key)
}
func (o DisplayObjectStore) DeleteManagedTemp(ctx context.Context, key string) error {
	return o.Client.DeleteManagedTemp(ctx, o.key(Temporary, key))
}
func (o DisplayObjectStore) OpenObject(ctx context.Context, scope, key string) (io.ReadCloser, error) {
	return o.Client.Get(ctx, o.Bucket, o.key(scope, key))
}
func (o ObjectStore) Check(ctx context.Context) error {
	_, e := o.Client.List(ctx, o.Bucket, o.key("archives", ""), "", 1)
	return e
}
func (o DisplayObjectStore) Check(ctx context.Context) error { return o.ObjectStore.Check(ctx) }
func (r R2) OpenObject(ctx context.Context, scope, key string) (io.ReadCloser, error) {
	return r.Client.Get(ctx, r.bucket(scope), key)
}
func (l *LocalHot) OpenObject(ctx context.Context, scope, key string) (io.ReadCloser, error) {
	k, e := scoped(scope, key)
	if e != nil {
		return nil, e
	}
	return l.Disk.Open(k)
}

func (l LocalCold) PutReader(ctx context.Context, key string, r io.Reader, n int64, caption string) (Reference, error) {
	return Reference{Key: key}, l.Disk.Put(ctx, key, r, n)
}
