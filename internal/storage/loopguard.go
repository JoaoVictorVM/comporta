package storage

import (
	"context"

	"github.com/JoaoVictorVM/comporta/internal/events"
)

// LocalCut is a request refused by the loop guard before any attempt: its
// terminal record and the event of its episode. Retries in the same episode
// carry the same event, so a commit that failed earlier is completed by the
// next one without duplicating it.
type LocalCut struct {
	Record Record
	Event  events.Event
}

// LocalCutStore commits a cut in one transaction and notifies subscribers only
// after a new event commits. Repeating an identical commit changes nothing.
type LocalCutStore interface {
	CommitLocalCut(context.Context, LocalCut) error
}
