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
	Budget   *BudgetHeaders
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
