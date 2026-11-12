package proxy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/loopguard"
)

// LoopRecorder makes a loop cut durable before it is answered.
type LoopRecorder interface {
	RecordLoopCut(context.Context, *config.Snapshot, *Envelope, loopguard.Decision) error
}

// SnapshotSource provides the configuration in force.
type SnapshotSource interface {
	Capture() *config.Snapshot
}

// LoopStage connects the loop guard to the pipeline. Source supplies the
// current policy, so a reload disables the guard for the next evaluation even
// inside a request that captured an older snapshot.
type LoopStage struct {
	Guard    *loopguard.Manager
	Source   SnapshotSource
	Recorder LoopRecorder
}

// staleRetries bounds how many times a policy older than the applied one is
// recaptured before giving up.
const staleRetries = 3

const recordTimeout = time.Second

func loopUnavailable() *Rejection {
	return &Rejection{Status: http.StatusServiceUnavailable, Code: CodeLoopGuardUnavailable, Class: ClassInternal, Message: "loop guard unavailable"}
}

// evaluate counts the arrival once and answers a cut. Only generation is
// guarded: counting tokens or listing models never loops an agent's spend.
func (s *LoopStage) evaluate(ctx context.Context, snapshot *config.Snapshot, e *Envelope) *Rejection {
	if s == nil || s.Guard == nil || e.Operation != OperationGeneration {
		return nil
	}
	request := loopguard.Request{
		Subject:   loopguard.Subject{App: e.AppName, Hash: e.Identity.Hash().Hex(), Anonymous: e.Identity.Anonymous()},
		RequestID: e.RequestID,
	}
	if e.LastMessagesHash.Known {
		request.Signature, request.HasSignature = e.LastMessagesHash.Value, true
	}
	current := snapshot
	if s.Source != nil {
		current = s.Source.Capture()
	}
	var decision loopguard.Decision
	for attempt := 0; ; attempt++ {
		policy, ok := loopguard.PolicyFor(current, e.AppName)
		if !ok {
			// The app left the configuration; accounting refuses it on the
			// reload path, and there is nothing left to protect here.
			return nil
		}
		request.Policy = policy
		var err error
		decision, err = s.Guard.Check(request)
		if errors.Is(err, loopguard.ErrStale) && s.Source != nil && attempt < staleRetries {
			current = s.Source.Capture()
			continue
		}
		if err != nil {
			return loopUnavailable()
		}
		break
	}
	if decision.Allowed {
		return nil
	}
	if s.Recorder == nil {
		return loopUnavailable()
	}
	// The deadline bounds the write alone; the gate ticket stays open until it
	// returns, so a deletion can never overtake a late record.
	write, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	if err := s.Recorder.RecordLoopCut(write, current, e, decision); err != nil {
		return &Rejection{Status: http.StatusServiceUnavailable, Code: CodeUsageStorageUnavailable, Class: ClassInternal, Message: "usage storage unavailable"}
	}
	return loopRejection(decision)
}

func loopRejection(d loopguard.Decision) *Rejection {
	retry := int64(math.Ceil(d.Remaining.Seconds()))
	if retry < 1 {
		retry = 1
	}
	pause := formatDuration(d.Episode.Cooldown)
	if !d.First {
		pause = strconv.FormatInt(retry, 10) + "s"
	}
	var message string
	if d.Episode.Reason == loopguard.ReasonSimilar {
		message = fmt.Sprintf("loop detected: %d similar requests in %s; requests from this user are paused for %s", d.Episode.Limit, formatDuration(d.Episode.Window), pause)
	} else {
		message = fmt.Sprintf("loop detected: more than %d requests in %s; requests from this user are paused for %s", d.Episode.Limit, formatDuration(d.Episode.Window), pause)
	}
	return &Rejection{Status: http.StatusTooManyRequests, Code: CodeLoopDetected, Class: ClassRateLimit, Message: message, RetryAfter: retry}
}

// formatDuration writes seconds without losing a configured fraction: 30s,
// 1.5s, 0.25s. Go's own format would render 120s as 2m0s.
func formatDuration(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}
