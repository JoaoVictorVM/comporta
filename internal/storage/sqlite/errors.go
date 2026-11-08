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
	return storage.ErrUnavailable
}
