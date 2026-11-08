package storage

import (
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/config"
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
