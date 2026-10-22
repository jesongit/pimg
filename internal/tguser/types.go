// Package tguser provides the Telegram user-session archive transport.
package tguser

import (
	"context"
	"log/slog"
	"time"
)

type Options struct {
	APIID       int
	APIHash     string
	SessionFile string
	SessionKey  []byte
	ProxyURL    string
	Logger      *slog.Logger
	OnMessage   func(Message)
	OnReady     func()      // Authorized primary connection, including reconnections.
	OnGap       func(int64) // Missing updates for a channel, or zero for a global gap.
}

type Status struct {
	State            string `json:"state"`
	Configured       bool   `json:"configured"`
	AccountID        int64  `json:"account_id,omitempty"`
	Name             string `json:"name,omitempty"`
	Error            string `json:"error,omitempty"`
	PasswordHint     string `json:"password_hint,omitempty"`
	UploadLimitBytes int64  `json:"upload_limit_bytes,omitempty"`
}

type Reference struct {
	AccountID     int64  `json:"account_id"`
	ChatID        int64  `json:"chat_id"`
	TopicID       int    `json:"topic_id"`
	MessageID     int    `json:"message_id"`
	DocumentID    int64  `json:"document_id,omitempty"`
	AccessHash    int64  `json:"access_hash,omitempty"`
	FileReference []byte `json:"file_reference,omitempty"`
	DCID          int    `json:"dc_id,omitempty"`
	Size          int64  `json:"size"`
}

type Group struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
type Topic struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Closed bool   `json:"closed"`
}
type Message struct {
	Reference Reference `json:"reference"`
	Date      int64     `json:"date"`
	Name      string    `json:"name"`
	MIME      string    `json:"mime"`
	Caption   string    `json:"caption"`
	IsService bool      `json:"is_service"`
}
type Page struct {
	Messages   []Message `json:"messages"`
	NextOffset int       `json:"next_offset"`
	Done       bool      `json:"done"`
}

// Transport is injectable for lifecycle tests; production uses an MTProto client.
type Transport interface {
	Status() Status
	Groups(context.Context) ([]Group, error)
	Topics(context.Context, int64) ([]Topic, error)
	CreateTopic(context.Context, int64, string) (Topic, error)
	RenameTopic(context.Context, int64, int, string) error
	CloseTopic(context.Context, int64, int, bool) error
	DeleteTopic(context.Context, int64, int) error
	Upload(context.Context, int64, int, string, string, []byte, []byte, string, int64) (Reference, error)
	Download(context.Context, Reference) ([]byte, error)
	Copy(context.Context, Reference, int64, int, int64) (Reference, error)
	Delete(context.Context, Reference) error
	History(context.Context, int64, int, time.Time, time.Time, int, int) (Page, error)
}
