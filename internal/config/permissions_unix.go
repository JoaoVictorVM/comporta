//go:build !windows

package config

import "os"

const groupOtherReadOrAccess = 0o055

func inspectPermissions(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&groupOtherReadOrAccess != 0, nil
}
