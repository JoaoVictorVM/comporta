//go:build windows

package keys

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func namedDescriptor(t *testing.T, path string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func TestStoreProtectedDACL(t *testing.T) {
	store := newTestStore(t)
	mustCreate(t, store, "bot")
	mustCreate(t, store, "bot")

	trusted, err := trustedSIDs()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store.dir, store.documentPath(), store.lockPath()} {
		descriptor := namedDescriptor(t, path)
		control, _, err := descriptor.Control()
		if err != nil {
			t.Fatal(err)
		}
		if control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatalf("%s must not inherit its ACL", filepath.Base(path))
		}
		dacl, _, err := descriptor.DACL()
		if err != nil || dacl == nil {
			t.Fatalf("%s has no DACL: %v", filepath.Base(path), err)
		}
		for i := range uint32(dacl.AceCount) {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				t.Fatal(err)
			}
			if !containsSID(trusted, aceSID(ace)) {
				t.Fatalf("%s grants access to an unexpected account", filepath.Base(path))
			}
		}
	}
}

func setDACL(t *testing.T, path, sddl string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func userSDDL(t *testing.T) string {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return "(A;;FA;;;" + user.User.Sid.String() + ")"
}

func TestStoreRejectsBroadACLAndReparsePoints(t *testing.T) {
	t.Run("broad document", func(t *testing.T) {
		store := newTestStore(t)
		mustCreate(t, store, "bot")
		setDACL(t, store.documentPath(), "D:P"+userSDDL(t)+"(A;;FR;;;WD)")
		if _, err := store.List(); !errors.Is(err, ErrStoreInsecure) {
			t.Fatalf("list: %v", err)
		}
		if _, err := store.Create(context.Background(), "bot"); !errors.Is(err, ErrStoreInsecure) {
			t.Fatalf("create: %v", err)
		}
	})
	t.Run("write access counts too", func(t *testing.T) {
		store := newTestStore(t)
		mustCreate(t, store, "bot")
		setDACL(t, store.documentPath(), "D:P"+userSDDL(t)+"(A;;FW;;;AU)")
		if _, err := store.List(); !errors.Is(err, ErrStoreInsecure) {
			t.Fatalf("list: %v", err)
		}
	})
	t.Run("broad lock", func(t *testing.T) {
		store := newTestStore(t)
		mustCreate(t, store, "bot")
		setDACL(t, store.lockPath(), "D:P"+userSDDL(t)+"(A;;FR;;;WD)")
		if _, err := store.Create(context.Background(), "bot"); !errors.Is(err, ErrStoreInsecure) {
			t.Fatalf("create: %v", err)
		}
	})
	t.Run("symbolic link", func(t *testing.T) {
		store := newTestStore(t)
		mustCreate(t, store, "bot")
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.Rename(store.documentPath(), outside); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, store.documentPath()); err != nil {
			t.Skipf("creating symlinks needs a privilege this account lacks: %v", err)
		}
		if _, err := store.List(); !errors.Is(err, ErrStoreInsecure) {
			t.Fatalf("list: %v", err)
		}
	})
}

func TestReplaceExistingStoreOnWindows(t *testing.T) {
	store := newTestStore(t)
	first := mustCreate(t, store, "bot")
	if _, err := store.Revoke(context.Background(), first.Metadata.ID); err != nil {
		t.Fatalf("replacing an existing file: %v", err)
	}

	// An open reader delays the replacement, which must wait instead of failing.
	holder, err := openPrivate(store.documentPath())
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(200*time.Millisecond, func() { _ = holder.Close() })
	if _, err := store.Create(context.Background(), "bot"); err != nil {
		t.Fatalf("a concurrent reader must not fail a commit: %v", err)
	}
	if !strings.Contains(readFile(t, store.documentPath()), `"revision": 3`) {
		t.Fatal("the commit was not published")
	}
}

func aceSID(ace *windows.ACCESS_ALLOWED_ACE) *windows.SID {
	return (*windows.SID)(unsafe.Pointer(&ace.SidStart))
}
