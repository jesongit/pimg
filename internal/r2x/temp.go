package r2x

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"pimg/internal/tempcdn"
)

func (c *Client) TempPublicURL(key string) string { return c.tempBase + "/" + key }

func (c *Client) checkPublicTemp() error {
	if c.tempBase == "" || c.cleanupBucket == "" || c.cleanupBucket == c.TempBucket || c.cleanupBucket == c.PermBucket || c.cdnToken == "" || (c.tempZone == "" && c.cdnZoneID == "") {
		return errors.New("临时公开外链及云端清理配置不完整")
	}
	return nil
}

func (c *Client) registerCleanup(ctx context.Context, key string, due int64) error {
	natural, ok := tempcdn.Expiry(key)
	if !ok || due <= 0 || due > natural {
		return errors.New("无效的临时对象清理记录")
	}
	body, _ := json.Marshal(map[string]any{"version": 1, "object_key": key, "expires_at": due})
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.cleanupBucket), Key: aws.String(tempcdn.JobKey(key, due)),
		Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
		ContentType: aws.String("application/json"), CacheControl: aws.String("private, no-store"),
		Metadata: map[string]string{"version": "1", "object_key": key, "expires_at": strconv.FormatInt(due, 10)},
	})
	return err
}

// PutPublicTemp durably registers cleanup before uploading any public plaintext.
// The timestamp in the data key also lets the Worker discover late/orphan PUTs.
func (c *Client) PutPublicTemp(ctx context.Context, key string, body io.Reader, size int64, mime string) error {
	if err := c.checkPublicTemp(); err != nil {
		return err
	}
	expires, ok := tempcdn.Expiry(key)
	if !ok || expires <= time.Now().Unix() {
		return errors.New("临时对象已过期或路径无效")
	}
	ctx, cancel := context.WithDeadline(ctx, time.Unix(expires, 0))
	defer cancel()
	if err := c.registerCleanup(ctx, key, expires); err != nil {
		return fmt.Errorf("写入云端清理记录失败: %w", err)
	}
	ttl := c.tempCacheSeconds
	if ttl <= 0 {
		ttl = 3600
	}
	ttl = min(ttl, int(expires-time.Now().Unix()))
	if ttl <= 0 {
		return errors.New("临时对象已过期")
	}
	cache := fmt.Sprintf("public, max-age=0, s-maxage=%d, must-revalidate", ttl)
	return c.PutObject(ctx, c.TempBucket, key, body, size, mime, cache)
}

// DeletePublicTemp is for explicit deletion. Scheduled expiry is owned by the
// Worker; the durable record survives either S3 deletion or CDN purge failures.
func (c *Client) DeletePublicTemp(ctx context.Context, key string) error {
	if err := c.checkPublicTemp(); err != nil {
		return err
	}
	expires, ok := tempcdn.Expiry(key)
	if !ok {
		return errors.New("无效的临时对象路径")
	}
	due := min(expires, time.Now().Unix())
	if err := c.registerCleanup(ctx, key, due); err != nil {
		return err
	}
	if err := c.Delete(ctx, c.TempBucket, key); err != nil {
		return err
	}
	zone := c.tempZone
	if zone == "" {
		zone = c.cdnZoneID
	}
	if err := c.purgeCDN(ctx, zone, []string{c.TempPublicURL(key)}); err != nil {
		return err
	}
	if err := c.Delete(ctx, c.cleanupBucket, tempcdn.JobKey(key, due)); err != nil {
		return err
	}
	if due != expires {
		return c.Delete(ctx, c.cleanupBucket, tempcdn.JobKey(key, expires))
	}
	return nil
}
