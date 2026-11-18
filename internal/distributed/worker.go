package distributed

import (
	"context"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
)

// ProjectOutbox applies undelivered transitions of every app. Claims expire
// after 30 seconds, so a replica that died mid-batch is retried by another;
// every transition is idempotent, so a retry never applies it twice.
func (c *Coordinator) ProjectOutbox(ctx context.Context) error {
	if c.Mode() != postgres.ModeNormal {
		return nil
	}
	entries, err := c.o.SQL.ClaimOutbox(ctx, "", 1000)
	if err != nil {
		return err
	}
	var delivered []int64
	for _, entry := range entries {
		if err := c.project(ctx, entry); err != nil {
			break
		}
		delivered = append(delivered, entry.ID)
	}
	if len(delivered) > 0 {
		return c.o.SQL.MarkDelivered(ctx, delivered...)
	}
	return nil
}

// RunProjection retries failed settlements and slot releases and applies the
// outbox, every second, until ctx ends.
func (c *Coordinator) RunProjection(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = c.RetryPending(final)
			_ = c.ProjectOutbox(final)
			cancel()
			return nil
		case <-ticker.C:
			_ = c.RetryPending(ctx)
			_ = c.ProjectOutbox(ctx)
		}
	}
}
