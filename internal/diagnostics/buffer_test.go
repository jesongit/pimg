package diagnostics

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestCaptureRedactionGroupsFiltersAndCursor(t *testing.T) {
	b := NewBuffer()
	log := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), b)).With("password", "plain-secret").WithGroup("store").With("access_key_id", "AK-SECRET")
	log.Info("started")
	log.Warn("Bearer bearer-secret pimg_testsecret https://user:url-secret@example.test/path?signature=query-secret", "err", errors.New(`{"password":"json-secret"} token=field-secret bot123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ123456`), "settings", map[string]string{"private": "object-secret"})
	log.Error("failure", "detail", "read timeout")
	s := b.Read(Query{Limit: 1, Level: slog.LevelWarn})
	if len(s.Items) != 1 || s.Items[0].Level != "ERROR" || !s.HasMore {
		t.Fatal(s)
	}
	s = b.Read(Query{After: 1, Limit: 1, Level: slog.LevelWarn})
	if len(s.Items) != 1 || s.Items[0].ID != 2 || s.NextAfter != 2 || !s.HasMore {
		t.Fatal(s)
	}
	e := s.Items[0]
	for _, v := range append([]string{e.Message}, values(e.Fields)...) {
		for _, secret := range []string{"plain-secret", "AK-SECRET", "bearer-secret", "testsecret", "url-secret", "query-secret", "json-secret", "field-secret", "object-secret"} {
			if strings.Contains(v, secret) {
				t.Fatalf("leaked %q: %s", secret, v)
			}
		}
	}
	if e.Fields["password"] != redacted || e.Fields["store.access_key_id"] != redacted {
		t.Fatal(e.Fields)
	}
	s.Items[0].Fields["password"] = "mutated"
	if b.Read(Query{Search: "object omitted"}).Items[0].Fields["password"] != redacted {
		t.Fatal("caller mutated ring")
	}
	s = b.Read(Query{After: 2, Limit: 1, Search: "timeout"})
	if len(s.Items) != 1 || s.NextAfter != 3 || s.HasMore {
		t.Fatal(s)
	}
	if !b.Read(Query{After: 999}).CursorReset {
		t.Fatal("stale cursor not identified")
	}
}

func values(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestKnownSecretsAreRemovedBeforeSearchAndRetention(t *testing.T) {
	b := NewBuffer("unlabelled-deployment-credential")
	log := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), b))
	log.Error("failure unlabelled-deployment-credential", "detail", "unlabelled-deployment-credential")
	if len(b.Read(Query{Search: "unlabelled-deployment"}).Items) != 0 {
		t.Fatal("search disclosed a deployment credential")
	}
	for _, e := range b.entries {
		if strings.Contains(e.Message, "unlabelled-deployment") {
			t.Fatal("credential retained")
		}
	}
}

func TestRuntimeBufferBoundsAndConcurrentReaders(t *testing.T) {
	b := NewBuffer()
	log := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), b))
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 800; i++ {
				log.Info(strings.Repeat("现场", 2000), "detail", strings.Repeat("x", 2000))
				b.Read(Query{Limit: 2})
			}
		}()
	}
	wg.Wait()
	if b.bytes > MaxBytes || b.count > MaxEntries {
		t.Fatal(b.bytes, b.count)
	}
	s := b.Read(Query{After: 1, Limit: 200})
	if !s.Gap || len(s.Items) != 200 || !s.HasMore {
		t.Fatal("unbounded or missing gap", len(s.Items), s.Gap, s.HasMore)
	}
	for i, e := range s.Items {
		if i > 0 && e.ID <= s.Items[i-1].ID {
			t.Fatal("cursor order")
		}
		if len(e.Message) > 4096 {
			t.Fatal("message bound")
		}
	}
}
