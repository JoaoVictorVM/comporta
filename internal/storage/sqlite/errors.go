package sqlite

import (
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	sqlitedriver "modernc.org/sqlite"
)

func databaseError(err error) error {
	var failure *sqlitedriver.Error
	if errors.As(err, &failure) && failure.Code()&255 == 13 {
		return storage.ErrFull
	}
	if failure != nil && failure.Code()&255 == 19 {
		return storage.ErrInvalid
	}
	return storage.ErrUnavailable
}

// sqliteCode returns the primary result code of a driver error.
func sqliteCode(err error) (int, bool) {
	var failure *sqlitedriver.Error
	if !errors.As(err, &failure) {
		return 0, false
	}
	return failure.Code() & 255, true
}
