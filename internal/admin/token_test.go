package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenBootstrapPrintsOnce(t *testing.T) {
	dir := t.TempDir()
	marker := &memoryMarker{}
	var shown []string
	show := func(token string) { shown = append(shown, token) }
	first, err := OpenCredential(context.Background(), dir, marker, show)
	if err != nil {
		t.Fatal(err)
	}
	if len(shown) != 1 || !tokenPattern.MatchString(shown[0]) {
		t.Fatalf("bootstrap must show one 64-hex token, got %d", len(shown))
	}
	if !first.Verify(shown[0]) {
		t.Fatal("the shown token does not authenticate")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenCredential(context.Background(), dir, marker, show)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if len(shown) != 1 {
		t.Fatal("a restart showed the token again")
	}
	if !second.Verify(shown[0]) {
		t.Fatal("a restart changed the token")
	}
	for _, wrong := range []string{"", strings.ToUpper(shown[0]), shown[0] + " ", shown[0][:63], strings.Repeat("0", 64)} {
		if second.Verify(wrong) {
			t.Fatalf("accepted a non-canonical or wrong token %q", wrong)
		}
	}
}

func TestTokenStoresOnlyDigest(t *testing.T) {
	dir := t.TempDir()
	var token string
	credential, err := OpenCredential(context.Background(), dir, &memoryMarker{}, func(value string) { token = value })
	if err != nil {
		t.Fatal(err)
	}
	defer credential.Close()
	data, err := os.ReadFile(filepath.Join(dir, TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("the token file holds the raw token")
	}
	digest := sha256.Sum256([]byte(token))
	if string(data) != `{"version":1,"sha256":"`+hex.EncodeToString(digest[:])+`"}` {
		t.Fatalf("unexpected token file %s", data)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.Name() != TokenFile && entry.Name() != TokenLockFile {
			t.Fatalf("leftover file %s", entry.Name())
		}
	}
}

func TestResetRefusesRunningGateway(t *testing.T) {
	dir := t.TempDir()
	marker := &memoryMarker{}
	var token string
	running, err := OpenCredential(context.Background(), dir, marker, func(value string) { token = value })
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, TokenFile))
	if _, err := ResetToken(context.Background(), dir, marker); !errors.Is(err, ErrTokenInUse) {
		t.Fatalf("reset with the gateway running: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, TokenFile))
	if string(before) != string(after) {
		t.Fatal("a refused reset changed the token file")
	}
	if err := running.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := ResetToken(context.Background(), dir, marker)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == token || !tokenPattern.MatchString(fresh) {
		t.Fatal("reset did not produce a new token")
	}
	restarted, err := OpenCredential(context.Background(), dir, marker, func(string) { t.Fatal("restart after reset showed a token") })
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.Verify(token) || !restarted.Verify(fresh) {
		t.Fatal("reset must invalidate the old token and enable the new one")
	}
}

func TestTokenBootstrapCrashWindow(t *testing.T) {
	dir := t.TempDir()
	marker := &memoryMarker{failMark: errMarkerDown}
	shown := 0
	if _, err := OpenCredential(context.Background(), dir, marker, func(string) { shown++ }); !errors.Is(err, errMarkerDown) {
		t.Fatalf("a failed marker must fail the bootstrap: %v", err)
	}
	if shown != 0 {
		t.Fatal("an unconfirmed bootstrap showed the token")
	}
	if _, err := os.Stat(filepath.Join(dir, TokenFile)); err != nil {
		t.Fatal("the file is written before the marker")
	}
	marker.failMark = nil
	credential, err := OpenCredential(context.Background(), dir, marker, func(string) { shown++ })
	if err != nil {
		t.Fatal(err)
	}
	if shown != 0 || !marker.initialized {
		t.Fatal("the next start must adopt the file and complete the marker without showing it")
	}
	_ = credential.Close()

	if err := os.Remove(filepath.Join(dir, TokenFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCredential(context.Background(), dir, marker, func(string) { shown++ }); !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("a missing file after bootstrap must require a reset: %v", err)
	}
	if shown != 0 {
		t.Fatal("a missing file was silently rotated")
	}
	if _, err := ResetToken(context.Background(), dir, marker); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, TokenFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	file, err := createPrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"version":2,"sha256":"00"}`)
	_ = file.Close()
	if _, err := OpenCredential(context.Background(), dir, marker, func(string) { shown++ }); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a corrupted file must require a reset: %v", err)
	}
}
