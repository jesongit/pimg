package tguser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestUserTopicEditsTreatOnlyNotModifiedAsIdempotent(t *testing.T) {
	for _, action := range []string{"rename", "close", "reopen"} {
		for _, errorType := range []string{"TOPIC_NOT_MODIFIED", "CHAT_ADMIN_REQUIRED", "CHAT_WRITE_FORBIDDEN", "CHANNEL_PRIVATE", "CHANNEL_INVALID", "TOPIC_ID_INVALID", "TOPIC_DELETED", "TOPIC_CLOSE_SEPARATELY"} {
			t.Run(action+"/"+errorType, func(t *testing.T) {
				calls := 0
				c := fakeClient(func(_ context.Context, in bin.Encoder, _ bin.Decoder) error {
					req, ok := in.(*tg.MessagesEditForumTopicRequest)
					if !ok {
						t.Fatalf("unexpected edit RPC %T", in)
					}
					calls++
					if req.TopicID != 7 {
						t.Fatalf("wrong topic %d", req.TopicID)
					}
					title, hasTitle := req.GetTitle()
					closed, hasClosed := req.GetClosed()
					if action == "rename" {
						if !hasTitle || title != "Same name" || hasClosed {
							t.Fatal("rename changed unrelated fields")
						}
					} else if hasTitle || !hasClosed || closed != (action == "close") {
						t.Fatal("close/open flags incorrect")
					}
					return fmt.Errorf("wrapped RPC: %w", tgerr.New(400, errorType))
				})
				var err error
				if action == "rename" {
					err = c.RenameTopic(context.Background(), 41, 7, "Same name")
				} else {
					err = c.CloseTopic(context.Background(), 41, 7, action == "close")
				}
				if errorType == "TOPIC_NOT_MODIFIED" {
					if err != nil {
						t.Fatalf("matching desired state failed: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), errorType) {
					t.Fatalf("real protocol error was swallowed: %s, %v", errorType, err)
				}
				if calls != 1 {
					t.Fatalf("edit RPC called %d times", calls)
				}
			})
		}
	}
}

func TestUserTopicEditIdempotencyRetainsLocalProtection(t *testing.T) {
	c := fakeClient(func(context.Context, bin.Encoder, bin.Decoder) error {
		t.Fatal("protected topic reached Telegram")
		return nil
	})
	if err := c.RenameTopic(context.Background(), 41, 1, "Same name"); err == nil {
		t.Fatal("General renamed")
	}
	if err := c.CloseTopic(context.Background(), 41, 1, true); err == nil {
		t.Fatal("General closed")
	}
	c.channels[41].Creator = false
	if err := c.RenameTopic(context.Background(), 41, 7, "Same name"); err == nil {
		t.Fatal("missing local rights accepted")
	}
	if err := c.CloseTopic(context.Background(), 41, 7, false); err == nil {
		t.Fatal("missing local rights accepted")
	}
}

func TestUserTopicEditsPreserveCancellation(t *testing.T) {
	for _, rename := range []bool{true, false} {
		t.Run(fmt.Sprint(rename), func(t *testing.T) {
			c := fakeClient(func(context.Context, bin.Encoder, bin.Decoder) error { return context.Canceled })
			var err error
			if rename {
				err = c.RenameTopic(context.Background(), 41, 7, "Same name")
			} else {
				err = c.CloseTopic(context.Background(), 41, 7, true)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}
