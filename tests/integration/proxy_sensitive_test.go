package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/loopguard"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

var placeholderPattern = regexp.MustCompile(`\[[A-Z]+_\d+_[0-9a-f]{24}\]`)

// withDetection writes a detection block for the app bot.
func withDetection(lines ...string) func(string) string {
	return func(text string) string {
		block := "    detection:\n"
		for _, line := range lines {
			block += "      " + line + "\n"
		}
		anchor := "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n"
		return strings.Replace(text, anchor, anchor+block, 1)
	}
}

// echoPlaceholders appends the placeholders the provider received to the text of
// its JSON answer, the way a model repeats what it was given. A prompt asking for
// a duplicate key gets a response the gateway cannot restore safely.
func echoPlaceholders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)
		answer := recorder.Body.String()
		if strings.Contains(recorder.Header().Get("Content-Type"), "application/json") {
			tokens := strings.Join(placeholderPattern.FindAllString(string(body), -1), " ")
			answer = strings.Replace(answer, `"Olá"`, `"Olá `+tokens+`"`, 1)
			if strings.Contains(string(body), "duplicate-key") {
				answer = strings.TrimSuffix(answer, "}") + `,"usage":{"input_tokens":1,"output_tokens":1}}`
			}
		}
		for name, values := range recorder.Header() {
			if name != "Content-Length" {
				w.Header()[name] = values
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write([]byte(answer))
	})
}

type sensitiveRig struct {
	t        *testing.T
	gw       *gateway
	key      string
	upstream *atomic.Int64
}

func startSensitiveGateway(t *testing.T, fake fakeprovider.Options, options gatewayOptions) *sensitiveRig {
	t.Helper()
	upstream := &atomic.Int64{}
	previous := fake.OnRequest
	fake.OnRequest = func(request fakeprovider.Request) {
		upstream.Add(1)
		if previous != nil {
			previous(request)
		}
	}
	options.metered, options.sensitive = true, true
	gw := startConfiguredGateway(t, fake, options)
	return &sensitiveRig{t: t, gw: gw, key: gw.createKey("bot").Key.Reveal(), upstream: upstream}
}

func messagesBody(text string) string {
	encoded, _ := json.Marshal(testkit.Prompt(text))
	return `{"model":"fake-claude","max_tokens":16,"metadata":{"user_id":"sensitive-user"},"messages":[{"role":"user","content":` + string(encoded) + `}]}`
}

func chatBody(text string) string {
	encoded, _ := json.Marshal(testkit.Prompt(text))
	return `{"model":"fake-gpt","user":"sensitive-user","messages":[{"role":"user","content":` + string(encoded) + `}]}`
}

func syntheticKey() string { return "sk-" + "ant-" + "api03-" + strings.Repeat("Zq7_", 10) }

// sensitiveSamples has one synthetic value per category.
var sensitiveSamples = []struct {
	category string
	value    string
}{
	{"cpf", testkit.DocumentID()},
	{"cnpj", "11.222.333/0001-81"},
	{"card", "4111 1111 1111 1111"},
	{"email", "cliente@example.test"},
	{"phone", "(11) 98765-4321"},
	{"pix", "123e4567-e89b-42d3-a456-426614174000"},
	{"secret", syntheticKey()},
}

// records waits until at least want records of bot are materialized.
func (r *sensitiveRig) records(want int) []storage.Record {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := r.gw.accounting.Flush(context.Background()); err != nil {
			r.t.Fatal(err)
		}
		page, err := r.gw.usage.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 1000})
		if err != nil {
			r.t.Fatal(err)
		}
		if len(page.Records) >= want || time.Now().After(deadline) {
			return page.Records
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *sensitiveRig) sensitiveEvents() []events.Event {
	r.t.Helper()
	page, err := r.gw.usage.ReadEvents(context.Background(), events.Filter{Types: []events.Type{events.SensitiveDataDetected}})
	if err != nil {
		r.t.Fatal(err)
	}
	return page.Events
}

