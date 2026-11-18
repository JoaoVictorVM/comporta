//go:build container

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

const observabilityProject = "comporta-observability-test"

// observabilityStack runs docker compose with the observability profile and
// the fake provider override, with the files the operator prepares.
type observabilityStack struct {
	t   *testing.T
	env []string
}

func (s *observabilityStack) compose(args ...string) string {
	s.t.Helper()
	base := []string{"compose", "-p", observabilityProject, "-f", "compose.yaml", "-f", "examples/observability/compose.fake-provider.yaml", "--profile", "observability"}
	command := exec.Command("docker", append(base, args...)...)
	command.Dir = repoRoot(s.t)
	command.Env = append(os.Environ(), s.env...)
	output, err := command.CombinedOutput()
	if err != nil {
		s.t.Fatalf("docker compose %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func getJSON(t *testing.T, target, user, password string, into any) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		request.SetBasicAuth(user, password)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	if into != nil {
		_ = json.Unmarshal(data, into)
	}
	return response.StatusCode
}

func TestObservabilityProfileDashboardReady(t *testing.T) {
	// The normal stack must not need any file of the profile.
	if out := compose(t, "config", "--services"); strings.TrimSpace(out) != "comporta" {
		t.Fatalf("services without the profile: %q", out)
	}
	dir := t.TempDir()
	tokenFile, passwordFile := filepath.Join(dir, "metrics-token"), filepath.Join(dir, "grafana-password")
	password := strings.TrimPrefix(testkit.Marker(), "cmpcanary")
	if err := os.WriteFile(passwordFile, []byte(password), 0o644); err != nil {
		t.Fatal(err)
	}
	stack := &observabilityStack{t: t, env: []string{
		"COMPORTA_CONFIG=./examples/observability/comporta-fake.yaml", "ANTHROPIC_API_KEY=synthetic-container-key",
		"COMPORTA_METRICS_TOKEN_FILE=" + tokenFile, "GRAFANA_ADMIN_PASSWORD_FILE=" + passwordFile,
	}}
	t.Cleanup(func() { stack.compose("down", "-v", "--remove-orphans") })

	// Only the UIs reach the host, on loopback; OTLP stays in the network.
	var rendered struct {
		Services map[string]struct {
			Ports []struct {
				HostIP string `json:"host_ip"`
				Target int    `json:"target"`
			} `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(stack.compose("config", "--format", "json")), &rendered); err != nil {
		t.Fatal(err)
	}
	for name, service := range rendered.Services {
		for _, port := range service.Ports {
			if port.HostIP != "127.0.0.1" || port.Target == 4318 {
				t.Fatalf("%s publishes %d on %q", name, port.Target, port.HostIP)
			}
		}
	}

	stack.compose("build")
	created := stack.compose("run", "--rm", "--no-deps", "comporta", "keys", "create", "my-app", "--config", "/etc/comporta/comporta.yaml")
	key := regexp.MustCompile(`cmp_[0-9A-Za-z]{43}`).FindString(created)
	reset := stack.compose("run", "--rm", "--no-deps", "comporta", "admin", "reset-token", "--config", "/etc/comporta/comporta.yaml")
	token := regexp.MustCompile(`store it now\): ([0-9a-f]{64})`).FindStringSubmatch(reset)
	if key == "" || token == nil {
		t.Fatalf("provisioning failed:\n%s\n%s", created, reset)
	}
	if err := os.WriteFile(tokenFile, []byte(token[1]), 0o644); err != nil {
		t.Fatal(err)
	}
	stack.compose("up", "-d")
	waitForHealth(t)
	// Grafana migrates its database on the first start, which is slow on some
	// Docker hosts; the one minute of the experience counts from the traffic.
	ready := time.Now().Add(12 * time.Minute)
	for getJSON(t, "http://127.0.0.1:3000/api/health", "", "", nil) != 200 || getJSON(t, "http://127.0.0.1:16686/api/v3/services", "", "", nil) != 200 {
		if time.Now().After(ready) {
			t.Fatalf("grafana or jaeger did not start:\n%s", stack.compose("logs"))
		}
		time.Sleep(time.Second)
	}

	firstTraffic := time.Now()
	sent := 0
	for _, body := range []string{testkit.AnthropicBody(), testkit.AnthropicStreamBody()} {
		request, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1/messages", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", key)
		request.Header.Set("X-Comporta-User", testkit.UserMarker())
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("traffic status %d", response.StatusCode)
		}
		sent++
	}

	// Grafana answers from its provisioned Prometheus datasource within a minute.
	query := "http://127.0.0.1:3000/api/datasources/proxy/uid/comporta-prometheus/api/v1/query?query=" + url.QueryEscape(`sum(comporta_requests_total{app="my-app"})`)
	for {
		var result struct {
			Data struct {
				Result []struct {
					Value []any `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		status := getJSON(t, query, "admin", password, &result)
		if len(result.Data.Result) == 1 && fmt.Sprint(result.Data.Result[0].Value[1]) == fmt.Sprint(sent) {
			break
		}
		if time.Since(firstTraffic) > 60*time.Second {
			var targets any
			getJSON(t, "http://127.0.0.1:9090/api/v1/targets", "", "", &targets)
			t.Fatalf("no data in Grafana one minute after the first request: status %d, result %+v, targets %+v", status, result, targets)
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("dashboard data available %v after the first request", time.Since(firstTraffic).Round(time.Second))
	var dashboard struct {
		Dashboard struct {
			Title  string            `json:"title"`
			Panels []json.RawMessage `json:"panels"`
		} `json:"dashboard"`
	}
	if status := getJSON(t, "http://127.0.0.1:3000/api/dashboards/uid/comporta-overview", "admin", password, &dashboard); status != 200 || dashboard.Dashboard.Title != "Comporta" || len(dashboard.Dashboard.Panels) == 0 {
		t.Fatalf("dashboard not provisioned: %d %+v", status, dashboard.Dashboard.Title)
	}
	if status := getJSON(t, "http://127.0.0.1:3000/api/dashboards/uid/comporta-overview", "admin", "wrong", nil); status != 401 {
		t.Fatalf("grafana accepted a wrong password: %d", status)
	}

	// Jaeger shows one trace per request, split into stages, through its v3 API.
	window := url.Values{
		"query.service_name":   {"comporta"},
		"query.start_time_min": {firstTraffic.Add(-time.Minute).UTC().Format(time.RFC3339Nano)},
		"query.start_time_max": {time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)},
		"query.num_traces":     {"50"},
	}
	byTrace := map[string]map[string]bool{}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		var found struct {
			Result struct {
				ResourceSpans []struct {
					ScopeSpans []struct {
						Spans []struct {
							TraceID string `json:"traceId"`
							Name    string `json:"name"`
						} `json:"spans"`
					} `json:"scopeSpans"`
				} `json:"resourceSpans"`
			} `json:"result"`
		}
		getJSON(t, "http://127.0.0.1:16686/api/v3/traces?"+window.Encode(), "", "", &found)
		byTrace = map[string]map[string]bool{}
		for _, rs := range found.Result.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, span := range ss.Spans {
					if byTrace[span.TraceID] == nil {
						byTrace[span.TraceID] = map[string]bool{}
					}
					byTrace[span.TraceID][span.Name] = true
				}
			}
		}
		if len(byTrace) >= sent || time.Now().After(deadline) {
			break
		}
	}
	if len(byTrace) != sent {
		t.Fatalf("%d traces for %d requests", len(byTrace), sent)
	}
	for _, seen := range byTrace {
		for _, stage := range []string{"comporta.request", "authenticate", "validate", "admit", "deliver", "upstream_attempt", "settle"} {
			if !seen[stage] {
				t.Fatalf("trace without %s: %v", stage, seen)
			}
		}
	}
	logs := stack.compose("logs", "--no-color")
	if strings.Contains(logs, testkit.Marker()) || strings.Contains(logs, testkit.UserMarker()) || strings.Contains(logs, token[1]) {
		t.Fatal("a marker or the admin token reached the logs")
	}
}
