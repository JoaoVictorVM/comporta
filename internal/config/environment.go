package config

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

const maxDotenvBytes = 1 << 20

// Environment is a private view of the variables captured at startup. Keeping it
// apart from the process environment means .env never leaks into child processes
// and later changes cannot alter a running gateway.
type Environment struct {
	vars map[string]string
}

// NewEnvironment builds an Environment from explicit values.
func NewEnvironment(vars map[string]string) *Environment {
	out := make(map[string]string, len(vars))
	for name, value := range vars {
		out[name] = value
	}
	return &Environment{vars: out}
}

// Lookup returns a captured variable.
func (e *Environment) Lookup(name string) (string, bool) {
	if e == nil {
		return "", false
	}
	value, ok := e.vars[name]
	return value, ok
}

// CaptureEnvironment merges .env from dir under the given process environment:
// a variable already present wins, even when empty. A missing .env is normal. A
// failure is always an *Error, whose text never includes .env contents.
func CaptureEnvironment(dir string, environ []string, fsys FileSystem) (*Environment, error) {
	if fsys == nil {
		fsys = osFileSystem{}
	}
	vars := make(map[string]string, len(environ))
	for _, entry := range environ {
		// Windows lists per-drive directories as entries with an empty name.
		if name, value, ok := strings.Cut(entry, "="); ok && name != "" {
			vars[name] = value
		}
	}

	data, err := readLimited(fsys, filepath.Join(dir, ".env"), maxDotenvBytes+1)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &Environment{vars: vars}, nil
	case err != nil:
		return nil, singleError(Diagnostic{Code: CodeInvalidEnvironment, Message: ".env cannot be read: " + classifyIOError(err)})
	case len(data) > maxDotenvBytes:
		return nil, singleError(Diagnostic{Code: CodeInvalidEnvironment, Message: ".env is too large"})
	}
	parsed, err := godotenv.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, singleError(Diagnostic{Code: CodeInvalidEnvironment, Message: ".env has invalid syntax"})
	}
	for name, value := range parsed {
		if _, present := vars[name]; !present {
			vars[name] = value
		}
	}
	return &Environment{vars: vars}, nil
}

// DetectSystemTimezone returns the IANA name of the system timezone, or "" when
// none can be identified. Windows zone names are not IANA names, so on Windows
// only TZ can provide one. The caller must then demand an explicit timezone
// instead of assuming UTC.
func DetectSystemTimezone() string {
	if name := strings.TrimPrefix(os.Getenv("TZ"), ":"); validTimezone(name) {
		return name
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, found := strings.Cut(filepath.ToSlash(target), "zoneinfo/"); found && validTimezone(name) {
			return name
		}
	}
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(data)); validTimezone(name) {
			return name
		}
	}
	return ""
}
