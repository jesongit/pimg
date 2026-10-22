package store

import (
	"context"
	"io"
	"pimg/internal/storage"
	"time"
)

// Historical backend=r2 rows chose a bucket by scope. Preserve that contract
// with two independently configured clients instead of sharing credentials.
type legacyR2Hot struct {
	storage.Hot
	temporary storage.Hot
}

func (b legacyR2Hot) at(scope string) storage.Hot {
	if scope == storage.Temporary {
		return b.temporary
	}
	return b.Hot
}
func (b legacyR2Hot) PutObject(c context.Context, s, k string, r io.Reader, n int64, m, cc string) error {
	return b.at(s).PutObject(c, s, k, r, n, m, cc)
}
func (b legacyR2Hot) HeadSize(c context.Context, s, k string) (int64, error) {
	return b.at(s).HeadSize(c, s, k)
}
func (b legacyR2Hot) Delete(c context.Context, s, k string) error { return b.at(s).Delete(c, s, k) }
func (b legacyR2Hot) PresignGet(c context.Context, s, k string, t time.Duration, cc string) (string, error) {
	return b.at(s).PresignGet(c, s, k, t, cc)
}
func (b legacyR2Hot) OpenObject(c context.Context, s, k string) (io.ReadCloser, error) {
	return b.at(s).(storage.HotReader).OpenObject(c, s, k)
}
func (b legacyR2Hot) PutPublicTemp(c context.Context, k string, r io.Reader, n int64, m string) error {
	return b.temporary.(storage.PublicTemporary).PutPublicTemp(c, k, r, n, m)
}
func (b legacyR2Hot) TempPublicURL(k string) string {
	return b.temporary.(storage.PublicTemporary).TempPublicURL(k)
}
func (b legacyR2Hot) DeletePublicTemp(c context.Context, k string) error {
	return b.temporary.(storage.PublicTemporary).DeletePublicTemp(c, k)
}
func (b legacyR2Hot) DeleteManagedTemp(c context.Context, k string) error {
	return b.temporary.(interface {
		DeleteManagedTemp(context.Context, string) error
	}).DeleteManagedTemp(c, k)
}
