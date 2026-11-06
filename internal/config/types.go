// Package config loads, validates and publishes the gateway configuration.
//
// Optional numeric limits use the zero value to mean "not configured": every
// configured limit must be strictly positive, so zero never collides with a
// real value and implicit unlimited budgets stay impossible to express.
package config

import (
	"regexp"
	"slices"
	"time"

	// Budgets depend on calendar rules, and minimal hosts and Windows do not
	// always ship an IANA database.
	_ "time/tzdata"
)

// MaxConfigBytes is the largest accepted configuration file.
const MaxConfigBytes = 1_000_000

// Redacted replaces every secret in public representations. Prefixes and
// suffixes are not preserved because short secrets would be fully exposed.
const Redacted = "[REDACTED]"

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	envRefPattern = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
)

type FailMode string

const (
	FailClosed FailMode = "fail_closed"
	FailOpen   FailMode = "fail_open"
)

type APIFormat string

const (
	FormatAnthropic APIFormat = "anthropic"
	FormatOpenAI    APIFormat = "openai"
)

type OnLimit string

const (
	OnLimitBlock     OnLimit = "block"
	OnLimitDowngrade OnLimit = "downgrade"
	OnLimitWarn      OnLimit = "warn"
)

type DetectionMode string

const (
	DetectionOff    DetectionMode = "off"
	DetectionDetect DetectionMode = "detect"
	DetectionRedact DetectionMode = "redact"
	DetectionBlock  DetectionMode = "block"
)

type AlertChannelType string

const (
	AlertWebhook AlertChannelType = "webhook"
	AlertDiscord AlertChannelType = "discord"
	AlertSlack   AlertChannelType = "slack"
	AlertNtfy    AlertChannelType = "ntfy"
)

// SecretRef points to a secret without holding its value. At most one source
// is set.
type SecretRef struct {
	Env  string
	File string
}

// IsSet reports whether the reference names any source.
func (r SecretRef) IsSet() bool { return r.Env != "" || r.File != "" }

// Config is the typed, fully defaulted configuration contract. Secret values
// never live here: see Snapshot for the accessors that expose them.
type Config struct {
	DataDir   string
	Timezone  string
	FailMode  FailMode
	Providers map[string]Provider
	Apps      map[string]App
	Listeners Listeners
	Pricing   map[string]Price
	Cache     Cache
	Alerts    Alerts
	Telemetry Telemetry
}

type Provider struct {
	APIFormat     APIFormat
	BaseURL       string
	APIKey        SecretRef
	Models        []string
	TLSMinVersion string
	// Local marks loopback or private destinations, which may omit a key and
	// use plain HTTP.
	Local bool
}

func (p Provider) clone() Provider {
	p.Models = slices.Clone(p.Models)
	return p
}

type Listener struct {
	Bind string
	Port int64
}

type Listeners struct {
	Proxy                 Listener
	Admin                 Listener
	AllowedHosts          []string
	MaxBodyBytes          int64
	ReadHeaderTimeout     time.Duration
	NonStreamingTimeout   time.Duration
	StreamingIdleTimeout  time.Duration
	UpstreamHeaderTimeout time.Duration
}

func (l Listeners) clone() Listeners {
	l.AllowedHosts = slices.Clone(l.AllowedHosts)
	return l
}

type Limits struct {
	RequestsPerMinute int64
	TokensPerMinute   int64
	MaxConcurrent     int64
}

// Budget is also the shape of a hard cap, so each budget dimension can have an
// independent ceiling.
type Budget struct {
	USDPerDay      Microusd
	USDPerMonth    Microusd
	TokensPerDay   int64
	TokensPerMonth int64
}

func (b Budget) isZero() bool { return b == Budget{} }

type PerUser struct {
	Limits  Limits
	Budget  Budget
	HardCap Budget
}

type LoopGuard struct {
	Enabled           bool
	SimilarRequests   int64
	SimilarWindow     time.Duration
	RequestsPerMinute int64
	Cooldown          time.Duration
}

