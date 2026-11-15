package storage

import (
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"regexp"
	"time"
)

var ErrUnavailable = errors.New("usage storage unavailable")
var ErrFull = errors.New("data directory is full")
var ErrConflict = errors.New("incompatible usage record")
var ErrInvalid = errors.New("invalid usage record")

type Record struct {
	CacheWriteEstimated                                                                           bool
	RequestID                                                                                     string
	AttemptIndex                                                                                  int
	StartedAt, FinishedAt                                                                         time.Time
	AppName, UserHash                                                                             string
	Anonymous                                                                                     bool
	Operation, Provider, ModelRequested, ModelUsed                                                string
	UpstreamStatus, DownstreamStatus                                                              *int
	Outcome, ErrorClass, ErrorCode                                                                string
	Input, Output, CacheWrite, CacheRead, RawInputTotal, CountedInput, CacheWrite5m, CacheWrite1h *int64
	Cost, ReservedCost                                                                            config.Microusd
	UsageMissing, Recovered, EstimateExceeded                                                     bool
	RequestCount                                                                                  int
	LatencyUS                                                                                     int64
	UpstreamTTFB, ClientTTFB                                                                      *int64
	Day, Timezone                                                                                 string
	Quote                                                                                         Quote
	// LimitsDegraded marks an attempt admitted under fail_open while limit state
	// was unverifiable. BlockReason tells a block by spent budget from a block by
	// pending reservations. Absent in older records, they read as false and empty.
	LimitsDegraded bool   `json:",omitempty"`
	BlockReason    string `json:",omitempty"`
	// Detections is the safe summary of the request's sensitive data inspection.
	// Absent when nothing matched and in records written before inspection existed.
	Detections sensitivedata.Summary `json:",omitempty"`
	// Policy fields record what the on_limit policy did, apart from the transport
	// outcome. Absent in older records, which never means warned.
	PolicyOutcome   string     `json:",omitempty"`
	PolicyLimitName string     `json:",omitempty"`
	PolicyScope     string     `json:",omitempty"`
	PolicyReason    string     `json:",omitempty"`
	PolicyRenewsAt  *time.Time `json:",omitempty"`
}

var policyLimitName = regexp.MustCompile(`^apps\.[a-z0-9-]{1,40}\.(per_user\.)?(limits|budget|hard_cap)\.[a-z_]{1,32}$`)

// ValidatePolicy holds the policy fields to their domain: a triggered policy
// names its limit, scope and reason; no policy leaves them all empty.
func (r Record) ValidatePolicy() error {
	switch r.PolicyOutcome {
	case "":
		if r.PolicyLimitName != "" || r.PolicyScope != "" || r.PolicyReason != "" || r.PolicyRenewsAt != nil {
			return ErrInvalid
		}
		return nil
	case "blocked", "warned", "downgraded":
	default:
		return ErrInvalid
	}
	if !policyLimitName.MatchString(r.PolicyLimitName) || (r.PolicyScope != "app" && r.PolicyScope != "user") ||
		(r.PolicyReason != "spent" && r.PolicyReason != "reservation") {
		return ErrInvalid
	}
	return nil
}

type Quote struct {
	EstimateMethod                  string
	Price, HighPrice                config.Price
	Write1h, HighWrite1h            config.Microusd
	Threshold                       int64
	Source                          string
	Version                         time.Time
	EstimatedInput, EstimatedOutput int64
	InputFormulaCost, ReservedCost  config.Microusd
	Generation                      int64
}

type Daily struct {
	Day, Timezone, AppName, Scope, Subject                                            string
	Anonymous                                                                         bool
	Requests, Attempts, Input, Output, CacheWrite, CacheRead, UsageMissing, Recovered int64
	Cost                                                                              config.Microusd
}

type Filter struct {
	AppName, UserHash string
	Anonymous         bool
	Since, Until      string
}

type Pending struct {
	Admission Record
	Result    *Record
}

type RecordCursor struct {
	StartedAt    time.Time
	RequestID    string
	AttemptIndex int
}
type RecordFilter struct {
	AppName, UserHash string
	Anonymous         bool
	From, Until       time.Time
	After             *RecordCursor
	Limit             int
}
type RecordPage struct {
	Records []Record
	Next    *RecordCursor
}
