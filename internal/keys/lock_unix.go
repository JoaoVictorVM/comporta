//go:build !windows

package keys

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openLockFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, privateFileMode)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrStoreInsecure
		}
		return nil, err
	}
	if err := checkPrivateHandle(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func tryLock(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EWOULDBLOCK), errors.Is(err, unix.EINTR):
		return false, nil
	}
	return false, err
}
