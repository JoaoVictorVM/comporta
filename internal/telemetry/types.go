// Package telemetry observes the gateway for operators: Prometheus metrics,
// OpenTelemetry traces and their own diagnostics. It never decides admission,
// pricing or delivery, and nothing it receives carries content, a raw user
// identifier or a user hash.
package telemetry

import (
	"regexp"
	"time"
)

// Operation is the kind of proxy route a request entered.
type Operation string

const (
	OperationGeneration  Operation = "generation"
	OperationCountTokens Operation = "count_tokens"
	OperationModels      Operation = "models"
)

// Outcome is the terminal result of one logical request. When several apply,
// the first in this order wins: canceled, error, loop_cut, blocked, cached,
// fallback, downgraded, ok.
type Outcome string

const (
	OutcomeCanceled   Outcome = "canceled"
	OutcomeError      Outcome = "error"
	OutcomeLoopCut    Outcome = "loop_cut"
	OutcomeBlocked    Outcome = "blocked"
	OutcomeCached     Outcome = "cached"
	OutcomeFallback   Outcome = "fallback"
	OutcomeDowngraded Outcome = "downgraded"
	OutcomeOK         Outcome = "ok"
)

// Stage is a fixed pipeline step that becomes an INTERNAL span when it runs.
type Stage string

const (
	StageAuthenticate Stage = "authenticate"
	StageIdentity     Stage = "identity"
	StageValidate     Stage = "validate"
	StagePause        Stage = "pause"
	StageSensitive    Stage = "sensitive"
	StageLoopGuard    Stage = "loop_guard"
	StageRoute        Stage = "route"
	StageAdmit        Stage = "admit"
	StageCacheLookup  Stage = "cache_lookup"
	StageFallback     Stage = "fallback"
	StageDeliver      Stage = "deliver"
	StageSettle       Stage = "settle"
)

// BlockType is the primary reason of a terminal refusal.
type BlockType string

const (
	BlockRate           BlockType = "rate"
	BlockConcurrency    BlockType = "concurrency"
	BlockBudget         BlockType = "budget"
	BlockHardCap        BlockType = "hard_cap"
	BlockReservation    BlockType = "reservation"
	BlockLoop           BlockType = "loop"
	BlockSensitive      BlockType = "sensitive"
	BlockPaused         BlockType = "paused"
	BlockAuthentication BlockType = "authentication"
	BlockValidation     BlockType = "validation"
)

// BlockScope is who the refusal applies to.
type BlockScope string

const (
	ScopeApp     BlockScope = "app"
	ScopeUser    BlockScope = "user"
	ScopeRequest BlockScope = "request"
)

// Block describes a terminal refusal. A zero Block means the request was not
// refused.
type Block struct {
	Type  BlockType
	Scope BlockScope
}

// AttemptResult classifies one upstream attempt that was actually sent.
type AttemptResult string

const (
	AttemptOK             AttemptResult = "ok"
	AttemptClientError    AttemptResult = "client_error"
	AttemptServerError    AttemptResult = "server_error"
	AttemptTransportError AttemptResult = "transport_error"
	AttemptCanceled       AttemptResult = "canceled"
)

// Optional holds a value that may not have been observed. An unobserved value
// is never reported as zero.
type Optional[T any] struct {
	Value T
	Known bool
}

// Some returns a known value.
func Some[T any](value T) Optional[T] { return Optional[T]{Value: value, Known: true} }

// RequestEnd is the projection of a finished logical request. It holds only
// configured names, closed categories and durations.
type RequestEnd struct {
	Outcome Outcome
	Status  Optional[int]
	Block   Block
	// ErrorType is a closed category of the gateway's own error vocabulary.
	ErrorType string
}

// AttemptEnd is the projection of one upstream attempt as the proxy saw it.
type AttemptEnd struct {
	// Sent is false when nothing reached a provider, such as a cache replay.
	Sent   bool
	Result AttemptResult
	Status Optional[int]
	// ErrorType is a closed category of the gateway's own error vocabulary.
	ErrorType string
	// Ended is when the upstream exchange ended, before its settlement.
	Ended time.Time
	// InputTokens and OutputTokens are the disjoint counts reported by the
	// provider; unknown values stay unknown.
	InputTokens      Optional[int64]
	OutputTokens     Optional[int64]
	CacheReadTokens  Optional[int64]
	CacheWriteTokens Optional[int64]
}

