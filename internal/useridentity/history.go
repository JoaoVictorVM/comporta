package useridentity

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"regexp"
)

// HistoryState is what a guard could prove about the data directory.
type HistoryState int

const (
	// HistoryEmpty means no state depends on a previous key.
	HistoryEmpty HistoryState = iota
	// HistoryPresent means state exists, or that its absence could not be proved.
	HistoryPresent
)

// HistoryGuard decides whether a missing key may be generated, because a new key
// silently orphans every pseudonym already stored. An error never means empty.
//
// A guard backed by real usage storage must inspect the logical state of the database
// and its WAL before the database is created, and Reset must run offline, unlink
// per-user data and limit state, and leave the app's spending totals intact: resetting
// identities cannot reopen an app's budget. A failed Reset keeps the service down and
// no new key is generated.
type HistoryGuard interface {
	Inspect(ctx context.Context) (HistoryState, error)
	Reset(ctx context.Context) error
}

// These cannot hold usage history. Everything else in the directory counts as history,
// including empty databases, because emptiness cannot be judged by size when a WAL may
// hold the data. The admin token file stores only a digest of the operator token.
var (
	keyStoreArtifacts = map[string]bool{"keys.json": true, "keys.lock": true, lockFile: true, "admin-token.json": true, "admin-token.lock": true}
	ownTemporary      = regexp.MustCompile(`^\.(hmac|keys)-[0-9A-Za-z_-]+\.tmp$|^\.admin-token-[0-9a-f]{32}\.tmp$`)
)

type filesystemGuard struct {
	dir string
}

// NewFilesystemGuard returns the guard used while no usage storage exists. It can
// prove emptiness and nothing more, so its Reset always refuses.
func NewFilesystemGuard(dir string) HistoryGuard { return filesystemGuard{dir: dir} }

func (g filesystemGuard) Inspect(ctx context.Context) (HistoryState, error) {
	if err := ctx.Err(); err != nil {
		return HistoryPresent, err
	}
	entries, err := os.ReadDir(g.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return HistoryEmpty, nil
	}
	if err != nil {
		return HistoryPresent, err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return HistoryPresent, nil
		}
		if !keyStoreArtifacts[entry.Name()] && !ownTemporary.MatchString(entry.Name()) {
			return HistoryPresent, nil
		}
	}
	return HistoryEmpty, nil
}

func (g filesystemGuard) Reset(context.Context) error { return ErrResetUnsupported }
