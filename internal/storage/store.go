package storage

import (
	"context"
	"time"
)

type AdmissionStore interface {
	Admit(context.Context, Record) error
}
type CompletionStore interface {
	Complete(context.Context, Record) error
	Commit(context.Context, []Record) error
	Pending(context.Context) ([]Pending, error)
}
type UsageReader interface {
	Daily(context.Context, Filter) ([]Daily, error)
	Apps(context.Context) ([]string, error)
	Records(context.Context, RecordFilter) (RecordPage, error)
}
type MaintenanceStore interface {
	Retain(context.Context, time.Time) error
}
