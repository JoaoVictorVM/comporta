//go:build !windows

package sqlite

import "os"

func checkDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errInsecure
	}
	return nil
}
