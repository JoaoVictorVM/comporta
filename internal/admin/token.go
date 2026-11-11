// Package admin implements the operator credential, the admin HTTP API and the
// services behind it. The services do not depend on HTTP, so the future panel
// and the CLI share them.
package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

const (
	// TokenFile holds only the digest of the admin token.
	TokenFile = "admin-token.json"
	// TokenLockFile is held by the running gateway and by a reset.
	TokenLockFile = "admin-token.lock"

	tokenBytes    = 32
	maxTokenFile  = 4 << 10
	readAttempts  = 5
	readRetryWait = 20 * time.Millisecond
)

var (
	errInsecure = errors.New("admin token file is not private")
	// ErrTokenMissing means the gateway was initialized before and its admin
	// token file is gone: the operator has to reset it explicitly.
	ErrTokenMissing = errors.New("admin token file is missing; stop the gateway and run \"comporta admin reset-token\"")
	// ErrTokenInvalid means the token file cannot be trusted.
	ErrTokenInvalid = errors.New("admin token file is invalid; stop the gateway and run \"comporta admin reset-token\"")
	// ErrTokenInUse means another process holds the admin token lock.
	ErrTokenInUse = errors.New("the admin token is in use by a running gateway; stop it before resetting the token")
	errTokenIO    = errors.New("admin token file could not be written")

	tokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// TemporaryTokenFile matches the private temporary files of an atomic write.
	TemporaryTokenFile = regexp.MustCompile(`^\.admin-token-[0-9a-f]{32}\.tmp$`)
)

type tokenDocument struct {
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
}

// Credential verifies the admin token. It keeps the token lock for the life of
// the gateway, so an offline reset cannot race with a running service.
type Credential struct {
	digest [sha256.Size]byte
	lock   *os.File
}

// Verify compares in constant time. Only the canonical form of a token is
// accepted, so no two strings authenticate as the same token.
func (c *Credential) Verify(token string) bool {
	if c == nil || !tokenPattern.MatchString(token) {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(digest[:], c.digest[:]) == 1
}

// Close releases the token lock.
func (c *Credential) Close() error {
	if c == nil || c.lock == nil {
		return nil
	}
	err := c.lock.Close()
	c.lock = nil
	return err
}

// OpenCredential loads the admin credential for a gateway that already holds the
// instance lock. On the first run it creates the token and hands it to show,
// exactly once. A token file written by a run that crashed before recording the
// marker is reused without being shown again.
func OpenCredential(ctx context.Context, dir string, marker storage.AdminMarker, show func(token string)) (*Credential, error) {
	lock, err := acquireTokenLock(dir)
	if err != nil {
		return nil, err
	}
	credential, err := openLocked(ctx, dir, marker, show)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	credential.lock = lock
	return credential, nil
}

func openLocked(ctx context.Context, dir string, marker storage.AdminMarker, show func(string)) (*Credential, error) {
	initialized, err := marker.AdminInitialized(ctx)
	if err != nil {
		return nil, err
	}
	digest, err := readDigest(dir)
	switch {
	case err == nil:
		if !initialized {
			if err := marker.MarkAdminInitialized(ctx); err != nil {
				return nil, err
			}
		}
		return &Credential{digest: digest}, nil
	case errors.Is(err, fs.ErrNotExist) && initialized:
		return nil, ErrTokenMissing
	case errors.Is(err, fs.ErrNotExist):
		token, digest, err := createToken(ctx, dir, marker)
		if err != nil {
			return nil, err
		}
		show(token)
		return &Credential{digest: digest}, nil
	case errors.Is(err, errInsecure), errors.Is(err, ErrTokenInvalid):
		return nil, ErrTokenInvalid
	}
	return nil, err
}

// ResetToken replaces the admin token of a stopped gateway and returns the new
// value, which the caller shows once. The caller holds the instance lock; the
// token lock is taken without waiting, so a running gateway makes it fail
// before anything changes.
func ResetToken(ctx context.Context, dir string, marker storage.AdminMarker) (string, error) {
	lock, err := acquireTokenLock(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Close() }()
	token, _, err := createToken(ctx, dir, marker)
	return token, err
}

func acquireTokenLock(dir string) (*os.File, error) {
	file, err := openLockFile(filepath.Join(dir, TokenLockFile))
	if err != nil {
		if errors.Is(err, errInsecure) {
			return nil, ErrTokenInvalid
		}
		return nil, errTokenIO
	}
	locked, err := tryLock(file)
	if err != nil || !locked {
		_ = file.Close()
		if err != nil {
			return nil, errTokenIO
		}
		return nil, ErrTokenInUse
	}
	return file, nil
}

// createToken writes the file first and the marker after it. A crash between the
// two leaves a valid file that the next start adopts.
func createToken(ctx context.Context, dir string, marker storage.AdminMarker) (string, [sha256.Size]byte, error) {
	var raw [tokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", [sha256.Size]byte{}, errTokenIO
	}
	token := hex.EncodeToString(raw[:])
	digest := sha256.Sum256([]byte(token))
	if err := writeDigest(dir, digest); err != nil {
		return "", [sha256.Size]byte{}, err
	}
	if err := marker.MarkAdminInitialized(ctx); err != nil {
		return "", [sha256.Size]byte{}, err
	}
	return token, digest, nil
}

func readDigest(dir string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	var data []byte
	var err error
	for attempt := range readAttempts {
		if attempt > 0 {
			time.Sleep(readRetryWait)
		}
		data, err = readTokenFile(filepath.Join(dir, TokenFile))
		if err == nil || !isTransientOpenError(errors.Unwrap(err)) {
			break
		}
	}
	if err != nil {
		return digest, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document tokenDocument
	if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF || document.Version != 1 || !tokenPattern.MatchString(document.SHA256) {
		return digest, ErrTokenInvalid
	}
	decoded, _ := hex.DecodeString(document.SHA256)
	copy(digest[:], decoded)
	return digest, nil
}

func readTokenFile(path string) ([]byte, error) {
	file, err := openPrivate(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errInsecure) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", errTokenIO, err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxTokenFile+1))
	if err != nil || len(data) > maxTokenFile {
		return nil, ErrTokenInvalid
	}
	return data, nil
}

func writeDigest(dir string, digest [sha256.Size]byte) error {
	data, _ := json.Marshal(tokenDocument{Version: 1, SHA256: hex.EncodeToString(digest[:])})
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return errTokenIO
	}
	temporary := filepath.Join(dir, ".admin-token-"+hex.EncodeToString(suffix[:])+".tmp")
	file, err := createPrivateFile(temporary)
	if err != nil {
		return errTokenIO
	}
	abandon := func() error {
		_ = file.Close()
		_ = os.Remove(temporary)
		return errTokenIO
	}
	if _, err := file.Write(data); err != nil {
		return abandon()
	}
	if err := file.Sync(); err != nil {
		return abandon()
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return errTokenIO
	}
	if err := replaceFile(temporary, filepath.Join(dir, TokenFile)); err != nil {
		_ = os.Remove(temporary)
		return errTokenIO
	}
	if err := syncDir(dir); err != nil {
		return errTokenIO
	}
	return nil
}
