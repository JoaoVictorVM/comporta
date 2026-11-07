package useridentity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const (
	keyFile  = "hmac.key"
	lockFile = "hmac.lock"

	defaultLockTimeout = 5 * time.Second
	lockPollInterval   = 20 * time.Millisecond

	readAttempts   = 5
	readRetryDelay = 50 * time.Millisecond

	temporarySuffixBytes = 16
)

// Startup failures. Their messages are fixed and name no path, cause or key material.
var (
	ErrKeySize          = errors.New("hmac.key must contain exactly 32 bytes")
	ErrKeyPermissions   = errors.New("hmac.key permissions are not private")
	ErrKeyUnavailable   = errors.New("user identity key unavailable")
	ErrHistoryExists    = errors.New("hmac.key not found but usage data exists; restore the key or start with --reset-user-history")
	ErrResetUnsupported = errors.New("user history reset requires usage storage support; restore hmac.key or use a new data directory")

	errInsecure      = errors.New("insecure file")
	errAlreadyExists = errors.New("destination exists")
)

// fileOps are the steps of a publication, replaceable so that tests can fail each one.
type fileOps struct {
	create  func(path string) (*os.File, error)
	write   func(file *os.File, data []byte) error
	sync    func(file *os.File) error
	publish func(from, to string) error
	syncDir func(dir string) error
}

func defaultFileOps() fileOps {
	return fileOps{
		create:  createPrivateFile,
		write:   func(file *os.File, data []byte) error { _, err := file.Write(data); return err },
		sync:    func(file *os.File) error { return file.Sync() },
		publish: publishNoReplace,
		syncDir: syncDir,
	}
}

// Options configures Open.
type Options struct {
	// Dir is the data directory.
	Dir string
	// History decides whether a missing key may be generated. Nil uses the
	// filesystem guard over Dir.
	History HistoryGuard
	// ResetHistory is the operator's explicit intent to recover from a lost key. It
	// never replaces a key that exists.
	ResetHistory bool
	// Entropy replaces the random source in tests. A failing source aborts startup:
	// it never falls back to something predictable.
	Entropy io.Reader
	// LockTimeout replaces the default wait for other starting processes.
	LockTimeout time.Duration
	Logger      *slog.Logger
}

type opener struct {
	Options
	ops fileOps
}

// Open loads the key of the data directory, generating it only when the guard proves
// that nothing depends on a previous one. The key is read once: replacing the file
// while the service runs has no effect.
func Open(ctx context.Context, options Options) (*Service, error) {
	return open(ctx, options, defaultFileOps())
}

func open(ctx context.Context, options Options, ops fileOps) (*Service, error) {
	o := &opener{Options: options, ops: ops}
	if o.History == nil {
		o.History = NewFilesystemGuard(o.Dir)
	}
	if o.Entropy == nil {
		o.Entropy = rand.Reader
	}
	if o.LockTimeout == 0 {
		o.LockTimeout = defaultLockTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}

	service, err := o.load()
	if !errors.Is(err, fs.ErrNotExist) {
		return o.loaded(service, err)
	}

	if err := o.prepareDir(); err != nil {
		return nil, err
	}
	unlock, err := o.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Another process may have published while this one waited for the lock.
	service, err = o.load()
	if !errors.Is(err, fs.ErrNotExist) {
		return o.loaded(service, err)
	}
	if err := o.authorizeGeneration(ctx); err != nil {
		return nil, err
	}
	return o.generate()
}

func (o *opener) loaded(service *Service, err error) (*Service, error) {
	if err != nil {
		return nil, err
	}
	if o.ResetHistory {
		o.Logger.Warn("--reset-user-history changed nothing: hmac.key is present and was kept")
	}
	return service, nil
}

func (o *opener) prepareDir() error {
	if err := ensurePrivateDir(o.Dir); err != nil {
		if errors.Is(err, errInsecure) {
			return ErrKeyPermissions
		}
		return ErrKeyUnavailable
	}
	return nil
}

func (o *opener) authorizeGeneration(ctx context.Context) error {
	state, err := o.History.Inspect(ctx)
	if err == nil && state == HistoryEmpty {
		return nil
	}
	if !o.ResetHistory {
		return ErrHistoryExists
	}
	// Only a guard that proved its state may reset it: an inspection that failed
	// says nothing about what the reset would destroy.
	if err != nil {
		return ErrResetUnsupported
	}
	if err := o.History.Reset(ctx); err != nil {
		return ErrResetUnsupported
	}
	return nil
}

