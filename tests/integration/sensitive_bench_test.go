package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// The latency scenario compares the same gateway with detection off and in detect
// mode for every category, on prompts of the reference size. It is long, so it runs
// only when asked for:
//
//	COMPORTA_SENSITIVE_BENCH=1 go test ./tests/integration -run '^TestSensitiveLatencyScenario$' -count=1 -timeout 60m
//
// COMPORTA_SENSITIVE_BENCH_REQUESTS and COMPORTA_SENSITIVE_BENCH_REPETITIONS
// override 10000 and 5; COMPORTA_SENSITIVE_BENCH_CLEAN removes the values to find,
// isolating the scan from the event write; COMPORTA_SENSITIVE_BENCH_OUT names a JSON
// file for the result. The provider's own time is the same in both modes and cancels out in the
// difference of percentiles.

const benchBodyBytes = 400_000

type latencyStats struct {
	P50US           int64   `json:"p50_us"`
	P95US           int64   `json:"p95_us"`
	P99US           int64   `json:"p99_us"`
	AllocBytesPerOp float64 `json:"alloc_bytes_per_request"`
	AllocsPerOp     float64 `json:"allocs_per_request"`
}

type latencyRepetition struct {
	Off         latencyStats `json:"off"`
	Detect      latencyStats `json:"detect"`
	DeltaP95US  int64        `json:"delta_p95_us"`
	DeltaP50US  int64        `json:"delta_p50_us"`
	DeltaP99US  int64        `json:"delta_p99_us"`
	OffFirstRun bool         `json:"off_ran_first"`
}

type latencyReport struct {
	DetectorRevision string              `json:"detector_revision"`
	GoVersion        string              `json:"go_version"`
	OS               string              `json:"os"`
	Arch             string              `json:"arch"`
	CPUs             int                 `json:"cpus"`
	Concurrency      int                 `json:"concurrency"`
	BodyBytes        int                 `json:"body_bytes"`
	Requests         int                 `json:"requests_per_mode"`
	Warmup           int                 `json:"warmup_requests"`
	Repetitions      []latencyRepetition `json:"repetitions"`
	WorstDeltaP95US  int64               `json:"worst_delta_p95_us"`
}

func envInt(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 0 {
		return value
	}
	return fallback
}

// benchBody builds a valid request of exactly benchBodyBytes with a few values to
// find, so detect mode also publishes its event.
func benchBody(t *testing.T) string {
	t.Helper()
	paragraph := "O relatório trimestral mostra crescimento estável nas regiões sul e sudeste, com margem preservada. "
	values := "Contato ana@example.org, documento " + testkit.DocumentID() + ". "
	if os.Getenv("COMPORTA_SENSITIVE_BENCH_CLEAN") != "" {
		values = "Sem dados a detectar. "
	}
	prefix := `{"model":"fake-claude","max_tokens":16,"messages":[{"role":"user","content":"` + testkit.Prompt(values)
	suffix := `"}]}`
	fill := benchBodyBytes - len(prefix) - len(suffix)
	body := prefix + strings.Repeat(paragraph, fill/len(paragraph)+1)[:fill] + suffix
	if len(body) != benchBodyBytes || !json.Valid([]byte(body)) {
		t.Fatalf("bench body has %d bytes", len(body))
	}
	return body
}

func percentile(sorted []time.Duration, p float64) int64 {
	index := int(float64(len(sorted)-1) * p)
	return sorted[index].Microseconds()
}

func measure(t *testing.T, r *sensitiveRig, body string, n int) latencyStats {
	t.Helper()
	latencies := make([]time.Duration, 0, n)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range n {
		start := time.Now()
		response, answer := r.gw.call("/v1/messages", r.key, body)
		latencies = append(latencies, time.Since(start))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", response.StatusCode, answer)
		}
	}
	runtime.ReadMemStats(&after)
	slices.Sort(latencies)
	return latencyStats{
		P50US: percentile(latencies, 0.50), P95US: percentile(latencies, 0.95), P99US: percentile(latencies, 0.99),
		AllocBytesPerOp: float64(after.TotalAlloc-before.TotalAlloc) / float64(n),
		AllocsPerOp:     float64(after.Mallocs-before.Mallocs) / float64(n),
	}
}

func TestSensitiveLatencyScenario(t *testing.T) {
	if os.Getenv("COMPORTA_SENSITIVE_BENCH") == "" {
		t.Skip("set COMPORTA_SENSITIVE_BENCH=1 to run the latency scenario")
	}
	requests := envInt("COMPORTA_SENSITIVE_BENCH_REQUESTS", 10000)
	repetitions := envInt("COMPORTA_SENSITIVE_BENCH_REPETITIONS", 5)
	warmup := min(requests/10+1, 500)
	all := []string{}
	for _, c := range sensitivedata.Categories() {
		all = append(all, c.String()+": detect")
	}
	off := func(text string) string { return strings.ReplaceAll(text, ": detect\n", ": off\n") }
	on := func(text string) string { return strings.ReplaceAll(text, ": off\n", ": detect\n") }
	r := startSensitiveGateway(t, fakeprovider.Options{}, gatewayOptions{edit: func(text string) string { return off(withDetection(all...)(text)) }})
	body := benchBody(t)
	report := latencyReport{
		DetectorRevision: sensitivedata.DetectorRevision, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), Concurrency: 1, BodyBytes: benchBodyBytes, Requests: requests, Warmup: warmup,
	}
	detecting := false
	for repetition := range repetitions {
		var result latencyRepetition
		// Alternating which mode goes first keeps a warming cache or a slow
		// disk from always favoring the same side.
		order := []bool{false, true}
		if repetition%2 == 1 {
			order = []bool{true, false}
		}
		result.OffFirstRun = !order[0]
		for _, detect := range order {
			if detect != detecting {
				if detect {
					r.gw.reload(on)
				} else {
					r.gw.reload(off)
				}
				detecting = detect
			}
			measure(t, r, body, warmup)
			stats := measure(t, r, body, requests)
			if detect {
				result.Detect = stats
			} else {
				result.Off = stats
			}
		}
		result.DeltaP50US = result.Detect.P50US - result.Off.P50US
		result.DeltaP95US = result.Detect.P95US - result.Off.P95US
		result.DeltaP99US = result.Detect.P99US - result.Off.P99US
		report.Repetitions = append(report.Repetitions, result)
		report.WorstDeltaP95US = max(report.WorstDeltaP95US, result.DeltaP95US)
		t.Logf("repetition %d: off p95 %dus, detect p95 %dus, delta %dus", repetition+1, result.Off.P95US, result.Detect.P95US, result.DeltaP95US)
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("%s", data)
	if out := os.Getenv("COMPORTA_SENSITIVE_BENCH_OUT"); out != "" {
		if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
