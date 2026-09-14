package storage

import (
	"context"
	"io"
	"net/http"
	"time"
)

// PresignOp is the verb a presigned URL authorises. The set is what a node
// running against object storage it has no credentials for actually needs:
// read a body, write one, check existence, remove it.
type PresignOp string

const (
	PresignOpGet    PresignOp = "get"
	PresignOpPut    PresignOp = "put"
	PresignOpHead   PresignOp = "head"
	PresignOpDelete PresignOp = "delete"
)

// Valid reports whether op is one this package knows how to sign. Callers that
// take an op off the wire must check it before use.
func (o PresignOp) Valid() bool {
	switch o {
	case PresignOpGet, PresignOpPut, PresignOpHead, PresignOpDelete:
		return true
	}
	return false
}

// Method is the HTTP verb a caller sends to a URL signed for op.
func (o PresignOp) Method() string {
	switch o {
	case PresignOpPut:
		return http.MethodPut
	case PresignOpHead:
		return http.MethodHead
	case PresignOpDelete:
		return http.MethodDelete
	default:
		return http.MethodGet
	}
}

// Store is the abstraction over blob storage. One implementation suffices for
// AWS S3, MinIO, Cloudflare R2, Backblaze B2, and Hetzner Object Storage — all
// speak the S3 protocol. A separate Filesystem implementation covers
// self-hosters who don't want any object-storage service at all.
//
// Keys are opaque; implementations may translate to filesystem paths, S3
// object keys, etc. Callers should not assume a particular separator.
type Store interface {
	// Get returns the object's body. The caller must close the reader.
	// Returns ErrNotFound when the key doesn't exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Put writes the object. Existing objects at the same key are overwritten.
	// contentType may be empty.
	Put(ctx context.Context, key string, body io.Reader, contentType string) error

	// PutPublic writes an object meant to be served publicly (avatars, org
	// logos) and returns a stable, browser-loadable URL for it. The S3 backend
	// sets a public-read ACL + long cache and returns the object URL; the
	// filesystem backend writes the object and returns a URL under the
	// configured public base (served by the backend's /public route).
	PutPublic(ctx context.Context, key string, body io.Reader, contentType string) (string, error)

	// Delete removes the object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error

	// DeletePrefix removes every object whose key starts with prefix and
	// returns how many went. Deleting a prefix with nothing under it is not an
	// error. Implementations that cannot enumerate (the node-side brokered
	// store) return ErrUnsupported, the same contract as PresignedGetURL.
	//
	// This exists because erasing a mailbox has to erase the message bodies it
	// synced, and there can be thousands of them under one prefix. Deleting
	// them one key at a time would mean a list the caller does not have.
	//
	// Every implementation must refuse a prefix that does not end in "/" and
	// one with fewer than MinPrefixSegments segments. An empty or shallow
	// prefix is never a real caller and is always a request to empty the
	// bucket.
	DeletePrefix(ctx context.Context, prefix string) (int, error)

	// Has reports whether the key has an object. (Not named Exists because the
	// legacy S3 Client.Exists already takes a bucket argument.)
	Has(ctx context.Context, key string) (bool, error)

	// PresignedGetURL returns a time-limited URL for a third party (typically
	// the browser) to fetch the object directly. Implementations that can't
	// produce one (filesystem, in particular) should return ErrUnsupported.
	PresignedGetURL(ctx context.Context, key string, ttl time.Duration) (string, error)

	// PresignedURL is PresignedGetURL generalised to the other verbs, so a
	// caller that holds no credential for the bucket can still work against it
	// through a party that does. contentType applies to PresignOpPut and is
	// ignored otherwise. Same contract on backends that cannot sign:
	// ErrUnsupported.
	PresignedURL(ctx context.Context, op PresignOp, key, contentType string, ttl time.Duration) (string, error)

	// Name returns the implementation identifier for admin UI / audit logs.
	Name() string
}
