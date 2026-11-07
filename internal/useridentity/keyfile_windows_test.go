//go:build windows

package useridentity

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
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

func TestKeyPrivateDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	mustOpen(t, dir)
	for _, name := range []string{keyFile, lockFile} {
		descriptor := namedDescriptor(t, filepath.Join(dir, name))
		private, err := daclIsPrivate(descriptor)
		if err != nil || !private {
			t.Fatalf("%s: private %v, %v", name, private, err)
		}
		control, _, err := descriptor.Control()
		if err != nil {
			t.Fatal(err)
		}
		if control&windows.SE_DACL_PROTECTED == 0 {
			t.Errorf("%s inherits entries from its directory", name)
		}
	}
}

// grantEveryone replaces the DACL with one that lets everyone read the file.
func grantEveryone(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestKeyBroadDACLRejected(t *testing.T) {
	dir := t.TempDir()
	mustOpen(t, dir)
	path := filepath.Join(dir, keyFile)
	grantEveryone(t, path)
	if _, err := Open(context.Background(), Options{Dir: dir}); !errors.Is(err, ErrKeyPermissions) {
		t.Fatalf("got %v", err)
	}
}

func TestKeyInheritedPlainFileRejected(t *testing.T) {
	dir := t.TempDir()
	// A plain write does not make a private file: chmod is no evidence on Windows.
	if err := os.WriteFile(filepath.Join(dir, keyFile), bytes.Repeat([]byte{1}, keyBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	grantEveryone(t, filepath.Join(dir, keyFile))
	if _, err := Open(context.Background(), Options{Dir: dir}); !errors.Is(err, ErrKeyPermissions) {
		t.Fatalf("got %v", err)
	}
}

// Creating a symbolic link needs a privilege the test account may not have, while a
// directory junction does not. A link reaches the handle check; a junction is refused
// earlier, when opening it as a file. Either way the service stays down and the entry
// is not treated as a missing key.
func TestKeyReparsePointRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, keyFile)
	target := filepath.Join(t.TempDir(), "elsewhere")
	writePrivate(t, target, bytes.Repeat([]byte{1}, keyBytes))
	want := ErrKeyPermissions
	if err := os.Symlink(target, path); err != nil {
		if mkErr := makeJunction(t, path); mkErr != nil {
			t.Skipf("no reparse point could be created: %v, %v", err, mkErr)
		}
		want = ErrKeyUnavailable
	}
	if _, err := Open(context.Background(), Options{Dir: dir}); !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func makeJunction(t *testing.T, path string) error {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	target, err := windows.UTF16FromString(`\??\` + t.TempDir())
	if err != nil {
		return err
	}
	size := (len(target) - 1) * 2
	buffer := make([]byte, 8+8+size+4)
	header := (*[4]uint16)(unsafe.Pointer(&buffer[8]))
	*(*uint32)(unsafe.Pointer(&buffer[0])) = windows.IO_REPARSE_TAG_MOUNT_POINT
	*(*uint16)(unsafe.Pointer(&buffer[4])) = uint16(8 + size + 4)
	header[0], header[1], header[2], header[3] = 0, uint16(size), uint16(size+2), 0
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[16])), len(target)), target)
	var returned uint32
	return windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil)
}

func TestConcurrentKeyPublicationWindows(t *testing.T) {
	dir := t.TempDir()
	const racers = 8
	path := filepath.Join(dir, keyFile)
	results := make([]error, racers)
	for i := range racers {
		writePrivate(t, filepath.Join(dir, ".hmac-racer"+string(rune('a'+i))+".tmp"), bytes.Repeat([]byte{byte(i)}, keyBytes))
	}
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			results[i] = publishNoReplace(filepath.Join(dir, ".hmac-racer"+string(rune('a'+i))+".tmp"), path)
		})
	}
	wg.Wait()
	winners := 0
	for i, err := range results {
		switch {
		case err == nil:
			winners++
			if !bytes.Equal(readKey(t, dir), bytes.Repeat([]byte{byte(i)}, keyBytes)) {
				t.Fatal("a later publication replaced the winner")
			}
		case !errors.Is(err, errAlreadyExists):
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d publications succeeded", winners)
	}
}
