package storage

import (
	"context"
	"errors"
	"time"
)

// ErrBusy reports that a subject still has usage or limit state in transit, so
// a deletion would leave something behind. Nothing was changed.
var ErrBusy = errors.New("subject has pending usage")

// BusyError is ErrBusy with the kind of state still in transit. The reason is
// a fixed category, safe to log: it never names the subject.
type BusyError struct{ Reason string }

func (e BusyError) Error() string        { return ErrBusy.Error() + ": " + e.Reason }
func (e BusyError) Is(target error) bool { return target == ErrBusy }

// AppPause is the persisted override of one app.
type AppPause struct {
	App       string
	Paused    bool
	UpdatedAt time.Time
}

// PauseState is every persisted pause override. An app without an entry is
// not paused by an operator.
type PauseState struct {
	Global          bool
	GlobalUpdatedAt time.Time
	Apps            []AppPause
}

// PauseChange is one operator action. All selects the global override; resuming
// all also clears every individual override, including those of removed apps.
type PauseChange struct {
	All    bool
	App    string
	Paused bool
	At     time.Time
}

// PauseStore keeps pause overrides and writes the matching event in the same
// transaction. Changed is false when the state already matched.
type PauseStore interface {
	LoadPause(ctx context.Context) (PauseState, error)
	SavePause(ctx context.Context, change PauseChange) (state PauseState, changed bool, err error)
}

// AdminMarker records that the admin credential was created once, so that a
// missing credential file later is a failure and never a silent rotation.
type AdminMarker interface {
	AdminInitialized(ctx context.Context) (bool, error)
	MarkAdminInitialized(ctx context.Context) error
}

// SubjectRef is one identified end user of one app. Anonymous buckets are never
// selected by a deletion.
type SubjectRef struct {
	App  string
	Hash string
}

// ForgetCounts reports what a deletion changed. Deleted rows are physically
// removed; Unlinked counts rows kept for the app whose link to the user was
// cleared, once per row.
type ForgetCounts struct {
	Requests         int64
	UsageDaily       int64
	Events           int64
	Journal          int64
	BudgetState      int64
	ReservationItems int64
	Unlinked         int64
}

// DeletedRows is the sum of the physically removed rows.
func (c ForgetCounts) DeletedRows() int64 {
	return c.Requests + c.UsageDaily + c.Events + c.Journal + c.BudgetState + c.ReservationItems
}

// ForgetStore deletes the persisted history of end users in one transaction.
type ForgetStore interface {
	// HistoricalApps lists every app that has any persisted state, including
	// apps no longer configured.
	HistoricalApps(ctx context.Context) ([]string, error)
	ForgetSubjects(ctx context.Context, subjects []SubjectRef) (ForgetCounts, error)
}

// UsageSubject selects the daily rows of one app. In the app scope Subject is
// empty; in the user scope it is the pseudonym of the user or of the anonymous
// bucket.
type UsageSubject struct {
	App       string
	Subject   string
	Anonymous bool
}

// UsageSelector reads daily aggregates of one scope. Without subjects in the app
// scope it reads every app. From and Through bound the day labels.
type UsageSelector struct {
	Scope         string
	Subjects      []UsageSubject
	From, Through string
}

// UsageQueryStore reads aggregates in one consistent read, ordered by day,
// timezone and app.
type UsageQueryStore interface {
	QueryDaily(ctx context.Context, selector UsageSelector) ([]Daily, error)
}