// TestSensitiveModeMatrixAndReload covers every category in every mode, and the
// rule that only requests capturing a new snapshot see a new policy.
func TestSensitiveModeMatrixAndReload(t *testing.T) {
	r := startSensitiveGateway(t, fakeprovider.Options{}, gatewayOptions{edit: withDetection("email: off")})
	for _, sample := range sensitiveSamples {
		for _, mode := range []string{"detect", "redact", "block"} {
			r.gw.reload(func(text string) string {
				return regexp.MustCompile(`    detection:\n(      .*\n)+`).ReplaceAllString(text, "    detection:\n      "+sample.category+": "+mode+"\n")
			})
			before := r.upstream.Load()
			response, answer := r.gw.call("/v1/messages", r.key, messagesBody("dado "+sample.value+" fim"))
			switch mode {
			case "block":
				if response.StatusCode != 400 || loopCode(t, answer) != "sensitive_data_blocked" || r.upstream.Load() != before {
					t.Fatalf("%s block: %d %s", sample.category, response.StatusCode, answer)
				}
				continue
			default:
				if response.StatusCode != 200 {
					t.Fatalf("%s %s: %d %s", sample.category, mode, response.StatusCode, answer)
				}
			}
			sent := string(r.gw.lastRequest().Body)
			if mode == "detect" && !strings.Contains(sent, sample.value) {
				t.Fatalf("%s detect changed the body", sample.category)
			}
			if mode == "redact" && (strings.Contains(sent, sample.value) || !strings.Contains(sent, "["+strings.ToUpper(sample.category)+"_1_")) {
				t.Fatalf("%s redact: %s", sample.category, sent)
			}
		}
	}
	// An invalid reload keeps the snapshot in force, and its policy with it.
	data, _ := os.ReadFile(r.gw.path)
	broken := strings.Replace(string(data), "secret: block", "secret: maybe", 1)
	if err := os.WriteFile(r.gw.path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := r.gw.config.Reload(context.Background()); status.Result != config.ResultRejected {
		t.Fatalf("invalid reload: %+v", status)
	}
	if response, answer := r.gw.call("/v1/messages", r.key, messagesBody(syntheticKey())); response.StatusCode != 400 {
		t.Fatalf("the rejected reload changed the policy: %d %s", response.StatusCode, answer)
	}
}

// TestSensitiveFormatsKeepPatches checks both formats and count_tokens, with the
// identity pseudonym and the output limit still in place.
func TestSensitiveFormatsKeepPatches(t *testing.T) {
	r := startSensitiveGateway(t, fakeprovider.Options{}, gatewayOptions{edit: withDetection("email: redact")})
	for _, call := range []struct{ path, body string }{
		{"/v1/messages", messagesBody("para cliente@example.test")},
		{"/v1/chat/completions", chatBody("para cliente@example.test")},
		{"/v1/messages/count_tokens", `{"model":"fake-claude","messages":[{"role":"user","content":"para cliente@example.test"}]}`},
	} {
		response, answer := r.gw.call(call.path, r.key, call.body)
		if response.StatusCode != 200 {
			t.Fatalf("%s: %d %s", call.path, response.StatusCode, answer)
		}
		sent := string(r.gw.lastRequest().Body)
		if strings.Contains(sent, "cliente@") || !placeholderPattern.MatchString(sent) || strings.Contains(sent, "sensitive-user") {
			t.Fatalf("%s sent %s", call.path, sent)
		}
		if call.path == "/v1/messages" && (!strings.Contains(sent, `"max_tokens":16`) || !regexp.MustCompile(`"user_id":"[0-9a-f]{32}"`).MatchString(sent)) {
			t.Fatalf("patches lost: %s", sent)
		}
	}
}

// TestSensitiveEventBeforeFanOut checks the event is durable before subscribers
// hear of it, once per request, and the record keeps the same summary.
func TestSensitiveEventBeforeFanOut(t *testing.T) {
	r := startSensitiveGateway(t, fakeprovider.Options{}, gatewayOptions{edit: withDetection("email: redact", "cpf: detect")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := r.gw.usage.Bus.Subscribe(ctx)
	response, answer := r.gw.call("/v1/messages", r.key, messagesBody("cliente@example.test e "+testkit.DocumentID()))
	if response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-sub.Events:
			if e.Type != events.SensitiveDataDetected {
				continue
			}
			stored := r.sensitiveEvents()
			if len(stored) != 1 || stored[0].ID != e.ID || stored[0].ValidateSensitive() != nil {
				t.Fatalf("notified before commit or duplicated: %+v", stored)
			}
		case <-deadline:
			t.Fatal("no sensitive data event")
		}
		break
	}
	records := r.records(1)
	if len(records) != 1 || len(records[0].Detections) != 2 || records[0].Detections[0].Action != sensitivedata.ModeDetect {
		t.Fatalf("records = %+v", records)
	}
	encoded, _ := json.Marshal(struct {
		R []storage.Record
		E []events.Event
	}{records, r.sensitiveEvents()})
	if strings.Contains(string(encoded), "cliente@") || strings.Contains(string(encoded), testkit.DocumentID()) || placeholderPattern.Match(encoded) {
		t.Fatalf("a value or a placeholder was persisted: %s", encoded)
	}
}

// TestSensitiveBudgetAndLoop checks a refusal reserves nothing, the estimate is
// still conservative after redaction, and placeholders with new nonces do not hide
// a loop.
func TestSensitiveBudgetAndLoop(t *testing.T) {
	clock := loopguard.NewFakeClock(time.Now())
	r := startSensitiveGateway(t, fakeprovider.Options{}, gatewayOptions{loop: true, loopClock: clock, edit: withDetection("email: redact", "secret: block")})
	response, answer := r.gw.call("/v1/messages", r.key, messagesBody(syntheticKey()))
	if response.StatusCode != 400 || r.upstream.Load() != 0 {
		t.Fatalf("block: %d %s", response.StatusCode, answer)
	}
	records := r.records(1)
	if len(records) != 1 || records[0].ReservedCost != 0 || records[0].Cost != 0 || records[0].Outcome != "blocked" || records[0].ErrorCode != "sensitive_data_blocked" {
		t.Fatalf("block record = %+v", records)
	}
	daily, _ := r.gw.usage.Daily(context.Background(), storage.Filter{AppName: "bot", Since: "2000-01-01", Until: "2100-01-01"})
	if len(daily) != 0 {
		t.Fatalf("a refusal reached the aggregates: %+v", daily)
	}

	body := messagesBody("loop para a@b.io")
	for i := 1; i <= 10; i++ {
		clock.Advance(time.Second)
		if response, answer := r.gw.call("/v1/messages", r.key, body); response.StatusCode != 200 {
			t.Fatalf("similar request %d: %d %s", i, response.StatusCode, answer)
		}
	}
	clock.Advance(time.Second)
	if response, answer := r.gw.call("/v1/messages", r.key, body); response.StatusCode != 429 || loopCode(t, answer) != "loop_detected" {
		t.Fatalf("eleventh similar request with fresh nonces: %d %s", response.StatusCode, answer)
	}
	for _, record := range r.records(11) {
		if record.AttemptIndex != 1 {
			continue
		}
		original, _ := pricing.EstimateInput(int64(len(body)), int64(len(body)), 0)
		if record.Quote.EstimatedInput < original {
			t.Fatalf("estimate %d below the original body's %d", record.Quote.EstimatedInput, original)
		}
	}
}

// TestSensitiveResponses covers restoration of JSON answers in both formats, the
// stream warning, and a failed restoration that still settles real usage.
func TestSensitiveResponses(t *testing.T) {
	r := startSensitiveGateway(t, fakeprovider.Options{}, gatewayOptions{upstream: echoPlaceholders, edit: withDetection("email: redact", "rehydrate: true")})
	for _, call := range []struct{ path, body string }{
		{"/v1/messages", messagesBody("Responda para cliente@example.test")},
		{"/v1/chat/completions", chatBody("Responda para cliente@example.test")},
	} {
		response, answer := r.gw.call(call.path, r.key, call.body)
		if response.StatusCode != 200 || !strings.Contains(answer, "Olá cliente@example.test") || placeholderPattern.MatchString(answer) {
			t.Fatalf("%s: %d %s", call.path, response.StatusCode, answer)
		}
		sent := string(r.gw.lastRequest().Body)
		if strings.Contains(sent, "cliente@") || !placeholderPattern.MatchString(sent) {
			t.Fatalf("the provider received the value: %s", sent)
		}
	}

	stream := strings.Replace(testkit.AnthropicStreamBody(), `"messages":[`, `"metadata":{"user_id":"u"},"messages":[{"role":"user","content":"de cliente@example.test"},`, 1)
	response, answer := r.gw.call("/v1/messages", r.key, stream)
	if response.StatusCode != 200 || response.Header.Get("X-Comporta-Rehydration") != "unsupported-for-stream" || !strings.Contains(answer, "message_stop") {
		t.Fatalf("stream: %d %q %s", response.StatusCode, response.Header.Get("X-Comporta-Rehydration"), answer)
	}

	response, answer = r.gw.call("/v1/messages", r.key, messagesBody("duplicate-key cliente@example.test"))
	if response.StatusCode != 502 || loopCode(t, answer) != "response_rehydration_failed" {
		t.Fatalf("failed restoration: %d %s", response.StatusCode, answer)
	}
	var failed *storage.Record
	for _, record := range r.records(4) {
		if record.ErrorCode == "response_rehydration_failed" {
			failed = &record
		}
	}
	if failed == nil || failed.Input == nil || *failed.Input != 1 || failed.Cost == 0 {
		t.Fatalf("the generated usage was not settled: %+v", failed)
	}
}