func (o *opener) keyPath() string { return filepath.Join(o.Dir, keyFile) }

// load returns fs.ErrNotExist only when the key is proved missing. Every other failure
// keeps the service down: treating it as missing could replace a key still in use.
func (o *opener) load() (*Service, error) {
	var last error
	for attempt := range readAttempts {
		if attempt > 0 {
			time.Sleep(readRetryDelay)
		}
		service, transient, err := o.loadOnce()
		if !transient {
			return service, err
		}
		last = err
	}
	return nil, last
}

// loadOnce also reports whether the failure may clear on its own.
func (o *opener) loadOnce() (service *Service, transient bool, err error) {
	file, err := openPrivate(o.keyPath())
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, false, fs.ErrNotExist
		case errors.Is(err, errInsecure):
			return nil, false, ErrKeyPermissions
		}
		return nil, isTransientOpenError(err), ErrKeyUnavailable
	}
	defer func() { _ = file.Close() }()
	// One byte over the limit tells an oversized file apart without reading it whole.
	data, err := io.ReadAll(io.LimitReader(file, keyBytes+1))
	if err != nil {
		return nil, false, ErrKeyUnavailable
	}
	if len(data) != keyBytes {
		return nil, false, ErrKeySize
	}
	return newService(data), false, nil
}

func newService(key []byte) *Service {
	service := &Service{}
	copy(service.key[:], key)
	return service
}

// lock waits for the exclusive lock. The file is never removed: it must stay the same
// file for every process, and the system releases it when its holder dies.
func (o *opener) lock(ctx context.Context) (func(), error) {
	file, err := openLockFile(filepath.Join(o.Dir, lockFile))
	if err != nil {
		if errors.Is(err, errInsecure) {
			return nil, ErrKeyPermissions
		}
		return nil, ErrKeyUnavailable
	}
	timeout := time.NewTimer(o.LockTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(lockPollInterval)
	defer ticker.Stop()
	for {
		locked, err := tryLock(file)
		if err != nil {
			_ = file.Close()
			return nil, ErrKeyUnavailable
		}
		if locked {
			return func() { _ = file.Close() }, nil
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ErrKeyUnavailable
		case <-timeout.C:
			_ = file.Close()
			return nil, ErrKeyUnavailable
		case <-ticker.C:
		}
	}
}

// generate prepares the key in a private temporary file and publishes it without ever
// replacing a destination. After publication the key is in force: a failed durability
// step aborts startup, and the next one loads the same key rather than a new one.
func (o *opener) generate() (*Service, error) {
	key := make([]byte, keyBytes)
	if _, err := io.ReadFull(o.Entropy, key); err != nil {
		return nil, ErrKeyUnavailable
	}
	suffix := make([]byte, temporarySuffixBytes)
	if _, err := io.ReadFull(o.Entropy, suffix); err != nil {
		return nil, ErrKeyUnavailable
	}
	o.removeAbandoned()
	temporary := filepath.Join(o.Dir, ".hmac-"+hex.EncodeToString(suffix)+".tmp")
	file, err := o.ops.create(temporary)
	if err != nil {
		return nil, ErrKeyUnavailable
	}
	abandon := func() (*Service, error) {
		_ = file.Close()
		_ = os.Remove(temporary)
		return nil, ErrKeyUnavailable
	}
	if err := o.ops.write(file, key); err != nil {
		return abandon()
	}
	if err := o.ops.sync(file); err != nil {
		return abandon()
	}
	if err := file.Close(); err != nil {
		return abandon()
	}
	if err := o.ops.publish(temporary, o.keyPath()); err != nil {
		_ = os.Remove(temporary)
		if errors.Is(err, errAlreadyExists) {
			return o.load()
		}
		return nil, ErrKeyUnavailable
	}
	if err := o.ops.syncDir(o.Dir); err != nil {
		return nil, ErrKeyUnavailable
	}
	o.Logger.Info("user identity key created; back it up together with the data directory")
	return newService(key), nil
}

// removeAbandoned clears what a process that died while generating left behind. The
// lock is held, so no temporary is in use, and none was ever published. Removal acts
// on the entry itself and never follows a link.
func (o *opener) removeAbandoned() {
	matches, err := filepath.Glob(filepath.Join(o.Dir, ".hmac-*.tmp"))
	if err != nil {
		return
	}
	for _, path := range matches {
		_ = os.Remove(path)
	}
}
