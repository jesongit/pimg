package diagnostics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const MaxEntries = 2048
const MaxBytes = 2 << 20

type Entry struct {
	ID      uint64            `json:"id"`
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
	size    int
}

type Buffer struct {
	mu                  sync.Mutex
	entries             [MaxEntries]Entry
	start, count, bytes int
	sequence            uint64
	started             time.Time
	instance            string
	redactor            *strings.Replacer
}

func NewBuffer(secrets ...string) *Buffer {
	b := &Buffer{started: time.Now().UTC()}
	pairs := []string{}
	for _, secret := range secrets {
		if secret != "" {
			pairs = append(pairs, secret, redacted)
		}
	}
	b.redactor = strings.NewReplacer(pairs...)
	var id [12]byte
	if _, err := rand.Read(id[:]); err == nil {
		b.instance = hex.EncodeToString(id[:])
	} else {
		b.instance = b.started.Format(time.RFC3339Nano)
	}
	return b
}

func (b *Buffer) redact(text string) string { return Redact(b.redactor.Replace(text)) }

type Query struct {
	After  uint64
	Limit  int
	Level  slog.Level
	Search string
}

type Snapshot struct {
	Instance    string    `json:"instance_id"`
	Started     time.Time `json:"started_at"`
	Items       []Entry   `json:"items"`
	NextAfter   uint64    `json:"next_after"`
	OldestID    uint64    `json:"oldest_id"`
	LatestID    uint64    `json:"latest_id"`
	Gap         bool      `json:"gap"`
	CursorReset bool      `json:"cursor_reset"`
	HasMore     bool      `json:"has_more"`
	Capacity    int       `json:"capacity"`
}

func (b *Buffer) Read(q Query) Snapshot {
	if q.Limit < 1 || q.Limit > 200 {
		q.Limit = 100
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Snapshot{Instance: b.instance, Started: b.started, Items: []Entry{}, LatestID: b.sequence, NextAfter: b.sequence, Capacity: MaxEntries}
	if b.count > 0 {
		s.OldestID = b.entries[b.start].ID
	}
	s.Gap = q.After > 0 && q.After+1 < s.OldestID
	s.CursorReset = q.After > s.LatestID
	search := strings.ToLower(q.Search)
	for i := 0; i < b.count; i++ {
		e := b.entries[(b.start+i)%MaxEntries]
		var level slog.Level
		_ = level.UnmarshalText([]byte(e.Level))
		if e.ID <= q.After || level < q.Level {
			continue
		}
		if search != "" {
			match := strings.Contains(strings.ToLower(e.Message), search)
			for k, v := range e.Fields {
				match = match || strings.Contains(strings.ToLower(k+" "+v), search)
			}
			if !match {
				continue
			}
		}
		if q.After > 0 && len(s.Items) == q.Limit {
			s.HasMore = true
			break
		}
		fields := make(map[string]string, len(e.Fields))
		for k, v := range e.Fields {
			fields[k] = v
		}
		e.Fields = fields
		s.Items = append(s.Items, e)
		if len(s.Items) > q.Limit {
			s.Items = s.Items[1:]
			s.HasMore = true
		}
	}
	if q.After > 0 && s.HasMore {
		s.NextAfter = s.Items[len(s.Items)-1].ID
	}
	return s
}

func (b *Buffer) append(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sequence++
	e.ID = b.sequence
	raw, _ := json.Marshal(e)
	e.size = len(raw)
	for b.count > 0 && (b.count == MaxEntries || b.bytes+e.size > MaxBytes) {
		b.bytes -= b.entries[b.start].size
		b.entries[b.start] = Entry{}
		b.start = (b.start + 1) % MaxEntries
		b.count--
	}
	b.entries[(b.start+b.count)%MaxEntries] = e
	b.count++
	b.bytes += e.size
}

// NewHandler mirrors INFO and higher into memory while keeping stdout behavior.
func NewHandler(next slog.Handler, buffer *Buffer) slog.Handler {
	return &handler{next: next, buffer: buffer}
}

type boundAttr struct {
	key   string
	value slog.Value
}
type handler struct {
	next   slog.Handler
	buffer *Buffer
	attrs  []boundAttr
	group  string
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo || h.next.Enabled(ctx, level)
}
func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.next.Enabled(ctx, r.Level) {
		err = h.next.Handle(ctx, r)
	}
	if r.Level < slog.LevelInfo {
		return err
	}
	e := Entry{Time: r.Time.UTC(), Level: r.Level.String(), Message: Clip(h.buffer.redact(r.Message), 4096), Fields: map[string]string{}}
	for _, a := range h.attrs {
		captureAttr(e.Fields, a.key, a.value, 0, h.buffer.redact)
	}
	r.Attrs(func(a slog.Attr) bool { captureAttr(e.Fields, h.group+a.Key, a.Value, 0, h.buffer.redact); return true })
	h.buffer.append(e)
	return err
}

func captureAttr(fields map[string]string, key string, value slog.Value, depth int, scrub func(string) string) {
	if len(fields) >= 32 || depth > 8 {
		return
	}
	sensitive := SensitiveKey(key)
	key = Clip(scrub(key), 128)
	if sensitive {
		fields[key] = redacted
		return
	}
	value = value.Resolve()
	if value.Kind() == slog.KindGroup {
		prefix := key
		if prefix != "" {
			prefix += "."
		}
		for _, a := range value.Group() {
			captureAttr(fields, prefix+a.Key, a.Value, depth+1, scrub)
		}
		return
	}
	// Arbitrary objects may contain credentials under unlabelled fields. Keep
	// errors, which carry useful failure causes; omit other complex objects.
	if value.Kind() == slog.KindAny {
		if e, ok := value.Any().(error); ok {
			fields[key] = Clip(scrub(e.Error()), 1024)
		} else {
			fields[key] = "[object omitted]"
		}
		return
	}
	fields[key] = Clip(scrub(value.String()), 1024)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(attrs)
	c.attrs = append([]boundAttr(nil), h.attrs...)
	for _, a := range attrs {
		c.attrs = append(c.attrs, boundAttr{h.group + a.Key, a.Value.Resolve()})
	}
	return &c
}
func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.next = h.next.WithGroup(name)
	c.group += name + "."
	return &c
}
