package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FilesystemStore is a Store backed by a local directory. Suitable for
// single-node self-hosted deployments. Not suitable for multi-node setups
// without shared storage (NFS, etc.).
//
// Object keys are translated to file paths under Root. Any ".." path component
// is rejected to prevent directory traversal.
type FilesystemStore struct {
	root string
	// publicBaseURL is prefixed onto keys by PutPublic so avatars/logos resolve
	// through the backend's /public route. Defaults to "/public" (same-origin)
	// when unset; operators with a split app/API origin set an absolute URL.
	publicBaseURL string
}

// NewFilesystem returns a FilesystemStore rooted at the given directory. The
// directory is created (recursively) if it doesn't exist. publicBaseURL is the
// URL prefix used for publicly-served objects; pass "" to default to /public.
func NewFilesystem(root, publicBaseURL string) (*FilesystemStore, error) {
	if root == "" {
		return nil, errors.New("filesystem store: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("filesystem store: abs path: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("filesystem store: mkdir: %w", err)
	}
	if publicBaseURL == "" {
		publicBaseURL = "/public"
	}
	return &FilesystemStore{root: abs, publicBaseURL: strings.TrimRight(publicBaseURL, "/")}, nil
}

func (s *FilesystemStore) Name() string { return "filesystem" }

func (s *FilesystemStore) resolve(key string) (string, error) {
	if key == "" {
		return "", errors.New("filesystem store: empty key")
	}
	// Reject '..' as a literal path component before normalization — otherwise
	// "a/../b" silently maps to "b" and collides with the literal key "b".
	for _, p := range strings.Split(key, "/") {
		if p == ".." {
			return "", errors.New("filesystem store: '..' not allowed in key")
		}
	}
	clean := filepath.Clean("/" + key)
	return filepath.Join(s.root, clean), nil
}

func (s *FilesystemStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	path, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

// Put writes atomically: stream to a temp file, fsync, then rename. Crashes
// mid-write leave the destination unchanged.
func (s *FilesystemStore) Put(_ context.Context, key string, body io.Reader, _ string) error {
	path, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".put-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once rename succeeds

	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// PutPublic writes the object and returns a URL under the configured public
// base. The backend serves these via its /public/*key route (see routes.go).
func (s *FilesystemStore) PutPublic(ctx context.Context, key string, body io.Reader, contentType string) (string, error) {
	if err := s.Put(ctx, key, body, contentType); err != nil {
		return "", err
	}
	return s.publicBaseURL + "/" + key, nil
}

func (s *FilesystemStore) Delete(_ context.Context, key string) error {
	path, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *FilesystemStore) Has(_ context.Context, key string) (bool, error) {
	path, err := s.resolve(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// PresignedGetURL is not supported by the filesystem backend — there's no
// authority to sign URLs against. Callers should fall back to streaming the
// object through the application.
func (s *FilesystemStore) PresignedGetURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", ErrUnsupported
}

// PresignedURL is unsupported for the same reason, for every verb. A node on
// another machine therefore cannot reach filesystem blobs at all, which is the
// honest answer: they are on a disk it does not have.
func (s *FilesystemStore) PresignedURL(_ context.Context, _ PresignOp, _, _ string, _ time.Duration) (string, error) {
	return "", ErrUnsupported
}

// DeletePrefix removes the directory a prefix maps to and everything under it,
// reporting how many files went. Keys are paths here, so a prefix is a
// directory and one RemoveAll finishes it.
func (s *FilesystemStore) DeletePrefix(_ context.Context, prefix string) (int, error) {
	if err := CheckPrefix(prefix); err != nil {
		return 0, err
	}
	dir, err := s.resolve(strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return 0, err
	}
	// Never let a resolve bug turn into "erase the store". resolve already
	// rejects "..", so this can only fire if root itself was handed in.
	if dir == s.root || !strings.HasPrefix(dir, s.root+string(os.PathSeparator)) {
		return 0, ErrUnsafePrefix
	}

	count := 0
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return 0, err
	}
	return count, nil
}
