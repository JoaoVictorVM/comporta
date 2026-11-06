package config

import (
	"maps"
	"slices"
	"time"
)

// Snapshot is one immutable generation of the configuration. A consumer captures
// it when an operation starts and keeps it until the operation ends, so a reload
// can never change the rules of a request already in flight. Accessors return
// defensive copies, which keeps the shared state unreachable from callers.
type Snapshot struct {
	generation int64
	loadedAt   time.Time
	cfg        Config
	secrets    map[string]Secret
	leaves     map[string]string
	publicYAML string
}

func newSnapshot(generation int64, at time.Time, candidate *Candidate) *Snapshot {
	tree := publicTree(&candidate.cfg)
	return &Snapshot{
		generation: generation,
		loadedAt:   at,
		cfg:        candidate.cfg,
		secrets:    candidate.secrets,
		leaves:     flattenTree(tree),
		publicYAML: renderTree(tree),
	}
}

// Generation is an increasing number local to this process. It must not be used
// as an identity that survives a restart.
func (s *Snapshot) Generation() int64 { return s.generation }

// LoadedAt is the UTC time this generation was published.
func (s *Snapshot) LoadedAt() time.Time { return s.loadedAt }

// DataDir is the absolute data directory.
func (s *Snapshot) DataDir() string { return s.cfg.DataDir }

// Timezone is the effective root timezone.
func (s *Snapshot) Timezone() string { return s.cfg.Timezone }

// FailMode is the effective root fail mode.
func (s *Snapshot) FailMode() FailMode { return s.cfg.FailMode }

// ProviderNames lists providers in sorted order.
func (s *Snapshot) ProviderNames() []string { return slices.Sorted(maps.Keys(s.cfg.Providers)) }

// Provider returns a copy of a provider definition.
func (s *Snapshot) Provider(name string) (Provider, bool) {
	provider, ok := s.cfg.Providers[name]
	return provider.clone(), ok
}

// ProviderForModel returns the name of the provider that declares a model.
func (s *Snapshot) ProviderForModel(model string) (string, bool) {
	for name, provider := range s.cfg.Providers {
		if slices.Contains(provider.Models, model) {
			return name, true
		}
	}
	return "", false
}

// ProviderAPIKey returns the resolved key. Local providers may have none.
func (s *Snapshot) ProviderAPIKey(name string) (Secret, bool) {
	secret, ok := s.secrets[joinPath(joinPath("providers", name), "api_key")]
	return secret, ok
}

// AppNames lists apps in sorted order.
func (s *Snapshot) AppNames() []string { return slices.Sorted(maps.Keys(s.cfg.Apps)) }

// App returns a copy of an app definition with defaults and derived values.
func (s *Snapshot) App(name string) (App, bool) {
	app, ok := s.cfg.Apps[name]
	return app.clone(), ok
}

// Price returns the configured override for a model.
func (s *Snapshot) Price(model string) (Price, bool) {
	price, ok := s.cfg.Pricing[model]
	return price, ok
}

// Listeners returns a copy of the listener settings.
func (s *Snapshot) Listeners() Listeners { return s.cfg.Listeners.clone() }

// Cache returns the global cache settings.
func (s *Snapshot) Cache() Cache { return s.cfg.Cache }

// Alerts returns a copy of the alert settings.
func (s *Snapshot) Alerts() Alerts { return s.cfg.Alerts.clone() }

// AlertSecret returns a resolved channel secret. The field is url,
// signing_secret or token.
func (s *Snapshot) AlertSecret(channel, field string) (Secret, bool) {
	secret, ok := s.secrets[joinPath(joinPath("alerts.channels", channel), field)]
	return secret, ok
}

// Telemetry returns the telemetry settings.
func (s *Snapshot) Telemetry() Telemetry { return s.cfg.Telemetry }

// EffectiveYAML renders the configuration with defaults and masked secrets. It is
// generated from typed values, never from the source file, and cannot be loaded
// back as input.
func (s *Snapshot) EffectiveYAML() string { return s.publicYAML }
