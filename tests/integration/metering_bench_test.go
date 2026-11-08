package integration

import (
	"io"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func BenchmarkHTTPMetering(b *testing.B) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "stream"
		}
		for _, metered := range []bool{false, true} {
			mode := "baseline"
			if metered {
				mode = "durable"
			}
			b.Run(name+"/"+mode, func(b *testing.B) {
				gw := startGatewayOptions(b, fakeprovider.Options{}, "", proxy.Hooks{}, metered)
				key := gw.createKey("bot").Key.Reveal()
				body := testkit.OpenAIBody()
				if stream {
					body = testkit.OpenAIStreamBody()
				}
				elapsed, firstByte := make([]time.Duration, 0, b.N), make([]time.Duration, 0, b.N)
				b.ResetTimer()
				for range b.N {
					request, err := http.NewRequest(http.MethodPost, gw.proxyURL("/v1/chat/completions"), strings.NewReader(body))
					if err != nil {
						b.Fatal(err)
					}
					request.Header.Set("Authorization", "Bearer "+key)
					request.Header.Set("Content-Type", "application/json")
					started := time.Now()
					var first time.Duration
					request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{GotFirstResponseByte: func() { first = time.Since(started) }}))
					response, err := http.DefaultClient.Do(request)
					if err != nil {
						b.Fatal(err)
					}
					_, err = io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if err != nil || response.StatusCode != 200 {
						b.Fatalf("response %d: %v", response.StatusCode, err)
					}
					elapsed = append(elapsed, time.Since(started))
					firstByte = append(firstByte, first)
				}
				b.StopTimer()
				slices.Sort(elapsed)
				slices.Sort(firstByte)
				if len(elapsed) > 0 {
					i := (len(elapsed) - 1) * 95 / 100
					b.ReportMetric(float64(elapsed[i].Nanoseconds())/1e6, "p95-ms")
					b.ReportMetric(float64(firstByte[i].Nanoseconds())/1e6, "p95-ttfb-ms")
				}
			})
		}
	}
}
