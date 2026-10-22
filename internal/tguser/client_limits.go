package tguser

import (
	"context"
	"math"
	"time"

	"github.com/gotd/td/tg"
)

// Telegram's ordinary-account fallback is 4000 protocol parts of 512 KiB.
// Query the account's current app configuration rather than guessing 2^31.
const defaultUploadLimit int64 = 4000 * 512 * 1024

func (c *Client) refreshUploadLimit(ctx context.Context, api *tg.Client, account int64, premium bool) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := api.HelpGetAppConfig(ctx, 0)
	if err != nil {
		return
	}
	full, ok := res.(*tg.HelpAppConfig)
	if !ok {
		return
	}
	obj, ok := full.Config.(*tg.JSONObject)
	if !ok {
		return
	}
	key := "upload_max_fileparts_default"
	if premium {
		key = "upload_max_fileparts_premium"
	}
	limit := defaultUploadLimit
	for _, v := range obj.Value {
		if v.Key == key {
			if n, ok := v.Value.(*tg.JSONNumber); ok && n.Value > 0 && n.Value <= 1<<20 && math.Trunc(n.Value) == n.Value {
				limit = int64(n.Value) * 512 * 1024
			}
			break
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.api == api && c.status.AccountID == account {
		c.status.UploadLimitBytes = limit
	}
}
