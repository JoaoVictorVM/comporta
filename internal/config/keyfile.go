package config

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrKeyFile is the fixed failure of a shared key file. It never carries the
// path contents or the bytes read.
var ErrKeyFile = errors.New("shared key file is unusable")

// KeyFileError names the field and a fixed reason, never the value.
type KeyFileError struct {
	Field  string
	Reason string
}

func (e *KeyFileError) Error() string { return e.Field + ": " + e.Reason }
func (e *KeyFileError) Is(target error) bool {
	return target == ErrKeyFile
}

// ReadSharedKey reads a key of exactly 32 bytes from a file that may be a
// Kubernetes projected volume entry. Those volumes swap a "..data" symlink
// atomically, so the entry is a symlink chain inside its own directory. The
// chain is resolved, and the final file must stay inside the directory of
// the configured path: a link that escapes it is refused rather than
// followed. The file must be regular and not writable by group or others.
func ReadSharedKey(field, path string) ([]byte, error) {
	fail := func(reason string) ([]byte, error) { return nil, &KeyFileError{Field: field, Reason: reason} }
	root, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return fail("directory is unreadable")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fail("file is unreadable")
	}
	if !within(root, resolved) {
		return fail("resolves outside its directory")
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return fail("file is unreadable")
	}
	if !info.Mode().IsRegular() {
		return fail("not a regular file")
	}
	if writableByOthers(info) {
		return fail("is writable by other users")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return fail("file is unreadable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 33))
	if err != nil {
		return fail("file is unreadable")
	}
	if len(data) != 32 {
		return fail("must contain exactly 32 bytes")
	}
	return data, nil
}

func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
