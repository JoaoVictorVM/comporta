package metering

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

var scanAt = time.Date(2026, 11, 15, 12, 0, 0, 123456789, time.UTC)

func blockSummary() sensitivedata.Summary {
	return sensitivedata.Summary{{DataType: sensitivedata.Secret, Count: 1, Action: sensitivedata.ModeBlock, SecretKinds: []string{sensitivedata.KindAnthropicAPIKey}}}
}

func TestSensitiveBlockZeroCost(t *testing.T) {
	service, store, snapshot, envelope := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := store.Bus.Subscribe(ctx)
	cut := proxy.SensitiveCut{At: scanAt, Status: http.StatusBadRequest, Code: proxy.CodeSensitiveDataBlocked, Class: proxy.ClassInvalidRequest, Summary: blockSummary()}
	if err := service.RecordSensitiveCut(ctx, snapshot, envelope, cut); err != nil {
		t.Fatal(err)
	}
	page, err := store.Records(ctx, storage.RecordFilter{AppName: "bot", Limit: 10})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("records: %v", err)
	}
	r := page.Records[0]
	if r.Outcome != "blocked" || r.AttemptIndex != 0 || r.RequestCount != 0 || r.Cost != 0 || r.ReservedCost != 0 || r.Input != nil ||
		r.UpstreamStatus != nil || *r.DownstreamStatus != 400 || r.ErrorCode != "sensitive_data_blocked" || len(r.Detections) != 1 {
		t.Fatalf("cut record: %+v", r)
	}
	pending, _ := store.Pending(ctx)
	daily, _ := store.Daily(ctx, storage.Filter{AppName: "bot", Since: "2000-01-01", Until: "2100-01-01"})
	if len(pending) != 0 || len(daily) != 0 {
		t.Fatal("a cut entered the journal or the aggregates")
	}
	select {
	case e := <-sub.Events:
		if e.Type != events.SensitiveDataDetected || e.UserHash != envelope.Identity.Hash().Hex() || e.At.Nanosecond()%1000 != 0 || e.ValidateSensitive() != nil {
			t.Fatalf("event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}

func TestSensitiveSummaryCompletion(t *testing.T) {
	service, store, snapshot, envelope := setup(t)
	ctx := context.Background()
	envelope.Sensitive = sensitivedata.Summary{{DataType: sensitivedata.Email, Count: 2, Action: sensitivedata.ModeRedact}}
	provider, _ := snapshot.Provider("test")
	admission, rejection := service.Admit(ctx, snapshot, envelope, "test", provider)
	if rejection != nil {
		t.Fatalf("admit: %+v", rejection)
	}
	admission.Complete(proxy.AttemptResult{RequestID: envelope.RequestID, AttemptIndex: 1, Operation: proxy.OperationGeneration, Detections: envelope.Sensitive})
	if err := service.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := store.Records(ctx, storage.RecordFilter{AppName: "bot", Limit: 10})
	if err != nil || len(page.Records) != 1 || len(page.Records[0].Detections) != 1 || page.Records[0].Detections[0].Count != 2 {
		t.Fatalf("records: %v %+v", err, page.Records)
	}
}

func TestSensitiveFailureNoPartialCounts(t *testing.T) {
	service, store, snapshot, envelope := setup(t)
	ctx := context.Background()
	cut := proxy.SensitiveCut{At: scanAt, Status: http.StatusServiceUnavailable, Code: proxy.CodeSensitiveScanFailed, Class: proxy.ClassInternal}
	if err := service.RecordSensitiveCut(ctx, snapshot, envelope, cut); err != nil {
		t.Fatal(err)
	}
	page, _ := store.Records(ctx, storage.RecordFilter{AppName: "bot", Limit: 10})
	read, _ := store.ReadEvents(ctx, events.Filter{})
	if len(page.Records) != 1 || page.Records[0].Detections != nil || page.Records[0].ErrorCode != "sensitive_data_scan_failed" || len(read.Events) != 0 {
		t.Fatalf("records=%+v events=%+v", page.Records, read.Events)
	}
	// A partial summary on a failed scan is refused by the store.
	cut.Summary = blockSummary()
	envelope.RequestID = "fedcba9876543210fedcba9876543210"
	if err := service.RecordSensitiveCut(ctx, snapshot, envelope, cut); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("partial counts accepted: %v", err)
	}
}

type noPublisher struct {
	storage.AdmissionStore
	storage.CompletionStore
	storage.MaintenanceStore
}

func TestPublicationFailureModes(t *testing.T) {
	service, store, _, envelope := setup(t)
	ctx := context.Background()
	summary := sensitivedata.Summary{{DataType: sensitivedata.CPF, Count: 1, Action: sensitivedata.ModeDetect}}
	for range 2 {
		if err := service.PublishSensitive(ctx, envelope, scanAt, summary); err != nil {
			t.Fatal(err)
		}
	}
	read, _ := store.ReadEvents(ctx, events.Filter{Types: []events.Type{events.SensitiveDataDetected}})
	if len(read.Events) != 1 {
		t.Fatalf("a retried publication duplicated the event: %d", len(read.Events))
	}
	missing := New(noPublisher{store, store, store}, service.catalog, service.logger, service.retention)
	if err := missing.PublishSensitive(ctx, envelope, scanAt, summary); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("store without events: %v", err)
	}
	if err := missing.RecordSensitiveCut(ctx, nil, envelope, proxy.SensitiveCut{}); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("store without cuts: %v", err)
	}
	if err := service.PublishSensitive(ctx, envelope, scanAt, nil); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("empty summary accepted: %v", err)
	}
}
