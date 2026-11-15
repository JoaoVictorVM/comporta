package storage

import (
	"context"

	"github.com/JoaoVictorVM/comporta/internal/events"
)

// SensitiveCut is a request refused by the sensitive data inspection before any
// attempt: its terminal record and, for a refusal by policy, the event of the
// inspection. A scan that failed has no event, because its counts are incomplete.
type SensitiveCut struct {
	Record Record
	Event  *events.Event
}

// SensitiveCutStore commits a cut in one transaction and notifies subscribers only
// after a new event commits. Repeating an identical commit changes nothing.
type SensitiveCutStore interface {
	CommitSensitiveCut(context.Context, SensitiveCut) error
}
