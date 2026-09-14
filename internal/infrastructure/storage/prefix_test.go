package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DeletePrefix("") empties the bucket, and there is no caller that wants that.
// Every rejection here is one keystroke away from being a real call.
func TestCheckPrefixRefusesAnythingTooBroad(t *testing.T) {
	for _, prefix := range []string{
		"",
		"/",
		"users/",
		"users/abc/",
		"users/abc/emails/",           // three segments: the whole mailbox list
		"users/abc/emails/def",        // no trailing slash: matches def-other/ too
		"users/abc/../../emails/def/", // traversal
		"users//emails/def/",
	} {
		if err := CheckPrefix(prefix); err == nil {
			t.Errorf("CheckPrefix(%q) allowed a prefix that is not one mailbox's", prefix)
		}
	}
}

// The shape the erasure job actually passes.
func TestCheckPrefixAllowsOneMailbox(t *testing.T) {
	prefix := "users/11111111-1111-1111-1111-111111111111/emails/22222222-2222-2222-2222-222222222222/"
	if err := CheckPrefix(prefix); err != nil {
		t.Fatalf("CheckPrefix(%q) = %v, want the mailbox prefix to be erasable", prefix, err)
	}
}

// A trailing slash is the difference between erasing one mailbox and erasing
// every mailbox whose id starts with the same characters. S3 prefixes are
// string matches, not paths.
func TestPrefixWithoutATrailingSlashIsRefused(t *testing.T) {
	if err := CheckPrefix("users/u/emails/abc"); err != ErrUnsafePrefix {
		t.Errorf("err = %v, want ErrUnsafePrefix", err)
	}
}

func TestFilesystemDeletePrefixRemovesOnlyThatMailbox(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystem(root, "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	mine := "users/alice/emails/box-1/"
	for _, key := range []string{mine + "a.emsg", mine + "b.emsg", mine + "c.emsg"} {
		if err := store.Put(ctx, key, strings.NewReader("body"), ""); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	// A sibling mailbox of the same person, and one whose id shares a prefix
	// with the one being erased: both must survive.
	neighbours := []string{"users/alice/emails/box-2/a.emsg", "users/alice/emails/box-10/a.emsg"}
	for _, key := range neighbours {
		if err := store.Put(ctx, key, strings.NewReader("body"), ""); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	n, err := store.DeletePrefix(ctx, mine)
	if err != nil {
		t.Fatalf("delete prefix: %v", err)
	}
	if n != 3 {
		t.Errorf("reported %d objects erased, want 3", n)
	}
	if _, err := os.Stat(filepath.Join(root, "users", "alice", "emails", "box-1")); !os.IsNotExist(err) {
		t.Error("the mailbox's directory is still there")
	}
	for _, key := range neighbours {
		has, err := store.Has(ctx, key)
		if err != nil || !has {
			t.Errorf("%s went with a different mailbox's erasure", key)
		}
	}
}

// Erasing a mailbox that never stored a body is done, not an error. Without
// this the erasure would stay outstanding forever for every mailbox that was
// disconnected before it finished its first sync.
func TestFilesystemDeletePrefixOfNothingSucceeds(t *testing.T) {
	store, err := NewFilesystem(t.TempDir(), "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	n, err := store.DeletePrefix(context.Background(), "users/nobody/emails/none/")
	if err != nil {
		t.Fatalf("delete prefix: %v", err)
	}
	if n != 0 {
		t.Errorf("erased %d objects from an empty prefix", n)
	}
}

// The node-side store holds no credential for the bucket and asks the control
// plane to sign one key at a time. A prefix delete has no such shape, so it
// must refuse rather than appear to work.
func TestBrokeredStoreCannotDeleteAPrefix(t *testing.T) {
	var s Store = &BrokeredStore{}
	if _, err := s.DeletePrefix(context.Background(), "users/a/emails/b/"); err != ErrUnsupported {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

// The backend probes its store at boot with the empty prefix to find out
// whether it can erase a mailbox at all, so the two refusals have to stay
// distinguishable: a real backend answers ErrUnsafePrefix without touching
// anything, and only the brokered one answers ErrUnsupported. If BrokeredStore
// ever validated the prefix first, both would answer ErrUnsafePrefix and the
// boot guard in cmd/backend would silently stop catching the misconfiguration
// it exists for.
func TestTheEmptyPrefixProbeTellsBrokeredApartFromReal(t *testing.T) {
	ctx := context.Background()

	fs, err := NewFilesystem(t.TempDir(), "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := fs.DeletePrefix(ctx, ""); err != ErrUnsafePrefix {
		t.Errorf("filesystem probe = %v, want ErrUnsafePrefix (a real store that can erase)", err)
	}

	var brokered Store = &BrokeredStore{}
	if _, err := brokered.DeletePrefix(ctx, ""); err != ErrUnsupported {
		t.Errorf("brokered probe = %v, want ErrUnsupported (a store that cannot erase)", err)
	}
}