// Consumption is what the metering confirmed for one concluded attempt. Cost
// and tokens are the integers the metering settled; they are never priced
// again here.
type Consumption struct {
	App       string
	Provider  string
	Model     string
	Operation Operation
	// CostMicrousd is the settled cost, including the conservative floor of a
	// response whose usage was incomplete.
	CostMicrousd     int64
	InputTokens      Optional[int64]
	OutputTokens     Optional[int64]
	CacheReadTokens  Optional[int64]
	CacheWriteTokens Optional[int64]
	UsageComplete    bool
	// SettlementConfirmed is false when the conclusion could not be made
	// durable; no confirmed cost is inferred from it.
	SettlementConfirmed bool
	// RequestID and AttemptIndex correlate the conclusion with the attempt
	// span that is still open. They never become a label or an attribute.
	RequestID    string
	AttemptIndex int
}

// AccountingObserver receives every concluded attempt from the metering, once,
// after the financial decision. Implementations must not block.
type AccountingObserver interface {
	ObserveConsumption(Consumption)
	// AccountingFailure reports a conclusion that the metering could not
	// confirm, with a closed reason.
	AccountingFailure(reason string)
}

// Accounting failure reasons.
const (
	ReasonSettlementUnconfirmed = "settlement_unconfirmed"
	ReasonPricingFailed         = "pricing_failed"
)

// Telemetry components and failure reasons, closed sets for
// telemetry_failures_total.
const (
	ComponentTracing    = "tracing"
	ComponentMetrics    = "metrics"
	ComponentAccounting = "accounting_observer"
	ComponentReload     = "reload"

	ReasonExportFailed    = "export_failed"
	ReasonScrapeFailed    = "scrape_failed"
	ReasonProviderInit    = "provider_init"
	ReasonShutdownTimeout = "shutdown_timeout"
	ReasonStaleSnapshot   = "stale_snapshot"
	ReasonRetired         = "provider_retired"
)

// Dropped span reasons, closed set for telemetry_dropped_spans_total.
const (
	DropQueueFull       = "queue_full"
	DropExportFailed    = "export_failed"
	DropReloadRetired   = "reload_retired"
	DropShutdownTimeout = "shutdown_timeout"
)

var (
	operations   = set(OperationGeneration, OperationCountTokens, OperationModels)
	outcomes     = set(OutcomeCanceled, OutcomeError, OutcomeLoopCut, OutcomeBlocked, OutcomeCached, OutcomeFallback, OutcomeDowngraded, OutcomeOK)
	blockTypes   = set(BlockRate, BlockConcurrency, BlockBudget, BlockHardCap, BlockReservation, BlockLoop, BlockSensitive, BlockPaused, BlockAuthentication, BlockValidation)
	blockScopes  = set(ScopeApp, ScopeUser, ScopeRequest)
	attempts     = set(AttemptOK, AttemptClientError, AttemptServerError, AttemptTransportError, AttemptCanceled)
	stages       = set(StageAuthenticate, StageIdentity, StageValidate, StagePause, StageSensitive, StageLoopGuard, StageRoute, StageAdmit, StageCacheLookup, StageFallback, StageDeliver, StageSettle)
	components   = set(ComponentTracing, ComponentMetrics, ComponentAccounting, ComponentReload)
	failures     = set(ReasonExportFailed, ReasonScrapeFailed, ReasonProviderInit, ReasonShutdownTimeout, ReasonStaleSnapshot, ReasonRetired, ReasonSettlementUnconfirmed, ReasonPricingFailed)
	dropReasons  = set(DropQueueFull, DropExportFailed, DropReloadRetired, DropShutdownTimeout)
	causePattern = regexp.MustCompile(`^(429|5[0-9][0-9]|connection|timeout)$`)
	errorPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

func set[T comparable](values ...T) map[T]bool {
	out := make(map[T]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

// errorType keeps a gateway error code only when it has the shape of the
// fixed vocabulary; anything else becomes a generic category.
func errorType(value string) string {
	if value == "" || errorPattern.MatchString(value) {
		return value
	}
	return "error"
}
