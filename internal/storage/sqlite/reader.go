package sqlite

import (
	"context"
	"database/sql"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"net/url"
	"path/filepath"
)

func OpenReader(ctx context.Context, dir string) (*Store, error) {
	path := filepath.Join(dir, "usage.sqlite")
	f, err := openPrivate(path)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	f.Close()
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro&_pragma=query_only(ON)&_pragma=busy_timeout(1000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	db.SetMaxOpenConns(4)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, storage.ErrUnavailable
	}
	return &Store{db: db, dir: dir}, nil
}
