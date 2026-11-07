package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func startFakeCommand(t *testing.T, args ...string) (*lockedBuffer, func() int) {
	t.Helper()
	stdout := &lockedBuffer{}
	var stderr lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan int, 1)
	go func() { exit <- Execute(ctx, append([]string{"fake-provider"}, args...), stdout, &stderr) }()
	stopped := false
	code := 0
	stop := func() int {
		if !stopped {
			stopped = true
			cancel()
			select {
			case code = <-exit:
			case <-time.After(10 * time.Second):
				t.Error("the fake did not stop")
			}
		}
		return code
	}
	t.Cleanup(func() { stop() })
	return stdout, stop
}

func TestFakeFlagsAndDefaultBind(t *testing.T) {
	command := newFakeProviderCommand(streams{stdout: io.Discard, stderr: io.Discard})
	for flag, want := range map[string]string{"port": "9090", "bind": "127.0.0.1", "scenario": "success"} {
		if got := command.Flags().Lookup(flag).DefValue; got != want {
			t.Errorf("%s default: %s", flag, got)
		}
	}
	port := freePort(t)
	stdout, stop := startFakeCommand(t, "--port", fmt.Sprint(port), "--scenario", "overload")
	waitForLog(t, stdout, "fake provider listening on 127.0.0.1:")
	if !strings.Contains(stdout.String(), "scenario overload") {
		t.Fatalf("stdout %q", stdout.String())
	}
	response, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/messages", port), "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 529 {
		t.Fatalf("status %d", response.StatusCode)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestFakeCommandStopsOnContext(t *testing.T) {
	port := freePort(t)
	stdout, stop := startFakeCommand(t, "--port", fmt.Sprint(port), "--scenario", "header-timeout")
	waitForLog(t, stdout, "listening")
	// A request held by the scenario must not keep the command from stopping.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if response, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/messages", port), "application/json", strings.NewReader(`{"model":"m"}`)); err == nil {
			_ = response.Body.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	<-done
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port still bound: %v", err)
	}
	_ = listener.Close()
}

func TestInvalidScenarioStartsNoListener(t *testing.T) {
	port := freePort(t)
	cases := [][]string{
		{"fake-provider", "--scenario", "nope", "--port", fmt.Sprint(port)},
		{"fake-provider", "--port", "70000"},
		{"fake-provider", "--port", "0"},
		{"fake-provider", "--bind", "not-an-ip", "--port", fmt.Sprint(port)},
	}
	for _, args := range cases {
		code, stdout, _ := run(t, args...)
		if code != 1 || strings.Contains(stdout, "listening") {
			t.Errorf("%v: exit %d, stdout %q", args, code, stdout)
		}
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("a listener was opened: %v", err)
	}
	_ = listener.Close()
}

func TestFakeBindFailureExitsOne(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	code, _, stderr := run(t, "fake-provider", "--port", fmt.Sprint(taken.Addr().(*net.TCPAddr).Port))
	if code != 1 || !strings.Contains(stderr, "could not listen on 127.0.0.1:") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}
