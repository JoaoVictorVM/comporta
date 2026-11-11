package admin

import (
	"log/slog"
	"time"
)

// ServiceOptions configures the operational services.
type ServiceOptions struct {
	Logger *slog.Logger
	Now    func() time.Time
}

// Service holds the operations of the admin API, independent of HTTP.
type Service struct {
	logger *slog.Logger
	now    func() time.Time
}

// NewService returns the operational services.
func NewService(options ServiceOptions) *Service {
	s := &Service{logger: options.Logger, now: options.Now}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}
