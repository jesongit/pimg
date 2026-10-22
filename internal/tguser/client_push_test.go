package tguser

import (
	"context"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"testing"
)

func TestReadyNotifiesOncePerAuthorizedConnectionWithoutHoldingClientLock(t *testing.T) {
	c := New(Options{})
	calls := 0
	c.opts.OnReady = func() {
		calls++
		if c.Status().State != "authorized" {
			t.Fatal("callback before authorization")
		}
	}
	user := &tg.User{ID: 77, FirstName: "fixture"}
	if err := c.authorized(user); err != nil {
		t.Fatal(err)
	}
	if err := c.authorized(user); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	c.connectionState(telegram.ConnectionStateConnecting)
	c.connectionState(telegram.ConnectionStateReady)
	if err := c.authorized(user); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("reconnection did not notify", calls)
	}
	if err := c.authorized(&tg.User{ID: 999}); err == nil {
		t.Fatal("wrong account accepted")
	}
	if calls != 2 {
		t.Fatal("wrong identity triggered catch-up")
	}
}

func TestServerUpdateGapRequestsCatchupWithoutMessage(t *testing.T) {
	c := New(Options{})
	calls := 0
	c.opts.OnGap = func(int64) { calls++ }
	for _, updates := range []tg.UpdatesClass{&tg.UpdatesTooLong{}, &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateChannelTooLong{ChannelID: 41}}}, &tg.UpdateShort{Update: &tg.UpdatePtsChanged{}}} {
		if err := c.handleUpdates(context.Background(), updates); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}
