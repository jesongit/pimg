// Package r2x 封装 Cloudflare R2（S3 兼容）访问：双桶隔离、流式上传、
// 预签名短链、CDN 缓存清理。永久桶对象经独立公开域名 + CDN 直接访问，
// 图床后端不参与永久读取链路。
package r2x

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Client struct {
	s3                                *s3.Client
	presign                           *s3.PresignClient
	TempBucket                        string
	PermBucket                        string
	PublicBase                        string
	cdnZoneID                         string
	cdnToken                          string
	hc                                *http.Client
	tempBase, cleanupBucket, tempZone string
	tempCacheSeconds                  int
}

type clientOptions struct {
	transport                         http.RoundTripper
	tempBase, cleanupBucket, tempZone string
	tempCacheSeconds                  int
}

func WithPublicTemp(base, cleanupBucket, zone string, cacheSeconds int) Option {
	return func(o *clientOptions) {
		o.tempBase, o.cleanupBucket, o.tempZone, o.tempCacheSeconds = base, cleanupBucket, zone, cacheSeconds
	}
}

type Option func(*clientOptions)

// WithHTTPTransport routes S3 operations and CDN purges through the same storage
// transport while retaining their separate request timeouts.
func WithHTTPTransport(transport http.RoundTripper) Option {
	return func(o *clientOptions) { o.transport = transport }
}

// New 用 R2 配置构造客户端。
func New(endpoint, accessKey, secret, tempBucket, permBucket, publicBase, cdnZoneID, cdnToken string, options ...Option) *Client {
	var opts clientOptions
	for _, option := range options {
		option(&opts)
	}
	cfg := aws.Config{
		Region:      "auto",
		Credentials: credentials.NewStaticCredentialsProvider(accessKey, secret, ""),
		// R2 对部分新校验和头支持有限：仅在请求显式要求时才计算/校验。
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if opts.transport != nil {
		cfg.HTTPClient = &http.Client{Transport: opts.transport}
	}
	cl := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	return &Client{
		s3:         cl,
		presign:    s3.NewPresignClient(cl),
		TempBucket: tempBucket,
		PermBucket: permBucket,
		PublicBase: publicBase,
		cdnZoneID:  cdnZoneID,
		cdnToken:   cdnToken,
		tempBase:   opts.tempBase, cleanupBucket: opts.cleanupBucket, tempZone: opts.tempZone, tempCacheSeconds: opts.tempCacheSeconds,
		hc: &http.Client{Transport: opts.transport, Timeout: 30 * time.Second},
	}
}

// PutObject 流式上传（size 已知，不整份驻留内存）。
func (c *Client) PutObject(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType, cacheControl string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	in := &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	}
	if cacheControl != "" {
		in.CacheControl = aws.String(cacheControl)
	}
	_, err := c.s3.PutObject(ctx, in, func(o *s3.Options) {
		// Complete originals can be streamed from a PNG header + encrypted file
		// or another provider. SigV4 still signs the request; payload integrity is
		// checked by the archive read-back instead of buffering/rewinding the body.
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
	})
	return err
}

// HeadSize 确认对象存在并返回大小。
func (c *Client) HeadSize(ctx context.Context, bucket, key string) (int64, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		return 0, err
	}
	if out.ContentLength == nil {
		return 0, fmt.Errorf("HEAD %s 未返回大小", key)
	}
	return *out.ContentLength, nil
}

// Get 流式读取对象。
func (c *Client) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

// Delete 删除对象（幂等：不存在视为成功）。
func (c *Client) Delete(ctx context.Context, bucket, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	return err
}

// PresignGet 生成短期预签名 GET 链接；cacheControl 随签名下发，禁止持久缓存。
func (c *Client) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration, cacheControl string) (string, error) {
	in := &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	}
	if cacheControl != "" {
		in.ResponseCacheControl = aws.String(cacheControl)
	}
	out, err := c.presign.PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

// PublicURL 永久对象的公开 URL。
func (c *Client) PublicURL(key string) string { return c.PublicBase + "/" + key }

// TempCacheControl 临时对象统一响应头。
const TempCacheControl = "private, max-age=0, no-store"

// SetCDNHTTPClient permits a controlled transport for connectivity testing.
func (c *Client) SetCDNHTTPClient(hc *http.Client) { c.hc = hc }

// PurgeCDN returns failures to the caller so revocation can be retried.
func (c *Client) PurgeCDN(ctx context.Context, urls []string) error {
	if c.cdnZoneID == "" || c.cdnToken == "" || len(urls) == 0 {
		return nil
	}
	return c.purgeCDN(ctx, c.cdnZoneID, urls)
}

func (c *Client) purgeCDN(ctx context.Context, zone string, urls []string) error {
	payload, _ := json.Marshal(map[string]any{"files": urls})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/purge_cache", zone),
		bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cdnToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("CDN 清理失败: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if !out.Success {
		return fmt.Errorf("CDN 清理失败: %v", out.Errors)
	}
	return nil
}
