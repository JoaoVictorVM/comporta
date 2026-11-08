//go:build !windows

package sqlite

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	privateDirMode  = 0o700
	privateFileMode = 0o600
	broaderThanUser = 0o077
)

// ensurePrivateDir never touches the mode of a directory that already exists:
// that remains a decision of the operator.
func ensurePrivateDir(dir string) error {
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return errInsecure
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), privateDirMode); err != nil {
		return err
	}
	if err := os.Mkdir(dir, privateDirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

func createPrivateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, privateFileMode)
}

// openPrivate opens without following links and checks the opened handle, so the
// file inspected is the file read. O_NONBLOCK keeps a planted FIFO from hanging.
func openPrivate(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, errInsecure
		}
		return nil, err
	}
	if err := checkPrivateHandle(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func checkPrivateHandle(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&broaderThanUser != 0 {
		return errInsecure
	}
	return nil
}

func replaceFile(from, to string) error {
	return os.Rename(from, to)
}

// syncDir makes the rename durable. Some filesystems refuse to sync directories;
// they offer no stronger guarantee, so that refusal is not a failure.
func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	if err := handle.Sync(); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	return nil
}

func isTransientOpenError(error) bool { return false }
