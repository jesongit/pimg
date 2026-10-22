package main

import (
	"net/http"
	"time"

	"pimg/internal/config"
	"pimg/internal/r2x"
	"pimg/internal/storagehttp"
	"pimg/internal/tgx"
)

// storageClients is shared by the server and CLI recovery commands. Only these
// clients receive the storage proxy; http.DefaultTransport is never modified.
func storageClients(cfg *config.Config) (*tgx.Client, *r2x.Client, error) {
	transport, err := storagehttp.NewTransport(cfg.Storage.ProxyURL)
	if err != nil {
		return nil, nil, err
	}
	// The Bot HTTP transport is only retained by the isolated development mock.
	// Production archives use the Telegram user session owned by store.Service.
	var tg *tgx.Client
	if cfg.DevMock {
		tg = tgx.New(cfg.Telegram.APIBase, cfg.Telegram.BotToken, cfg.Telegram.ChannelID)
		tg.SetHTTPClient(&http.Client{Transport: transport, Timeout: 10 * time.Minute})
	}
	r2 := r2x.New(cfg.R2.Endpoint, cfg.R2.AccessKeyID, cfg.R2.SecretAccessKey,
		cfg.R2.TempBucket, cfg.R2.PermBucket, cfg.R2.PublicBaseURL,
		cfg.R2.CDNZoneID, cfg.R2.CDNAPIToken, r2x.WithHTTPTransport(transport),
		r2x.WithPublicTemp(cfg.R2.TempPublicBaseURL, cfg.R2.TempCleanupBucket, cfg.R2.TempCDNZoneID, cfg.R2.TempCacheSeconds))
	return tg, r2, nil
}
