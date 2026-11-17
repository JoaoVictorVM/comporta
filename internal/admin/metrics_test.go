package admin

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeMetrics struct {
	enabled bool
	fail    bool
	scrapes int
}

func (f *fakeMetrics) MetricsEnabled() bool { return f.enabled }

func (f *fakeMetrics) WriteMetrics(w io.Writer) error {
	f.scrapes++
	if f.fail {
		return errors.New("collector state with secret detail")
	}
	_, err := io.WriteString(w, "# TYPE comporta_requests_total counter\ncomporta_requests_total 1\n")
	return err
}

func (f *apiFixture) scrape(source MetricsSource, header http.Header, remote string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "http://localhost:8081/metrics", nil)
	request.RequestURI = "/metrics"
	request.RemoteAddr = "192.0.2.10:40000"
	if remote != "" {
		request.RemoteAddr = remote
	}
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	recorder := httptest.NewRecorder()
	f.handler.Metrics(source).ServeHTTP(recorder, request)
	return recorder
}

func TestMetricsRequiresAdminBearer(t *testing.T) {
	f := newAPI(t, nil)
	source := &fakeMetrics{enabled: true}
	ok := f.scrape(source, http.Header{"Authorization": {"Bearer " + f.token}}, "")
	if ok.Code != 200 || ok.Header().Get("Content-Type") != MetricsContentType || ok.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(ok.Body.String(), "comporta_requests_total 1") {
		t.Fatalf("scrape: %d %v %s", ok.Code, ok.Header(), ok.Body.String())
	}
	for name, header := range map[string]http.Header{
		"missing":   {},
		"wrong":     {"Authorization": {"Bearer ca_wrong"}},
		"duplicate": {"Authorization": {"Bearer " + f.token, "Bearer " + f.token}},
		"basic":     {"Authorization": {"Basic " + f.token}},
		"query":     {"X-Admin-Token": {f.token}},
	} {
		recorder := f.scrape(source, header, "198.51.100."+string(rune('1'+len(name)%8))+":1")
		if recorder.Code != 401 || errorCode(t, recorder) != "invalid_admin_token" || strings.Contains(recorder.Body.String(), f.token) {
			t.Fatalf("%s: %d %s", name, recorder.Code, recorder.Body.String())
		}
	}
	if source.scrapes != 1 {
		t.Fatalf("a refused request collected metrics: %d", source.scrapes)
	}
	// Failures share the admin guard: an address that keeps guessing is held off.
	var last *httptest.ResponseRecorder
	for range GuardMaxFailures + 1 {
		last = f.scrape(source, http.Header{"Authorization": {"Bearer guess"}}, "203.0.113.9:1")
	}
	if last.Code != 429 || errorCode(t, last) != "admin_rate_limited" || last.Header().Get("Retry-After") == "" {
		t.Fatalf("guard: %d %s", last.Code, last.Body.String())
	}
	if blocked := f.scrape(source, http.Header{"Authorization": {"Bearer " + f.token}}, "203.0.113.9:1"); blocked.Code != 429 {
		t.Fatalf("cooldown ignored: %d", blocked.Code)
	}
	// A collection failure answers a generic 503 without its detail.
	source.fail = true
	failed := f.scrape(source, http.Header{"Authorization": {"Bearer " + f.token}}, "")
	if failed.Code != 503 || errorCode(t, failed) != "metrics_unavailable" || strings.Contains(failed.Body.String(), "secret") {
		t.Fatalf("collection failure: %d %s", failed.Code, failed.Body.String())
	}
}

func TestMetricsRejectsCookieAndAppKey(t *testing.T) {
	f := newAPI(t, nil)
	login := f.do(call{method: http.MethodPost, target: Prefix + "session", body: `{"token":"` + f.token + `"}`, header: jsonHeader("Origin", testOrigin)})
	if login.Code != http.StatusCreated {
		t.Fatalf("login %d %s", login.Code, login.Body.String())
	}
	var cookie string
	for _, c := range login.Result().Cookies() {
		if c.Name == SessionCookie {
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie")
	}
	source := &fakeMetrics{enabled: true}
	for name, header := range map[string]http.Header{
		"session cookie": {"Cookie": {cookie}},
		"app key bearer": {"Authorization": {"Bearer ck_live_00000000000000000000000000000000"}},
		"app key header": {"X-Api-Key": {f.token}},
	} {
		if recorder := f.scrape(source, header, ""); recorder.Code != 401 {
			t.Fatalf("%s: %d", name, recorder.Code)
		}
	}
	if source.scrapes != 0 {
		t.Fatal("metrics collected without the admin token")
	}
}

func TestMetricsDisabled(t *testing.T) {
	f := newAPI(t, nil)
	source := &fakeMetrics{}
	recorder := f.scrape(source, http.Header{"Authorization": {"Bearer " + f.token}}, "")
	if recorder.Code != 404 || errorCode(t, recorder) != "not_found" || source.scrapes != 0 {
		t.Fatalf("disabled: %d %s", recorder.Code, recorder.Body.String())
	}
	// The answer does not depend on the credential.
	if anonymous := f.scrape(source, nil, ""); anonymous.Code != 404 || anonymous.Body.String() != recorder.Body.String() {
		t.Fatalf("anonymous disabled: %d %s", anonymous.Code, anonymous.Body.String())
	}
	// A body is never accepted.
	source.enabled = true
	request := httptest.NewRequest(http.MethodGet, "http://localhost:8081/metrics", strings.NewReader("x"))
	request.RequestURI = "/metrics"
	request.Header.Set("Authorization", "Bearer "+f.token)
	with := httptest.NewRecorder()
	f.handler.Metrics(source).ServeHTTP(with, request)
	if with.Code != 400 || source.scrapes != 0 {
		t.Fatalf("body: %d", with.Code)
	}
}
