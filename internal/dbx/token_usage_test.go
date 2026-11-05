package dbx

import (
	"context"
	"errors"
	"testing"
)

func TestTokenUsageCoalescesWritesWithoutDelayingRevocation(t *testing.T) {
	d := telegramTestDB(t)
	if err := d.InsertToken(&APIToken{ID: "usage", Name: "usage", TokenHash: "usage-hash", Prefix: "usage"}); err != nil {
		t.Fatal(err)
	}
	load := func() *APIToken {
		t.Helper()
		token, err := d.GetTokenByHash("usage-hash")
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	version := func() string {
		t.Helper()
		v, err := d.ChangeVersion(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	stale := load()
	before := version()
	d.TouchToken(stale)
	after := version()
	if before == after || !load().LastUsedAt.Valid {
		t.Fatal("first use was not recorded")
	}
	d.TouchToken(load())
	// Simulate another request that read the token before the first write.
	d.TouchToken(stale)
	if version() != after {
		t.Fatal("repeated use invalidated database caches")
	}
	if _, err := d.Exec(`UPDATE api_tokens SET last_used_at=? WHERE id='usage'`, now()-61); err != nil {
		t.Fatal(err)
	}
	old := load()
	before = version()
	d.TouchToken(old)
	if version() == before || load().LastUsedAt.Int64 <= old.LastUsedAt.Int64 {
		t.Fatal("last-use timestamp did not refresh after interval")
	}
	if err := d.RevokeToken("usage"); err != nil {
		t.Fatal(err)
	}
	before = version()
	d.TouchToken(stale)
	if version() != before {
		t.Fatal("stale request updated a revoked token")
	}
	if _, err := d.GetTokenByHash("usage-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revocation was delayed: %v", err)
	}
}
