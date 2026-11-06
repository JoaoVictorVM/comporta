package config

import (
	"errors"
	"io/fs"
)

// permissionInspector reports whether users other than the owner can read path.
type permissionInspector func(path string) (exposed bool, err error)

// checkPermissions produces warnings only: it neither invalidates a configuration
// nor changes permissions, which remain a decision of the operator. A path that
// does not exist yet, such as a data directory before first use, exposes nothing.
func checkPermissions(paths []string, inspect permissionInspector) []Diagnostic {
	var out []Diagnostic
	for _, path := range paths {
		exposed, err := inspect(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			out = append(out, Diagnostic{
				Code:    CodePermissionWarning,
				Message: "cannot verify access by other users to " + displayPath(path) + "; do not assume it is private",
			})
		case exposed:
			out = append(out, Diagnostic{
				Code:    CodePermissionWarning,
				Message: displayPath(path) + " is readable by other users",
			})
		}
	}
	return out
}
