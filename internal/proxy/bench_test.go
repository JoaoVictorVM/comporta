package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func benchmarkBody(messages int) []byte {
	var body strings.Builder
	body.WriteString(`{"model":"fake-gpt","stream":true,"future":{"a":[1,2,3]},"messages":[`)
	for i := range messages {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"role":"user","content":"message number %d with some synthetic text"}`, i)
	}
	body.WriteString(`]}`)
	return []byte(body.String())
}

func BenchmarkParseAndPatch(b *testing.B) {
	for _, messages := range []int{1, 100, 10_000} {
		data := benchmarkBody(messages)
		b.Run(fmt.Sprintf("messages=%d", messages), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				document, err := ParseDocument(data)
				if err != nil {
					b.Fatal(err)
				}
				patch, err := document.SetMember(document.Root(), "stream_options", []byte(`{"include_usage":true}`))
				if err != nil {
					b.Fatal(err)
				}
				_ = document.Apply([]Patch{patch})
			}
		})
	}
}

func BenchmarkSSEFraming(b *testing.B) {
	chunk := []byte(strings.Repeat(deltaChunk, 64))
	b.SetBytes(int64(len(chunk)))
	state := &streamState{}
	for b.Loop() {
		parser := newSSEParser()
		parser.Feed(chunk)
		for {
			event, ok, err := parser.Next()
			if err != nil {
				b.Fatal(err)
			}
			if !ok {
				break
			}
			_, _ = state.inspect(event, "p")
		}
	}
}

// BenchmarkRequestThroughHandler includes a new client connection per request, which
// is the cost of the one-request-per-connection decision.
func BenchmarkRequestThroughHandler(b *testing.B) {
	upstream := startBenchUpstream(b)
	fx := newBenchFixture(b, upstream)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for b.Loop() {
		request, err := http.NewRequest(http.MethodPost, fx.server.URL+"/v1/chat/completions", strings.NewReader(openAIBody))
		if err != nil {
			b.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+fx.key)
		response, err := client.Do(request)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			b.Fatalf("status %d", response.StatusCode)
		}
	}
}