type AppCache struct {
	Enabled           bool
	TTLSeconds        int64
	SharedAcrossUsers bool
}

type Detection struct {
	CPF       DetectionMode
	CNPJ      DetectionMode
	Card      DetectionMode
	Email     DetectionMode
	Phone     DetectionMode
	Pix       DetectionMode
	Secret    DetectionMode
	Rehydrate bool
}

type App struct {
	RequireUser   bool
	AllowedModels []string
	MaxTokens     int64
	OnLimit       OnLimit
	DowngradeTo   string
	Timezone      string
	FailMode      FailMode
	Limits        Limits
	Budget        Budget
	HardCap       Budget
	PerUser       PerUser
	LoopGuard     LoopGuard
	Fallback      []string
	Cache         AppCache
	Detection     Detection
}

func (a App) clone() App {
	a.AllowedModels = slices.Clone(a.AllowedModels)
	a.Fallback = slices.Clone(a.Fallback)
	return a
}

// Price holds microdollars per one million tokens.
type Price struct {
	Input      Microusd
	Output     Microusd
	CacheWrite Microusd
	CacheRead  Microusd
}

type Cache struct {
	MaxBytes int64
}

type AlertChannel struct {
	Type          AlertChannelType
	URL           SecretRef
	SigningSecret SecretRef
	ServerURL     string
	Topic         string
	Token         SecretRef
}

type Alerts struct {
	Channels          map[string]AlertChannel
	Thresholds        []int64
	AnomalyMultiplier int64
	AnomalyMinUSD     Microusd
}

func (a Alerts) clone() Alerts {
	a.Thresholds = slices.Clone(a.Thresholds)
	channels := make(map[string]AlertChannel, len(a.Channels))
	for name, channel := range a.Channels {
		channels[name] = channel
	}
	a.Channels = channels
	return a
}

type Telemetry struct {
	MetricsEnabled bool
	OTelEnabled    bool
	OTelEndpoint   string
}

func defaultConfig() Config {
	return Config{
		DataDir:   "./data",
		FailMode:  FailClosed,
		Providers: map[string]Provider{},
		Apps:      map[string]App{},
		Pricing:   map[string]Price{},
		Listeners: Listeners{
			Proxy:                 Listener{Bind: "127.0.0.1", Port: 8080},
			Admin:                 Listener{Bind: "127.0.0.1", Port: 8081},
			MaxBodyBytes:          10_000_000,
			ReadHeaderTimeout:     5 * time.Second,
			NonStreamingTimeout:   300 * time.Second,
			StreamingIdleTimeout:  60 * time.Second,
			UpstreamHeaderTimeout: 60 * time.Second,
		},
		Cache: Cache{MaxBytes: 256_000_000},
		Alerts: Alerts{
			Channels:          map[string]AlertChannel{},
			Thresholds:        []int64{50, 80, 100},
			AnomalyMultiplier: 5,
			AnomalyMinUSD:     500_000,
		},
		Telemetry: Telemetry{MetricsEnabled: true},
	}
}

func defaultApp() App {
	return App{
		MaxTokens: 4096,
		OnLimit:   OnLimitBlock,
		Limits:    Limits{MaxConcurrent: 20},
		PerUser:   PerUser{Limits: Limits{MaxConcurrent: 4}},
		LoopGuard: LoopGuard{
			Enabled:           true,
			SimilarRequests:   10,
			SimilarWindow:     30 * time.Second,
			RequestsPerMinute: 60,
			Cooldown:          120 * time.Second,
		},
		Cache: AppCache{TTLSeconds: 600},
		Detection: Detection{
			CPF:    DetectionOff,
			CNPJ:   DetectionOff,
			Card:   DetectionOff,
			Email:  DetectionOff,
			Phone:  DetectionOff,
			Pix:    DetectionOff,
			Secret: DetectionOff,
		},
	}
}
