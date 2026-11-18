package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// projectedVolume lays a secret out as Kubernetes does: the visible entry is a
// symlink to ..data/<name>, and ..data is a symlink to a timestamped directory
// that an update replaces atomically.
func projectedVolume(t *testing.T, root, name string, content []byte, generation string) {
	t.Helper()
	dir := filepath.Join(root, "..2026_"+generation)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o440); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, "..data_tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Base(dir), tmp); err != nil {
		t.Skip("symlinks are not available to this user")
	}
	if err := os.Rename(tmp, filepath.Join(root, "..data")); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		_ = os.Remove(filepath.Join(root, "..data"))
		if err := os.Rename(tmp, filepath.Join(root, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	entry := filepath.Join(root, name)
	if _, err := os.Lstat(entry); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(filepath.Join("..data", name), entry); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProjectedSecretAtomicUpdate(t *testing.T) {
	root := t.TempDir()
	first, second := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	projectedVolume(t, root, "hmac.key", first, "01")
	got, err := ReadSharedKey("deployment.hmac_key_file", filepath.Join(root, "hmac.key"))
	if err != nil || !bytes.Equal(got, first) {
		t.Fatalf("first generation: %v", err)
	}
	projectedVolume(t, root, "hmac.key", second, "02")
	got, err = ReadSharedKey("deployment.hmac_key_file", filepath.Join(root, "hmac.key"))
	if err != nil || !bytes.Equal(got, second) {
		t.Fatalf("after the atomic swap: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "short.key"), []byte("short"), 0o440); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSharedKey("deployment.cursor_key_file", filepath.Join(root, "short.key")); !errors.Is(err, ErrKeyFile) {
		t.Fatalf("a key of the wrong size must be refused, got %v", err)
	}
}

func TestProjectedSecretRejectsEscape(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	secret := filepath.Join(outside, "stolen.key")
	if err := os.WriteFile(secret, bytes.Repeat([]byte{3}, 32), 0o400); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "hmac.key")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("symlinks are not available to this user")
	}
	_, err := ReadSharedKey("deployment.hmac_key_file", link)
	var keyErr *KeyFileError
	if !errors.As(err, &keyErr) || keyErr.Reason != "resolves outside its directory" {
		t.Fatalf("an escaping link must be refused, got %v", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte(outside)) {
		t.Fatal("the error names the target")
	}
	if runtime.GOOS != "windows" {
		writable := filepath.Join(root, "open.key")
		if err := os.WriteFile(writable, bytes.Repeat([]byte{4}, 32), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(writable, 0o666); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSharedKey("deployment.hmac_key_file", writable); !errors.Is(err, ErrKeyFile) {
			t.Fatal("a key writable by others must be refused")
		}
	}
}

func TestRedisTLSVerification(t *testing.T) {
	text := baseYAML + replaced(t, distributedYAML, "    username: comporta\n", "    username: comporta\n    tls: {enabled: true, ca_file: /run/secrets/redis/ca.crt, server_name: redis.internal}\n")
	candidate := requireValid(t, text)
	tls := candidate.cfg.Storage.Redis.TLS
	if !tls.Enabled || tls.CAFile == "" || tls.ServerName != "redis.internal" {
		t.Fatalf("TLS settings lost: %+v", tls)
	}
	requireDiagnostic(t, baseYAML+replaced(t, distributedYAML, "    username: comporta\n", "    username: comporta\n    tls: {enabled: false, ca_file: /x}\n"), CodeInvalidValue, "storage.redis.tls.enabled")
	requireDiagnostic(t, baseYAML+replaced(t, distributedYAML, "    username: comporta\n", "    username: comporta\n    tls: {skip_verify: true}\n"), CodeUnknownField, "storage.redis.tls.skip_verify")
}
