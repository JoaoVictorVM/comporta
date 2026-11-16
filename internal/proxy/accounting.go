package proxy

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

// Admission is the answer of accounting to an admitted attempt: the callback
// that ends it, and the budget headers it may expose.
type Admission struct {
	Complete func(AttemptResult)
	// Conclude, when set, replaces Complete and reports whether the result was
	// made durable together with the end of its reservation. Without it the
	// proxy cannot know, and treats the conclusion as unconfirmed.
	Conclude func(AttemptResult) error
	Budget   *BudgetHeaders
	// Selected is the only candidate that may be sent; empty means the original.
	Selected CandidateID
	Policy   PolicyOutcome
}

// CandidateID correlates the admitted choice with a body the proxy prepared.
// It is a closed set: a selection never carries a URL or a header.
type CandidateID string

const (
	CandidateOriginal  CandidateID = "original"
	CandidateDowngrade CandidateID = "downgrade"
)

// PolicyOutcome is what the on_limit policy did to the admitted attempt.
type PolicyOutcome string

const (
	PolicyNone       PolicyOutcome = ""
	PolicyBlocked    PolicyOutcome = "blocked"
	PolicyWarned     PolicyOutcome = "warned"
	PolicyDowngraded PolicyOutcome = "downgraded"
)

// Candidate is one prepared way to send the request, described by what
// accounting needs to price it. The body itself stays with the proxy.
type Candidate struct {
	ID                CandidateID
	Model             string
	ProviderName      string
	Provider          config.Provider
	BodySizeEffective int64
	// Unavailable is the answer when the policy needs this candidate but it could
	// not be prepared, such as a target in another API format.
	Unavailable *Rejection
}

// PolicyAccounting admits one of several candidates. The original comes
// first. Accounting that only implements Accounting never applies a policy.
type PolicyAccounting interface {
	AdmitCandidates(context.Context, *config.Snapshot, *Envelope, []Candidate) (Admission, *Rejection)
}

// SessionAccounting admits a logical request whose attempts may continue on
// other models after an eligible failure. A nil session with no rejection
// means continuations are not available, and the admission is a plain one
// whose Complete ends it.
type SessionAccounting interface {
	AdmitSession(context.Context, *config.Snapshot, *Envelope, []Candidate) (AccountingSession, Admission, *Rejection)
}

// AccountingSession is the accounting side of one logical request. The proxy
// calls it from one goroutine, in order: Conclude for each attempt, Continue
// between attempts and Close once at the end.
type AccountingSession interface {
	// Conclude makes the attempt's result durable and settles its reservation.
	// An error means that result is not assured, so nothing else may be sent.
	Conclude(AttemptResult) error
	// Continue admits the next attempt after an eligible failure of the
	// previous model, with its own quote and reservation. A rejection is final.
	Continue(ctx context.Context, e *Envelope, candidates []Candidate, from, cause string) (Admission, *Rejection)
	// Close ends the logical request: the last concluded attempt becomes the
	// terminal one, and terminal is the answer when it differs from that
	// attempt's own. The concurrency slot is freed here, exactly once.
	Close(terminal *Failure)
}

// BudgetHeaders describe the most restrictive applicable budget right after
// admission. They are a photograph of that moment, not a promise about the
// balance when a stream ends.
type BudgetHeaders struct {
	Remaining string
	Reset     time.Time
	Unit      string
	Limit     string
}

// LimitDetail adds typed fields to a refusal caused by a limit.
type LimitDetail struct {
	LimitName string
	Scope     string
	Reason    string
	RenewsAt  *time.Time
}

type Accounting interface {
	Admit(context.Context, *config.Snapshot, *Envelope, string, config.Provider) (Admission, *Rejection)
}

const budgetHeaderPrefix = "x-comporta-budget-"

// applyBudgetHeaders sets the gateway's own budget headers. Upstream headers
// never carry this prefix through, so these are always authoritative.
func applyBudgetHeaders(header http.Header, budget *BudgetHeaders) {
	if budget == nil {
		return
	}
	header.Set(budgetHeaderPrefix+"remaining", budget.Remaining)
	header.Set(budgetHeaderPrefix+"reset", strconv.FormatInt(budget.Reset.Unix(), 10))
	header.Set(budgetHeaderPrefix+"unit", budget.Unit)
	header.Set(budgetHeaderPrefix+"limit", budget.Limit)
}
