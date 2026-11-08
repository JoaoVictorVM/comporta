package proxy

import (
	"context"
	"github.com/JoaoVictorVM/comporta/internal/config"
)

type Accounting interface {
	Admit(context.Context, *config.Snapshot, *Envelope, string, config.Provider) (func(AttemptResult), *Rejection)
}
