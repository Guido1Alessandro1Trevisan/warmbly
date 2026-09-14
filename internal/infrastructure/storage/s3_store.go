package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// High-level Store methods on *Client. These let callers depend on the
// storage.Store interface so the same code works against AWS S3, MinIO,
// Cloudflare R2, Backblaze B2, Hetzner Object Storage, or the filesystem
// backend without conditional logic.
//
// The lower-level Client.GetObject / PutObject / etc. (via embedded *s3.Client)
// remain available for existing call sites that haven't been migrated yet.

func (c *Client) Name() string { return "s3" }

func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return out.Body, nil
}

func (c *Client) Put(ctx context.Context, key string, body io.Reader, contentType string) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(c.Bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	_, err := c.PutObject(ctx, in)
	return err
}

// PutPublic writes a public-read object with a long immutable cache and
// returns its public URL. Used for avatars and org logos.
func (c *Client) PutPublic(ctx context.Context, key string, body io.Reader, contentType string) (string, error) {
	in := &s3.PutObjectInput{
		Bucket:       aws.String(c.Bucket),
		Key:          aws.String(key),
		Body:         body,
		CacheControl: aws.String("public, max-age=31536000, immutable"),
		ACL:          types.ObjectCannedACLPublicRead,
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if _, err := c.PutObject(ctx, in); err != nil {
		return "", err
	}
	if c.PublicBaseURL != "" {
		return strings.TrimRight(c.PublicBaseURL, "/") + "/" + key, nil
	}
	return fmt.Sprintf("https://%s.s3.amazonaws.com/%s", c.Bucket, key), nil
}

func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.Bucket),
		Key:    aws.String(key),
	})
	return err
}

func (c *Client) Has(ctx context.Context, key string) (bool, error) {
	return c.Exists(ctx, c.Bucket, key)
}

func (c *Client) PresignedGetURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return c.PresignedURL(ctx, PresignOpGet, key, "", ttl)
}

// PresignedURL signs one operation on one key. This is what lets a worker hold
// no bucket credentials at all: the control plane signs, the node sends the
// bytes straight to the store, and nothing is proxied.
//
// A signature covers the verb, so a URL minted for a read cannot be used to
// overwrite.
func (c *Client) PresignedURL(ctx context.Context, op PresignOp, key, contentType string, ttl time.Duration) (string, error) {
	ps := s3.NewPresignClient(c.Client)
	expires := s3.WithPresignExpires(ttl)

	switch op {
	case PresignOpGet:
		out, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(c.Bucket),
			Key:    aws.String(key),
		}, expires)
		if err != nil {
			return "", err
		}
		return out.URL, nil

	case PresignOpPut:
		in := &s3.PutObjectInput{
			Bucket: aws.String(c.Bucket),
			Key:    aws.String(key),
		}
		// Signed when present, so the sender must send the same header back.
		// Left unsigned when empty rather than defaulted, which would make
		// every unspecified upload fail the signature check.
		if contentType != "" {
			in.ContentType = aws.String(contentType)
		}
		out, err := ps.PresignPutObject(ctx, in, expires)
		if err != nil {
			return "", err
		}
		return out.URL, nil

	case PresignOpHead:
		out, err := ps.PresignHeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(c.Bucket),
			Key:    aws.String(key),
		}, expires)
		if err != nil {
			return "", err
		}
		return out.URL, nil

	case PresignOpDelete:
		out, err := ps.PresignDeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(c.Bucket),
			Key:    aws.String(key),
		}, expires)
		if err != nil {
			return "", err
		}
		return out.URL, nil

	default:
		return "", fmt.Errorf("storage: cannot presign unknown op %q", op)
	}
}

// DeletePrefix removes every object under prefix, in pages, and reports how
// many went. A mailbox's bodies are one object per message, so a busy mailbox
// is thousands of keys; DeleteObjects takes a thousand at a time.
//
// Errors stop the walk rather than being collected: the caller retries the
// whole prefix, and a partially-erased prefix that reported success would be
// recorded as erased with bytes still in the bucket.
func (c *Client) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if err := CheckPrefix(prefix); err != nil {
		return 0, err
	}

	deleted := 0
	pager := s3.NewListObjectsV2Paginator(c.Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.Bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return deleted, err
		}
		if len(page.Contents) == 0 {
			continue
		}
		ids := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			ids = append(ids, types.ObjectIdentifier{Key: obj.Key})
		}
		out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(c.Bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return deleted, err
		}
		// A 200 carrying per-key errors is the shape S3 uses for a partial
		// failure; without this the caller is told the prefix is clean while
		// some of the customer's mail is still in the bucket.
		if len(out.Errors) > 0 {
			first := out.Errors[0]
			return deleted, fmt.Errorf("storage: %d of %d objects under %q could not be deleted: %s",
				len(out.Errors), len(ids), prefix, aws.ToString(first.Message))
		}
		// Quiet mode returns only failures, and there were none.
		deleted += len(ids)
	}
	return deleted, nil
}
