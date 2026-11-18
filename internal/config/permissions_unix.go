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

// writableByOthers reports a mode that lets the group or others replace the
// bytes of a shared key. A projected volume uses 0440 or 0400.
func writableByOthers(info os.FileInfo) bool { return info.Mode().Perm()&0o022 != 0 }
