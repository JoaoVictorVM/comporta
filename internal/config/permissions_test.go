package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadableByOthersWarns(t *testing.T) {
	warnings := checkPermissions([]string{"/etc/comporta.yaml"}, func(string) (bool, error) { return true, nil })
	if len(warnings) != 1 || warnings[0].Code != CodePermissionWarning || !strings.Contains(warnings[0].Message, "readable by other users") {
		t.Fatalf("warnings: %+v", warnings)
	}
	if got := checkPermissions([]string{"/x"}, func(string) (bool, error) { return false, nil }); len(got) != 0 {
		t.Fatalf("a private path must not warn: %+v", got)
	}
	if got := checkPermissions([]string{"/x"}, func(string) (bool, error) { return false, fs.ErrNotExist }); len(got) != 0 {
		t.Fatalf("an absent path exposes nothing: %+v", got)
	}

	if runtime.GOOS == "windows" {
		return
	}
	path := filepath.Join(t.TempDir(), "comporta.yaml")
	writeFile(t, path, baseYAML)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	warnings = checkPermissions([]string{path}, inspectPermissions)
	if len(warnings) != 1 {
		t.Fatalf("mode 0644 must warn: %+v", warnings)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatal("inspection must never change permissions")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if warnings = checkPermissions([]string{path}, inspectPermissions); len(warnings) != 0 {
		t.Fatalf("mode 0600 must not warn: %+v", warnings)
	}
}

func TestPermissionInspectionUnavailableIsExplicit(t *testing.T) {
	warnings := checkPermissions([]string{"/x"}, func(string) (bool, error) { return false, errors.New("no ACL API") })
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "do not assume it is private") {
		t.Fatalf("an unavailable inspection must say so: %+v", warnings)
	}
}

func TestPermissionInspectionRunsOnThisPlatform(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	writeFile(t, path, baseYAML)
	if _, err := inspectPermissions(path); err != nil {
		t.Fatalf("inspection of an existing file: %v", err)
	}
	if _, err := inspectPermissions(filepath.Join(dir, "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an absent path must map to ErrNotExist: %v", err)
	}
}

func TestStartupWarnsAboutPermissionsWithoutFailing(t *testing.T) {
	h := newHarness(t, baseYAML, func(o *ManagerOptions) {
		o.inspect = func(string) (bool, error) { return true, nil }
	})
	logs := h.logs.String()
	if !strings.Contains(logs, "permission_warning") || !strings.Contains(logs, "readable by other users") {
		t.Fatalf("logs: %s", logs)
	}
	if h.manager.Capture().Generation() != 1 {
		t.Fatal("a permission warning must not stop startup")
	}
}
