package r2x

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type ListedObject struct {
	Key      string
	Size     int64
	ETag     string
	Modified int64
}
type ObjectPage struct {
	Objects []ListedObject
	Next    string
}

func (c *Client) List(ctx context.Context, bucket, prefix, cursor string, limit int32) (ObjectPage, error) {
	in := &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(limit)}
	if cursor != "" {
		in.ContinuationToken = aws.String(cursor)
	}
	o, e := c.s3.ListObjectsV2(ctx, in)
	if e != nil {
		return ObjectPage{}, e
	}
	p := ObjectPage{Next: aws.ToString(o.NextContinuationToken)}
	for _, v := range o.Contents {
		m := int64(0)
		if v.LastModified != nil {
			m = v.LastModified.Unix()
		}
		p.Objects = append(p.Objects, ListedObject{aws.ToString(v.Key), aws.ToInt64(v.Size), aws.ToString(v.ETag), m})
	}
	return p, nil
}
func (c *Client) ObjectVersion(ctx context.Context, bucket, key string) (string, error) {
	o, e := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if e != nil {
		return "", e
	}
	return fmt.Sprintf("%s:%d", aws.ToString(o.ETag), aws.ToInt64(o.ContentLength)), nil
}
func (c *Client) DeleteManagedTemp(ctx context.Context, key string) error {
	if e := c.Delete(ctx, c.TempBucket, key); e != nil {
		return e
	}
	zone := c.tempZone
	if zone == "" {
		zone = c.cdnZoneID
	}
	if zone != "" && c.cdnToken != "" && c.tempBase != "" {
		return c.purgeCDN(ctx, zone, []string{c.TempPublicURL(key)})
	}
	return nil
}
